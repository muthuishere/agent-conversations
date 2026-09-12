// Package apl runs HTTP through `apl`, the user's identity broker, instead of
// through net/http.
//
// The point is what this process NEVER touches. A bearer token for a real
// Microsoft tenant is a credential that can read every message the user can
// read; the moment it enters this program it is one debug print, one error
// string, one `ps` line and one shell-history entry away from being leaked.
// `apl` already holds the OAuth grant and refreshes it, so the cheapest way to
// be safe is to never be given the secret at all: we hand apl a METHOD, a URL,
// some headers and a body, and it hands back a response. There is no token in
// a flag, in an env var, in this process's memory, or in this package's logs.
//
// The shape is deliberately the one the teams channel already expects — a
// Doer with `Do(*http.Request) (*http.Response, error)` — so the entire Graph
// adapter above it (paging, delta cursors, threading, retry, path encoding) is
// unchanged and untested-against twice. Swapping the transport must not be a
// rewrite of the thing being transported.
package apl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultBinary is the broker on PATH.
const DefaultBinary = "apl"

// DefaultTimeout bounds one apl invocation. apl has its own --timeout; this is
// the outer bound on the whole child process, because a broker that hangs
// waiting on an interactive login would otherwise hang the listener forever.
const DefaultTimeout = 60 * time.Second

// Transport is a Doer backed by `apl call`.
//
// The zero value is not usable: a Transport with no handle would silently fall
// through to whatever apl considers a default, and "which identity did that
// request go out as" is not a question to answer by guessing.
type Transport struct {
	// Handle is an apl identity, e.g. "ms:<label>" as listed by `apl accounts`.
	// It is a LABEL, not a credential — safe in a flag and in a log line.
	Handle string

	// Binary is the apl executable. Empty means DefaultBinary from PATH.
	Binary string

	// Scopes are passed as repeated --scope flags. apl refuses the call when
	// the stored grant is missing one, BEFORE any request leaves the machine,
	// and tells the user the exact `apl login` that fixes it. Declaring the
	// scopes you need turns a 403 from a remote API into a local, actionable
	// error.
	Scopes []string

	// Timeout bounds one invocation. Zero means DefaultTimeout.
	Timeout time.Duration

	// Debug, when non-nil, receives one line per request: method, URL and
	// status. It NEVER receives the response body. The body is a real user's
	// real messages, and a transport that logs it at debug level has quietly
	// turned every operator's terminal scrollback into a copy of the tenant.
	Debug func(string)

	// run is injectable so the unit tests can drive a fake without a PATH
	// dance. Nil means exec.
	run func(ctx context.Context, bin string, args []string, stdin io.Reader) (stdout, stderr []byte, code int, err error)
}

// New builds a Transport for one handle.
func New(handle string) (*Transport, error) {
	if strings.TrimSpace(handle) == "" {
		return nil, errors.New("apl transport needs a handle (see `apl accounts`)")
	}
	return &Transport{Handle: handle}, nil
}

// AuthError is what apl's own refusal becomes: a typed, loud error carrying
// apl's verbatim remediation line.
//
// This is the error that matters most, because it is the one the user can
// actually fix, and the fix is a single command apl already computed. Summarising
// it into "authentication failed" throws that away and leaves the user to
// reverse-engineer which scope, on which handle, from which provider. So the
// `Run: apl login …` line is carried through untouched and printed untouched.
type AuthError struct {
	Handle string
	// Remediation is apl's own line, e.g.
	// "Run: apl login ms:<label> --force --scope ChannelMessage.Read.All".
	Remediation string
	// Detail is apl's full stderr line.
	Detail string
}

func (e *AuthError) Error() string {
	if e.Remediation != "" {
		return fmt.Sprintf("apl refused the call for handle %q: %s\n%s",
			e.Handle, e.Detail, e.Remediation)
	}
	return fmt.Sprintf("apl refused the call for handle %q: %s", e.Handle, e.Detail)
}

// missingScopeRE matches apl's measured wording:
//
//	apl call: missing scope(s): Foo.Bar. Run: apl login ms:x --force --scope Foo.Bar (auth error)
var (
	missingScopeRE = regexp.MustCompile(`missing scope\(s\)`)
	runHintRE      = regexp.MustCompile(`Run: apl [^\n]*`)
	httpStatusRE   = regexp.MustCompile(`HTTP (\d{3})`)
)

// Do executes one request through apl and returns an *http.Response.
//
// What apl gives us and what it does not, measured rather than assumed:
//   - the response BODY goes to stdout, verbatim;
//   - a non-2xx becomes a non-zero exit plus `apl call: HTTP <code>` on stderr,
//     with the body still on stdout;
//   - a 2xx prints nothing on stderr at all, so "no status line, exit 0" is
//     the only signal for success and is treated as 200;
//   - response HEADERS are not surfaced. That is a real loss and it is named
//     here rather than papered over: `Retry-After` does not survive, so the
//     Graph adapter's throttle backoff falls back to its exponential ceiling
//     instead of honouring the server's own number.
func (t *Transport) Do(req *http.Request) (*http.Response, error) {
	if strings.TrimSpace(t.Handle) == "" {
		return nil, errors.New("apl transport has no handle — refusing to send a request as an unknown identity")
	}

	args := []string{"call", t.Handle, req.Method, req.URL.String()}
	for _, s := range t.Scopes {
		args = append(args, "--scope", s)
	}
	var stdin io.Reader
	if req.Body != nil {
		body, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading request body: %w", err)
		}
		if len(body) > 0 {
			// stdin, never an argv string: a request body can contain anything
			// a person typed, and argv is visible in `ps` to every user on the
			// box.
			stdin = bytes.NewReader(body)
			args = append(args, "--body-file", "-")
		}
	}
	for k, vs := range req.Header {
		if strings.EqualFold(k, "Authorization") {
			// apl owns authentication. A caller-supplied Authorization header
			// would either be ignored or fight the broker's own, and either way
			// it means a credential reached this process — which is the single
			// thing this transport exists to prevent.
			continue
		}
		for _, v := range vs {
			args = append(args, "-H", k+": "+v)
		}
	}

	timeout := t.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	args = append(args, "--timeout", timeout.String())

	ctx := req.Context()
	ctx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
	defer cancel()

	bin := t.Binary
	if bin == "" {
		bin = DefaultBinary
	}
	runner := t.run
	if runner == nil {
		runner = execRun
	}

	stdout, stderr, code, err := runner(ctx, bin, args, stdin)
	errText := string(stderr)

	if missingScopeRE.MatchString(errText) {
		return nil, &AuthError{
			Handle:      t.Handle,
			Remediation: strings.TrimSpace(runHintRE.FindString(errText)),
			Detail:      firstLine(errText),
		}
	}
	if err != nil && code < 0 {
		// The broker itself could not be run: not on PATH, killed, timed out.
		return nil, fmt.Errorf("running %s: %w", bin, err)
	}

	status := http.StatusOK
	if m := httpStatusRE.FindStringSubmatch(errText); m != nil {
		status, _ = strconv.Atoi(m[1])
	} else if code != 0 {
		// A failure apl did not classify as an HTTP status. Reporting it as a
		// 2xx would let the Graph adapter decode an error page as a message
		// list; reporting it as a 5xx makes the retry policy treat it as the
		// transient failure it most likely is.
		return nil, fmt.Errorf("apl call failed (exit %d): %s", code, firstLine(errText))
	}

	if t.Debug != nil {
		// Method, URL and status ONLY. Never the body.
		t.Debug(fmt.Sprintf("apl %s %s %s -> %d", t.Handle, req.Method, req.URL.Redacted(), status))
	}

	resp := &http.Response{
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{},
		Body:          io.NopCloser(bytes.NewReader(stdout)),
		ContentLength: int64(len(stdout)),
		Request:       req,
	}
	if len(stdout) > 0 {
		resp.Header.Set("Content-Type", "application/json")
	}
	return resp, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if s == "" {
		return "no output"
	}
	return s
}

// execRun is the real runner. Exit status is returned rather than raised: a
// non-zero exit from apl is usually an HTTP status, which is information, not
// a failure of the call.
func execRun(ctx context.Context, bin string, args []string, stdin io.Reader) ([]byte, []byte, int, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out.Bytes(), errb.Bytes(), ee.ExitCode(), nil
		}
		return out.Bytes(), errb.Bytes(), -1, err
	}
	return out.Bytes(), errb.Bytes(), 0, nil
}

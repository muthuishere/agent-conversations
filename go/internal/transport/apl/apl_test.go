package apl

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAPL writes a shell script named `apl` into a fresh directory and puts
// that directory FIRST on PATH for the duration of the test.
//
// This is the whole reason `go test ./...` stays offline. The alternative —
// injecting a function and never running a child process — would test the
// argument builder and leave the actual exec path, the exit-code handling and
// the stdout/stderr split unexercised, which is exactly where a transport like
// this breaks. The script records its argv and stdin so the test can assert
// what was sent, without a network and without a real identity.
func fakeAPL(t *testing.T, body string) (dir string) {
	t.Helper()
	dir = t.TempDir()
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"" + dir + "/argv\"\n" +
		"cat > \"" + dir + "/stdin\"\n" +
		body
	path := filepath.Join(dir, "apl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func argvOf(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "argv"))
	if err != nil {
		t.Fatalf("fake apl was never invoked: %v", err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func get(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// A 2xx from apl prints the body on stdout and NOTHING on stderr, so silence
// plus exit 0 has to read as 200. Anything else and every successful Graph call
// looks like a failure.
func TestSuccessfulCallBecomesA200WithTheBody(t *testing.T) {
	fakeAPL(t, `printf '{"id":"alice"}'`)
	tr, err := New("ms:test")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tr.Do(get(t, "https://graph.microsoft.com/v1.0/me"))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != `{"id":"alice"}` {
		t.Fatalf("body = %q", b)
	}
}

// apl's exit code is not the answer; its `HTTP <code>` line is. A 404 must
// arrive as a 404 so the Graph adapter can say "not found" instead of trying to
// decode an error envelope as a message list.
func TestHTTPStatusIsReadFromStderr(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		want         int
	}{
		{"not found", `printf '{"error":{"code":"NotFound"}}'; echo "apl call: HTTP 404 (user error)" >&2; exit 1`, 404},
		{"throttled", `printf ''; echo "apl call: HTTP 429 (server error)" >&2; exit 1`, 429},
		{"server error", `printf ''; echo "apl call: HTTP 503 (server error)" >&2; exit 1`, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeAPL(t, tc.script)
			tr, _ := New("ms:test")
			resp, err := tr.Do(get(t, "https://graph.microsoft.com/v1.0/me"))
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// The error the user can actually FIX. apl computes the exact `apl login` that
// repairs a missing grant; losing that line turns a one-command fix into an
// afternoon of guessing which scope on which handle.
func TestMissingScopeBecomesATypedErrorCarryingAplsOwnRemediation(t *testing.T) {
	fakeAPL(t, `echo "apl call: missing scope(s): ChannelMessage.Read.All. Run: apl login ms:test --force --scope ChannelMessage.Read.All (auth error)" >&2; exit 2`)
	tr, _ := New("ms:test")
	_, err := tr.Do(get(t, "https://graph.microsoft.com/v1.0/me/joinedTeams"))
	if err == nil {
		t.Fatal("a missing scope was reported as success")
	}
	var ae *AuthError
	if !asAuthError(err, &ae) {
		t.Fatalf("not a *AuthError: %T %v", err, err)
	}
	if ae.Remediation != "Run: apl login ms:test --force --scope ChannelMessage.Read.All (auth error)" &&
		!strings.HasPrefix(ae.Remediation, "Run: apl login ms:test --force --scope ChannelMessage.Read.All") {
		t.Fatalf("remediation not carried through verbatim: %q", ae.Remediation)
	}
	if !strings.Contains(err.Error(), "apl login") {
		t.Fatalf("the printed error hides the fix: %v", err)
	}
}

// A body goes over stdin, never argv: message text is arbitrary user input and
// argv is world-readable in `ps`.
func TestBodyTravelsOverStdinNotArgv(t *testing.T) {
	dir := fakeAPL(t, `printf '{"id":"m1"}'`)
	tr, _ := New("ms:test")
	req, err := http.NewRequest(http.MethodPost,
		"https://graph.microsoft.com/v1.0/chats/chat1/messages",
		strings.NewReader(`{"body":{"content":"hello bob"}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := tr.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	argv := argvOf(t, dir)
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, "hello bob") {
		t.Fatalf("the message body leaked into argv: %v", argv)
	}
	if !containsPair(argv, "--body-file", "-") {
		t.Fatalf("body was not sent over stdin: %v", argv)
	}
	stdin, err := os.ReadFile(filepath.Join(dir, "stdin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stdin) != `{"body":{"content":"hello bob"}}` {
		t.Fatalf("stdin = %q", stdin)
	}
	if !containsPair(argv, "-H", "Content-Type: application/json") {
		t.Fatalf("headers not passed through: %v", argv)
	}
}

// The transport exists so a credential never reaches this process. If one is
// handed in anyway it must be dropped, not forwarded.
func TestAuthorizationHeaderIsNeverForwarded(t *testing.T) {
	dir := fakeAPL(t, `printf '{}'`)
	tr, _ := New("ms:test")
	req := get(t, "https://graph.microsoft.com/v1.0/me")
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	resp, err := tr.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if joined := strings.Join(argvOf(t, dir), " "); strings.Contains(joined, "not-a-real-token") {
		t.Fatal("a bearer token was forwarded into apl's argv")
	}
}

// Graph ids look like `19:abc@thread.tacv2`, and both `:` and `@` survive
// url.PathEscape untouched. The teams adapter percent-encodes them itself; this
// asserts the transport does not silently decode them again on the way out,
// which would 404 every request while looking perfectly healthy.
func TestPercentEncodedPathSegmentsSurvive(t *testing.T) {
	dir := fakeAPL(t, `printf '{}'`)
	tr, _ := New("ms:test")
	const u = "https://graph.microsoft.com/v1.0/teams/t1/channels/19%3Aabc%40thread.tacv2/messages?%24top=10"
	resp, err := tr.Do(get(t, u))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	argv := argvOf(t, dir)
	if argv[len(argv)-3] != u && !contains(argv, u) {
		t.Fatalf("url was rewritten on the way to apl: %v", argv)
	}
}

// Scopes declared up front turn a remote 403 into a local, actionable refusal.
func TestScopesArePassedThrough(t *testing.T) {
	dir := fakeAPL(t, `printf '{}'`)
	tr, _ := New("ms:test")
	tr.Scopes = []string{"Chat.Read", "ChannelMessage.Read.All"}
	resp, err := tr.Do(get(t, "https://graph.microsoft.com/v1.0/me"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	argv := argvOf(t, dir)
	if !containsPair(argv, "--scope", "Chat.Read") || !containsPair(argv, "--scope", "ChannelMessage.Read.All") {
		t.Fatalf("scopes missing: %v", argv)
	}
}

// The body is real people's real messages. It must not reach a debug log.
func TestDebugLoggingNeverIncludesTheBody(t *testing.T) {
	fakeAPL(t, `printf '{"value":[{"body":{"content":"a private thing"}}]}'`)
	tr, _ := New("ms:test")
	var lines []string
	tr.Debug = func(s string) { lines = append(lines, s) }
	resp, err := tr.Do(get(t, "https://graph.microsoft.com/v1.0/me/chats"))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(lines) == 0 {
		t.Fatal("nothing was logged at all")
	}
	for _, l := range lines {
		if strings.Contains(l, "a private thing") {
			t.Fatalf("the response body was logged: %q", l)
		}
	}
}

// A handle is the one thing that decides WHOSE mailbox this reads. Defaulting
// it is not an option.
func TestAnEmptyHandleIsRefused(t *testing.T) {
	if _, err := New("  "); err == nil {
		t.Fatal("an empty handle was accepted")
	}
}

// apl missing from PATH must be an error, not a mysterious empty 200.
func TestMissingBinaryIsALoudError(t *testing.T) {
	tr, _ := New("ms:test")
	tr.Binary = filepath.Join(t.TempDir(), "definitely-not-here")
	if _, err := tr.Do(get(t, "https://graph.microsoft.com/v1.0/me")); err == nil {
		t.Fatal("a missing apl binary was reported as success")
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func containsPair(ss []string, flag, val string) bool {
	for i := 0; i+1 < len(ss); i++ {
		if ss[i] == flag && ss[i+1] == val {
			return true
		}
	}
	return false
}

func asAuthError(err error, target **AuthError) bool {
	for err != nil {
		if e, ok := err.(*AuthError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

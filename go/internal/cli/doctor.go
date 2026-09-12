package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/muthuishere/agent-conversations/go/internal/convo"
	"github.com/muthuishere/agent-conversations/go/internal/host/herdr"
)

// doctorTimeout bounds every probe `doctor` runs. A hung `apl` or `herdr`
// child must not hang the whole check — a prerequisite tool that cannot answer
// in a few seconds is itself the finding.
const doctorTimeout = 5 * time.Second

// doctorCheck is one row of the table ADR-001/002 asked for: a name, whether
// it is a hard requirement, who actually needs it, and — on failure — the one
// line that fixes it. Never a stack trace; a teammate reading this output
// should never have to open the source to know what to run next.
type doctorCheck struct {
	Name        string `json:"name"`
	Mandatory   bool   `json:"mandatory"`
	NeededBy    string `json:"needed_by"`
	OK          bool   `json:"ok"`
	Detail      string `json:"detail,omitempty"`
	Remediation string `json:"remediation,omitempty"`
}

// doctorReport is `convo doctor --json`.
type doctorReport struct {
	OK     bool          `json:"ok"`
	Checks []doctorCheck `json:"checks"`
}

// cmdDoctor is the single command the owner's rule asked for: check every
// prerequisite ADR-001 and ADR-002 declare mandatory, print a table, and exit
// 0 ONLY if every mandatory row passed. Everything else here is implementing
// that sentence.
func (a *App) cmdDoctor(ctx context.Context, o options) error {
	rep := doctorReport{OK: true}

	aplCheck := a.doctorAPL(ctx)
	rep.Checks = append(rep.Checks, aplCheck)

	herdrPathCheck, herdrPathOK := a.doctorHerdrOnPath()
	rep.Checks = append(rep.Checks, herdrPathCheck)

	rep.Checks = append(rep.Checks, a.doctorHerdrServer(ctx, herdrPathOK))

	rep.Checks = append(rep.Checks, a.doctorInPane())

	if strings.TrimSpace(o.channel) != "" {
		rep.Checks = append(rep.Checks, a.doctorChannel(ctx, o))
	}

	var failedRemediations []string
	for _, c := range rep.Checks {
		if c.Mandatory && !c.OK {
			rep.OK = false
			failedRemediations = append(failedRemediations, c.Remediation)
		}
	}

	if o.asJSON {
		if err := a.print(o, "", rep); err != nil {
			return err
		}
	} else {
		a.printDoctorTable(rep)
	}

	if !rep.OK {
		return convo.Wrap(convo.ErrNotConfigured,
			"prerequisites not met: %s", strings.Join(failedRemediations, "; "))
	}
	return nil
}

func (a *App) printDoctorTable(rep doctorReport) {
	fmt.Fprintf(a.Stdout, "%-24s %-9s %-42s %-6s %s\n", "CHECK", "REQUIRED", "NEEDED BY", "STATUS", "DETAIL")
	for _, c := range rep.Checks {
		req := "no"
		if c.Mandatory {
			req = "yes"
		}
		status := "ok"
		if !c.OK {
			status = "FAIL"
		}
		detail := c.Detail
		if !c.OK && c.Remediation != "" {
			if detail != "" {
				detail += " -> " + c.Remediation
			} else {
				detail = c.Remediation
			}
		}
		fmt.Fprintf(a.Stdout, "%-24s %-9s %-42s %-6s %s\n", c.Name, req, c.NeededBy, status, detail)
	}
	if rep.OK {
		fmt.Fprintln(a.Stdout, "\nok: apl and herdr are both wired up (ADR-001/002 satisfied)")
	} else {
		fmt.Fprintln(a.Stdout, "\nnot configured: see the FAIL rows above")
	}
}

// doctorAPL is the credential-broker check ADR-002 makes mandatory: apl must
// be on PATH, `apl accounts` must run, and it must actually list a handle —
// an apl with zero handles fronts nothing. Handle LABELS are safe to print;
// `apl accounts` never emits a token, which is the whole point of the broker.
func (a *App) doctorAPL(ctx context.Context) doctorCheck {
	c := doctorCheck{
		Name:      "apl on PATH + accounts",
		Mandatory: true,
		NeededBy:  "fetch, listen, respond (any real channel)",
	}
	path, err := exec.LookPath("apl")
	if err != nil {
		c.Remediation = "install apl: npm install -g @deemwarhq/apl"
		c.Detail = "not found on PATH"
		return c
	}

	cctx, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, path, "accounts", "--json")
	out, err := cmd.Output()
	if err != nil {
		c.Remediation = "run `apl accounts` to see the underlying error"
		c.Detail = "apl accounts failed: " + firstLineOf(err.Error())
		return c
	}
	var accounts []struct {
		Handle string `json:"handle"`
	}
	if uerr := json.Unmarshal(out, &accounts); uerr != nil {
		c.Remediation = "run `apl accounts` to see the underlying error"
		c.Detail = "apl accounts returned unparseable output"
		return c
	}
	if len(accounts) == 0 {
		c.Remediation = "no apl handles: run `apl login <provider>:<label>`"
		c.Detail = "apl accounts is empty"
		return c
	}
	handles := make([]string, 0, len(accounts))
	for _, acc := range accounts {
		handles = append(handles, acc.Handle)
	}
	c.OK = true
	c.Detail = fmt.Sprintf("%d handle(s): %s", len(handles), strings.Join(handles, ", "))
	return c
}

// doctorHerdrOnPath resolves the herdr binary exactly as the host package
// does: $HERDR_BIN_PATH first (a guest process has a different PATH than the
// pane that exported it), PATH otherwise.
func (a *App) doctorHerdrOnPath() (doctorCheck, bool) {
	c := doctorCheck{
		Name:      "herdr on PATH",
		Mandatory: true,
		NeededBy:  "host list|state|deliver, next",
	}
	if bp := strings.TrimSpace(a.env("HERDR_BIN_PATH", "")); bp != "" {
		if st, err := os.Stat(bp); err == nil && !st.IsDir() {
			c.OK = true
			c.Detail = bp + " ($HERDR_BIN_PATH)"
			return c, true
		}
		c.Remediation = "install herdr: $HERDR_BIN_PATH is set but does not point at a file"
		c.Detail = "$HERDR_BIN_PATH=" + bp
		return c, false
	}
	path, err := exec.LookPath("herdr")
	if err != nil {
		c.Remediation = "install herdr and put it on PATH (or set $HERDR_BIN_PATH)"
		c.Detail = "not found on PATH"
		return c, false
	}
	c.OK = true
	c.Detail = path
	return c, true
}

// doctorHerdrServer is the check `herdrPreflight` (host.go) also runs before
// host list|state|deliver and next: resolve the socket the way the herdr host
// itself does — $HERDR_SOCKET_PATH first — and confirm `agent list` succeeds.
// This is what tells apart "herdr is installed" from "a herdr server is
// actually up", which are two different failures with two different fixes.
func (a *App) doctorHerdrServer(ctx context.Context, binOK bool) doctorCheck {
	c := doctorCheck{
		Name:      "herdr server reachable",
		Mandatory: true,
		NeededBy:  "host list|state|deliver, next",
	}
	if !binOK {
		c.Remediation = "install herdr first (see the row above)"
		c.Detail = "herdr binary not found"
		return c
	}
	h, env, err := a.host(options{hostKind: "herdr"})
	if err != nil {
		c.Remediation = "start herdr: run `herdr` in a terminal"
		c.Detail = errMessage(err)
		return c
	}
	cctx, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	if _, err := h.Agents(cctx); err != nil {
		c.Remediation = "start herdr: run `herdr` in a terminal"
		c.Detail = errMessage(err)
		return c
	}
	c.OK = true
	if env.SocketPath != "" {
		c.Detail = "socket " + env.SocketPath
	} else {
		c.Detail = "default socket"
	}
	return c
}

// doctorInPane is informational only: whether THIS process happens to be
// running inside a herdr pane right now. Useful context, never a blocker —
// `convo doctor` from a plain shell must still be able to pass.
func (a *App) doctorInPane() doctorCheck {
	c := doctorCheck{
		Name:     "inside a herdr pane",
		OK:       true,
		NeededBy: "informational only",
	}
	env := herdr.Detect(a.Getenv)
	if env.InHost {
		c.Detail = fmt.Sprintf("yes (pane %s, session %s)", env.PaneID, env.Session)
	} else {
		c.Detail = "no"
	}
	return c
}

// doctorChannel exercises the one channel the caller actually asked about.
// It is mandatory ONLY because `--channel` was given: nothing here should
// force a channel choice on a caller who just wants to check apl/herdr.
func (a *App) doctorChannel(ctx context.Context, o options) doctorCheck {
	c := doctorCheck{
		Name:      "--channel " + o.channel + " configured and reachable",
		Mandatory: true,
		NeededBy:  "fetch, listen, respond (this channel)",
	}
	ch, err := a.channel(o)
	if err != nil {
		c.Remediation = errMessage(err)
		c.Detail = "channel did not configure"
		return c
	}
	cctx, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	id, err := ch.Identity(cctx)
	if err != nil {
		c.Remediation = errMessage(err)
		c.Detail = "Identity() failed"
		return c
	}
	c.OK = true
	c.Detail = "identity: " + id.Name
	return c
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// herdrPreflight is the SHARED fast-fail ADR-001 asked for: `host list|state|
// deliver` and `next` all call this before doing any real work, so a missing
// herdr server fails in well under the time a full CLI shell-out would take,
// with the exact same remediation line `doctor` prints — never a raw JSON
// error bubbled up from the herdr child process.
//
// fetch, listen, journal and respond do NOT call this: they never touch the
// agent host at all, and gating them on herdr would be enforcing a dependency
// they do not have.
func (a *App) herdrPreflight(ctx context.Context, o options) error {
	if o.hostKind != "" && o.hostKind != "herdr" {
		// A caller who explicitly asked for a different Host implementation
		// (the exec test double) is not using herdr, and there is nothing to
		// preflight.
		return nil
	}
	h, _, err := a.host(o)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	if _, err := h.Agents(cctx); err != nil {
		return convo.Wrap(convo.ErrHostUnavailable,
			"herdr server not reachable — start herdr: run `herdr` in a terminal (%s)", errMessage(err))
	}
	return nil
}

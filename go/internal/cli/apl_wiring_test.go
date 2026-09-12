package cli

import (
	"bytes"
	"strings"
	"testing"
)

// An apl handle is enough on its own. Requiring --teams-base-url alongside it
// would make the real-tenant path harder to reach than the simulator path,
// which is exactly backwards.
func TestAplHandleAloneResolvesToRealGraph(t *testing.T) {
	a := &App{Getenv: func(string) string { return "" }, Stderr: &bytes.Buffer{}}
	ch, err := a.channel(options{channel: "teams", teamsAplHandle: "ms:example"})
	if err != nil {
		t.Fatalf("apl-handle channel did not resolve: %v", err)
	}
	if ch == nil {
		t.Fatal("nil channel")
	}
}

// Two credentials configured at once is a misunderstanding, and the safe
// reading is always "use the one that does not put a secret in this process".
// It is announced rather than silently preferred.
func TestAplHandleWinsOverTokenEnvAndSaysSo(t *testing.T) {
	const secret = "not-a-real-token"
	var errOut bytes.Buffer
	a := &App{Stderr: &errOut, Getenv: func(k string) string {
		if k == "TEAMS_AUTH" {
			return secret
		}
		return ""
	}}
	if _, err := a.channel(options{
		channel: "teams", teamsAplHandle: "ms:example", teamsTokenEnv: "TEAMS_AUTH",
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.Contains(errOut.String(), "ignored") {
		t.Fatalf("the token being dropped was not announced: %q", errOut.String())
	}
	if strings.Contains(errOut.String(), secret) {
		t.Fatal("the notice leaked the credential")
	}
}

// The simulator path must keep working exactly as it did: a base URL, no apl.
func TestSimulatorPathIsUnchangedByTheAplOption(t *testing.T) {
	a := &App{Getenv: func(string) string { return "" }, Stderr: &bytes.Buffer{}}
	if _, err := a.channel(options{
		channel: "teams", teamsBaseURL: "http://127.0.0.1:4000/v1.0", teamsUser: "alice",
	}); err != nil {
		t.Fatalf("simulator path broke: %v", err)
	}
}

// ADR-002, Tightened: --teams-token-env is kept ONLY for the local simulator.
// A loopback base URL (the simulator's shape) is allowed through unchanged;
// anything else is refused at exit 65, naming the apl handle as the real fix.
func TestTeamsTokenEnvRefusedAgainstNonLoopback(t *testing.T) {
	const secret = "not-a-real-token"
	getenv := func(k string) string {
		if k == "TEAMS_AUTH" {
			return secret
		}
		return ""
	}

	// Loopback stays allowed — this is the existing simulator path and must
	// not regress.
	for _, u := range []string{
		"http://127.0.0.1:4000/v1.0",
		"http://localhost:4000/v1.0",
		"http://[::1]:4000/v1.0",
	} {
		a := &App{Getenv: getenv, Stderr: &bytes.Buffer{}}
		if _, err := a.channel(options{
			channel: "teams", teamsBaseURL: u, teamsTokenEnv: "TEAMS_AUTH",
		}); err != nil {
			t.Fatalf("loopback base url %q was refused: %v", u, err)
		}
	}

	// A real tenant's URL must be refused outright — apl is the only way in.
	a := &App{Getenv: getenv, Stderr: &bytes.Buffer{}}
	_, err := a.channel(options{
		channel: "teams", teamsBaseURL: "https://graph.microsoft.com/v1.0", teamsTokenEnv: "TEAMS_AUTH",
	})
	if err == nil {
		t.Fatal("a raw token against a non-loopback base url was accepted")
	}
	if !strings.Contains(err.Error(), "teams-apl-handle") {
		t.Fatalf("refusal does not point at the real fix: %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("the refusal leaked the credential")
	}
}

// With neither mode configured there is nowhere to send, and the error has to
// name both ways out.
func TestNeitherModeNamesBothWaysOut(t *testing.T) {
	a := &App{Getenv: func(string) string { return "" }, Stderr: &bytes.Buffer{}}
	_, err := a.channel(options{channel: "teams"})
	if err == nil {
		t.Fatal("a teams channel with no transport was accepted")
	}
	for _, want := range []string{"teams-apl-handle", "teams-base-url"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error does not mention %s: %v", want, err)
		}
	}
}

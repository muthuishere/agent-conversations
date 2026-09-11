package cli

import (
	"strings"
	"testing"
)

// `--channel teams` must actually resolve. It was implemented, tested and
// unreachable: the CLI's channel switch only knew `memory`, so every
// `--channel=teams` died with "unknown channel" and the adapter could not be
// used for anything. An implementation nothing can select is not shipped.
func TestTeamsChannelIsReachable(t *testing.T) {
	a := &App{Getenv: func(string) string { return "" }}
	ch, err := a.channel(options{channel: "teams", teamsBaseURL: "http://127.0.0.1:4000/v1.0"})
	if err != nil {
		t.Fatalf("teams channel did not resolve: %v", err)
	}
	if ch == nil {
		t.Fatal("nil channel")
	}
}

// Without a base URL there is nowhere to send, so it fails at exit 65 rather
// than reporting a reply as sent into the void.
func TestTeamsChannelNeedsABaseURL(t *testing.T) {
	a := &App{Getenv: func(string) string { return "" }}
	if _, err := a.channel(options{channel: "teams"}); err == nil {
		t.Fatal("a teams channel with no base URL was accepted")
	} else if !strings.Contains(err.Error(), "teams-base-url") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

// The token is taken BY THE NAME of an environment variable and read here, so
// it never appears in shell history, in `ps`, or in any log that echoes the
// command line. An empty one fails loudly — an unauthenticated listener gets
// 401s that look exactly like a quiet channel.
func TestTeamsTokenComesFromANamedEnvVarAndIsNeverEchoed(t *testing.T) {
	const secret = "not-a-real-token"
	a := &App{Getenv: func(k string) string {
		if k == "TEAMS_AUTH" {
			return secret
		}
		return ""
	}}
	if _, err := a.channel(options{
		channel: "teams", teamsBaseURL: "http://127.0.0.1:4000/v1.0", teamsTokenEnv: "TEAMS_AUTH",
	}); err != nil {
		t.Fatalf("token by env-var name rejected: %v", err)
	}

	_, err := a.channel(options{
		channel: "teams", teamsBaseURL: "http://127.0.0.1:4000/v1.0", teamsTokenEnv: "MISSING_VAR",
	})
	if err == nil {
		t.Fatal("an empty credential was accepted")
	}
	if !strings.Contains(err.Error(), "MISSING_VAR") {
		t.Fatalf("the error should name the variable: %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("the error message leaked the credential")
	}
}

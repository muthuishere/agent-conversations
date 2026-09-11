package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Both spellings of a flag must work. `--count=10` and `--count 10` are the
// same thing to every other CLI on the machine, and a tool that accepts only
// the first fails its own documented examples with "flag needs an argument" —
// an error that tells you nothing about which spelling it wanted.
//
// The hard part is that this split happens BEFORE the flag package sees
// anything, because message text must never be parsed as options. So both
// properties are asserted together: they are the two halves of one decision.
func TestSplitArgs(t *testing.T) {
	cases := []struct {
		name      string
		cmd       string
		rest      []string
		wantPos   []string
		wantFlags []string
	}{
		{
			name: "attached value", cmd: "next", rest: []string{"--count=10"},
			wantFlags: []string{"--count=10"},
		},
		{
			name: "separated value", cmd: "next", rest: []string{"--count", "10"},
			wantFlags: []string{"--count", "10"},
		},
		{
			name: "flag after the command, then a positional",
			cmd:  "ack", rest: []string{"--tag", "t1", "m-1"},
			wantPos: []string{"m-1"}, wantFlags: []string{"--tag", "t1"},
		},
		{
			name: "positional first, flag after",
			cmd:  "ack", rest: []string{"m-1", "--tag", "t1"},
			wantPos: []string{"m-1"}, wantFlags: []string{"--tag", "t1"},
		},
		{
			name: "boolean flag consumes nothing",
			cmd:  "next", rest: []string{"--ack", "--count", "2"},
			wantFlags: []string{"--ack", "--count", "2"},
		},
		{
			name: "boolean flag does not eat the positional",
			cmd:  "ack", rest: []string{"--json", "m-1"},
			wantPos: []string{"m-1"}, wantFlags: []string{"--json"},
		},
		{
			// The message is free text. A person typing something dash-shaped
			// into a chat window is ordinary traffic, not a malformed command.
			name: "respond text that looks like a flag",
			cmd:  "respond", rest: []string{"abc", "--not-a-flag really"},
			wantPos: []string{"abc", "--not-a-flag really"},
		},
		{
			name: "respond text with flags and spaces, verbatim",
			cmd:  "respond", rest: []string{"--channel", "memory", "m-1", "--count", "3", "is fine"},
			wantPos:   []string{"m-1", "--count 3 is fine"},
			wantFlags: []string{"--channel", "memory"},
		},
		{
			name: "deliver text is verbatim after the agent name",
			cmd:  "host", rest: []string{"deliver", "responder-a", "--json", "and", "--wait"},
			wantPos: []string{"deliver", "responder-a", "--json and --wait"},
		},
		{
			name: "deliver with real leading flags",
			cmd:  "host", rest: []string{"--wait", "--timeout", "30s", "deliver", "responder-a", "hi"},
			wantPos:   []string{"deliver", "responder-a", "hi"},
			wantFlags: []string{"--wait", "--timeout", "30s"},
		},
		{
			name: "host list takes flags in any order",
			cmd:  "host", rest: []string{"list", "--host", "exec"},
			wantPos: []string{"list"}, wantFlags: []string{"--host", "exec"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos, flags := splitArgs(tc.cmd, tc.rest)
			if !eq(pos, tc.wantPos) {
				t.Errorf("positional = %#v, want %#v", pos, tc.wantPos)
			}
			if !eq(flags, tc.wantFlags) {
				t.Errorf("flags = %#v, want %#v", flags, tc.wantFlags)
			}
		})
	}
}

func eq(got, want []string) bool {
	if len(got) == 0 && len(want) == 0 {
		return true
	}
	return reflect.DeepEqual(got, want)
}

// End to end through Run: the separated spelling reaches the flag package as a
// value and not as a positional. This is the bug as it was actually reported —
// `convo next --count 10` died with "flag needs an argument".
func TestSeparatedFlagValueReachesTheCommand(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"AGENT_CONVERSATIONS_HOME": home}

	seed(t, home, "default",
		`{"id":"m-1","at":"2026-09-09T10:00:00Z","from":{"id":"u-alice","name":"alice"},"text":"one","html":null,"source":{"kind":"channel","conversationId":"c-general","name":"#general","threadId":"m-1"},"replyToId":null,"mentionsMe":false}`,
		`{"id":"m-2","at":"2026-09-09T10:00:01Z","from":{"id":"u-bob","name":"bob"},"text":"two","html":null,"source":{"kind":"channel","conversationId":"c-general","name":"#general","threadId":"m-1"},"replyToId":null,"mentionsMe":false}`,
	)

	code, out, errOut := runCLI(t, env, "next", "--count", "1")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "m-1") || strings.Contains(out, "m-2") {
		t.Fatalf("--count 1 delivered %q", out)
	}

	// And the attached spelling is still the same command.
	code, out, errOut = runCLI(t, env, "next", "--count=1")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "m-2") {
		t.Fatalf("--count=1 delivered %q", out)
	}
}

// seed writes journal lines the way the ingest path would.
func seed(t *testing.T, home, tag string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, "journal"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(home, "journal", tag+".ndjson"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

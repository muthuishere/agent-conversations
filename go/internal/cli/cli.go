// Package cli wires the interfaces to a command line.
//
// It is deliberately thin. Every decision of consequence lives behind
// convo.Channel, convo.Host or convo.Store; this file only chooses an
// implementation, parses flags, prints, and maps a typed error to an exit code.
// If a command here starts making policy, the seam is in the wrong place.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	memchan "github.com/muthuishere/agent-conversations/go/internal/channel/memory"
	teamschan "github.com/muthuishere/agent-conversations/go/internal/channel/teams"
	whatsappchan "github.com/muthuishere/agent-conversations/go/internal/channel/whatsapp"
	"github.com/muthuishere/agent-conversations/go/internal/convo"
	"github.com/muthuishere/agent-conversations/go/internal/host/exechost"
	"github.com/muthuishere/agent-conversations/go/internal/host/herdr"
	filestore "github.com/muthuishere/agent-conversations/go/internal/store/file"
)

// Version of the CLI.
const Version = "0.1.0"

// App holds the injectable surface so every command is testable without
// touching the real process: streams, environment and clock all come in.
type App struct {
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
	// NewHost overrides host construction in tests.
	NewHost func(kind string, env herdr.Env, execCmd string) (convo.Host, error)
	// NewChannel overrides channel construction in tests.
	NewChannel func(kind string) (convo.Channel, error)
}

// NewApp builds an App bound to the real process.
func NewApp() *App {
	return &App{Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv}
}

type options struct {
	hostKind string
	execCmd  string
	tag      string
	home     string
	asJSON   bool
	timeout  time.Duration
	wait     bool
	count    int
	ack      bool
	all      bool
	newOnly  bool
	channel  string

	// teams channel configuration. Nothing above the channel seam reads these;
	// they exist here only because a CLI is where a human supplies them.
	teamsBaseURL   string
	teamsUser      string
	teamsTokenEnv  string
	teamsAplHandle string
	teamsAplScopes string
	teamsScan      int

	// whatsapp channel configuration. The handle names an apl identity; this
	// package never sees a credential, which is the point of the broker.
	waHandle       string
	waChatLimit    int
	waMessageLimit int

	// consumer-side delivery filters (next / journal). These NEVER touch the
	// journal: a filtered-out message is still on disk and still visible to
	// `convo journal --all`. See convo.Filter and store/file/held.go.
	mentionsMe  bool
	from        stringList
	excludeFrom stringList
	match       string
	kind        string

	// ingest (fetch / listen)
	in            string
	prime         bool // accepted, no-op: start-at-now is the default (see --replay-history)
	replayHistory bool
	once          bool
	fetchParallel int
	pollActive    time.Duration
	pollMid       time.Duration
	pollIdle      time.Duration
	idle1         time.Duration
	idle2         time.Duration
}

const usage = `convo — a generic agent-conversation CLI.

  convo self                          am I inside an agent host, and which agent am I?
  convo host list                     discover addressable agents
  convo host state <name>             one agent's state
  convo host deliver <name> <text>    hand a message over, respecting backpressure
  convo fetch                         one ingest pass: channel -> journal
  convo listen [--once]               fetch on a loop, with adaptive backoff
  convo journal [--new|--all]         look at the durable log (never consumes)
  convo next [--count n] [--ack]      hand outstanding messages to this consumer
  convo ack <id...> | --all           mark messages processed
  convo respond <messageId> <text>    reply, routed from the message's own source
  convo doctor                        check apl/herdr prerequisites (ADR-001/002); exit 0 only if the mandatory ones pass
  convo version

Global flags:
  --host herdr|exec   which agent host (default herdr, or $CONVO_HOST)
  --exec-cmd '<cmd>'  command for the exec host (or $CONVO_EXEC_CMD)
  --tag <t>           partition the journal/cursors (default $CONVO_TAG or "default")
  --home <dir>        state directory (default $AGENT_CONVERSATIONS_HOME)
  --channel <name>    the transport (built in: memory, teams, whatsapp). No default: a reply
                      with nowhere to go must fail loudly, not silently succeed.
  --json              machine-readable output

Teams channel flags (or the matching $CONVO_TEAMS_* env var):
  --teams-apl-handle <h> RECOMMENDED. An identity from "apl accounts", e.g.
                         ms:<label>. Graph is then reached through apl and this
                         process never sees a credential. Implies a base URL of
                         https://graph.microsoft.com/v1.0 and IGNORES
                         --teams-token-env. $CONVO_TEAMS_APL_HANDLE
  --teams-apl-scope <s>  comma-separated scopes apl must already hold, checked
                         locally before any request leaves. Off by default: apl's
                         record of a grant can understate the token, and refusing
                         a call the tenant would serve is the worse failure.
                         $CONVO_TEAMS_APL_SCOPES
  --teams-base-url <u>   Graph root, no trailing slash. $CONVO_TEAMS_BASE_URL
  --teams-token-env <V>  NAME of the env var holding the bearer token — never the
                         token itself. For a simulator. $CONVO_TEAMS_TOKEN_ENV
  --teams-user <name>    x-user-name, for a Graph-shaped simulator only.
                         $CONVO_TEAMS_USER
  --teams-scan-depth <n> threads per channel scanned for replies (default 10)

WhatsApp channel flags:
  --whatsapp-handle <h>  apl handle for the account, e.g. whatsapp:personal
                         (a bare label is accepted too). $CONVO_WHATSAPP_HANDLE
  --whatsapp-chat-limit <n>     chats pulled per discovery pass (default 200)
  --whatsapp-message-limit <n>  messages pulled per conversation per fetch (default 50)

Delivery filters (next, journal) — composable, applied AT DELIVERY ONLY:
  --mentions-me            only messages that mention the configured identity
  --from <name|id>         repeatable; the repeats OR together
  --exclude-from <name|id> repeatable; wins over --from
  --match <regex>          on the message text
  --in <needle>            conversation id or name (consumer side)
  --kind chat|channel      the source kind
  Different flags AND together. A filtered-out message is NEVER removed from
  the journal ('convo journal --all' shows everything) and NEVER lost from the
  delivery path: 'next' holds it and re-offers it once the filter allows it.

Ingest flags (fetch, listen):
  --in <needle>       only conversations whose id or name matches
  --replay-history    on a conversation with no cursor yet, replay its WHOLE
                      history into the journal. The default is to start it at
                      NOW and ingest nothing from its past: a listener journals
                      what happens while it listens, not the archive.
  --prime             accepted for compatibility; a no-op, because it is now
                      the default
  --fetch-parallel <n> conversations fetched at once (default 6; 1 = sequential)
  --once              listen: run exactly one pass and exit
  --poll-active/-mid/-idle, --idle-1, --idle-2   adaptive backoff tiers

Exit codes: 0 ok · 64 nothing/timeout · 65 bad args or not configured ·
66 conflict/busy · 69 host or target unavailable · 70 self-delivery refused ·
75 target blocked (a human is needed). See go/README.md.
`

// Run executes one invocation and returns the process exit code.
func (a *App) Run(ctx context.Context, argv []string) int {
	err := a.run(ctx, argv)
	if err == nil {
		return convo.ExitOK
	}
	// stderr is always one JSON object with a stable `code`, so a caller never
	// has to parse English to decide what to do.
	enc := json.NewEncoder(a.Stderr)
	_ = enc.Encode(map[string]any{"error": map[string]string{
		"code":    convo.Code(err),
		"message": errMessage(err),
	}})
	return convo.ExitCode(err)
}

func errMessage(err error) string {
	var e *convo.Err
	if errors.As(err, &e) {
		return e.Message
	}
	return err.Error()
}

func (a *App) run(ctx context.Context, argv []string) error {
	if len(argv) == 0 {
		fmt.Fprint(a.Stdout, usage)
		return convo.Wrap(convo.ErrNotConfigured, "no command given")
	}
	cmd := argv[0]
	rest := argv[1:]

	switch cmd {
	case "-h", "--help", "help":
		fmt.Fprint(a.Stdout, usage)
		return nil
	case "version", "--version":
		fmt.Fprintln(a.Stdout, Version)
		return nil
	}

	fs := flag.NewFlagSet("convo", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var o options
	fs.StringVar(&o.hostKind, "host", a.env("CONVO_HOST", "herdr"), "")
	fs.StringVar(&o.execCmd, "exec-cmd", a.env("CONVO_EXEC_CMD", ""), "")
	fs.StringVar(&o.tag, "tag", a.env("CONVO_TAG", "default"), "")
	fs.StringVar(&o.home, "home", a.env("AGENT_CONVERSATIONS_HOME", ""), "")
	fs.BoolVar(&o.asJSON, "json", false, "")
	fs.DurationVar(&o.timeout, "timeout", 0, "")
	fs.BoolVar(&o.wait, "wait", false, "")
	fs.IntVar(&o.count, "count", 0, "")
	fs.BoolVar(&o.ack, "ack", false, "")
	fs.BoolVar(&o.all, "all", false, "")
	fs.BoolVar(&o.newOnly, "new", false, "")
	fs.StringVar(&o.channel, "channel", a.env("CONVO_CHANNEL", ""), "")
	fs.StringVar(&o.teamsBaseURL, "teams-base-url", a.env("CONVO_TEAMS_BASE_URL", ""), "")
	fs.StringVar(&o.teamsUser, "teams-user", a.env("CONVO_TEAMS_USER", ""), "")
	fs.StringVar(&o.teamsTokenEnv, "teams-token-env", a.env("CONVO_TEAMS_TOKEN_ENV", ""), "")
	fs.StringVar(&o.teamsAplHandle, "teams-apl-handle", a.env("CONVO_TEAMS_APL_HANDLE", ""), "")
	fs.StringVar(&o.teamsAplScopes, "teams-apl-scope", a.env("CONVO_TEAMS_APL_SCOPES", ""), "")
	fs.IntVar(&o.teamsScan, "teams-scan-depth", envInt(a.env("CONVO_TEAMS_SCAN_DEPTH", "0")), "")
	fs.StringVar(&o.waHandle, "whatsapp-handle", a.env("CONVO_WHATSAPP_HANDLE", ""), "")
	fs.IntVar(&o.waChatLimit, "whatsapp-chat-limit", envInt(a.env("CONVO_WHATSAPP_CHAT_LIMIT", "0")), "")
	fs.IntVar(&o.waMessageLimit, "whatsapp-message-limit", envInt(a.env("CONVO_WHATSAPP_MESSAGE_LIMIT", "0")), "")
	fs.BoolVar(&o.mentionsMe, "mentions-me", false, "")
	fs.Var(&o.from, "from", "")
	fs.Var(&o.excludeFrom, "exclude-from", "")
	fs.StringVar(&o.match, "match", "", "")
	fs.StringVar(&o.kind, "kind", "", "")
	// --in is BOTH an ingest selector (fetch/listen: what do we poll) and a
	// delivery filter (next/journal: what do we hand over). Same spelling,
	// same needle semantics, different command — deliberately, because
	// narrowing ingest loses history and narrowing delivery does not.
	fs.StringVar(&o.in, "in", "", "")
	fs.BoolVar(&o.prime, "prime", false, "")
	fs.BoolVar(&o.replayHistory, "replay-history", false, "")
	fs.IntVar(&o.fetchParallel, "fetch-parallel", envInt(a.env("CONVO_FETCH_PARALLEL", "0")), "")
	fs.BoolVar(&o.once, "once", false, "")
	fs.DurationVar(&o.pollActive, "poll-active", defaultActive, "")
	fs.DurationVar(&o.pollMid, "poll-mid", defaultMid, "")
	fs.DurationVar(&o.pollIdle, "poll-idle", defaultIdle, "")
	fs.DurationVar(&o.idle1, "idle-1", defaultIdle1, "")
	fs.DurationVar(&o.idle2, "idle-2", defaultIdle2, "")

	// Split positionals from flags so `host deliver a "--not a flag"` works:
	// the text of a message is arbitrary and must never be parsed as options.
	args, flagArgs := splitArgs(cmd, rest)
	if err := fs.Parse(flagArgs); err != nil {
		return convo.Wrap(convo.ErrNotConfigured, "bad arguments: %v", err)
	}

	switch cmd {
	case "self":
		return a.cmdSelf(ctx, o)
	case "host":
		return a.cmdHost(ctx, o, args)
	case "fetch":
		return a.cmdFetch(ctx, o)
	case "listen":
		return a.cmdListen(ctx, o)
	case "journal":
		return a.cmdJournal(ctx, o)
	case "next":
		return a.cmdNext(ctx, o)
	case "ack":
		return a.cmdAck(ctx, o, args)
	case "respond":
		return a.cmdRespond(ctx, o, args)
	case "doctor":
		return a.cmdDoctor(ctx, o)
	default:
		fmt.Fprint(a.Stdout, usage)
		return convo.Wrap(convo.ErrNotConfigured, "unknown command %q", cmd)
	}
}

// boolFlags are the flags that take no value.
//
// This set is what makes `--flag value` possible at all. Go's flag package can
// only tell a value-taking flag from a boolean one AFTER it has been told which
// is which, but the split between "options" and "message text" has to happen
// BEFORE parsing, or arbitrary message text gets eaten as options. So the split
// needs its own copy of that one fact, and this is it. A flag added below and
// forgotten here would swallow the word after it; every value flag is safe by
// default, which is the right way round for a tool whose payload is free text.
var boolFlags = map[string]bool{
	"json": true, "wait": true, "ack": true, "all": true, "new": true,
	"prime": true, "replay-history": true, "once": true, "help": true, "h": true, "version": true,
	"mentions-me": true,
}

// flagName strips the dashes and anything from `=` onwards.
func flagName(s string) string {
	name := strings.TrimLeft(s, "-")
	if i := strings.IndexByte(name, '='); i >= 0 {
		name = name[:i]
	}
	return name
}

// isFlag reports whether a token opens a flag. A bare "-" is not a flag, and
// neither is anything that does not start with one.
func isFlag(s string) bool { return len(s) > 1 && strings.HasPrefix(s, "-") }

// takesValue reports whether a flag token consumes the NEXT argument.
// `--flag=value` carries its own value and consumes nothing.
func takesValue(s string) bool {
	if strings.ContainsRune(s, '=') {
		return false
	}
	return !boolFlags[flagName(s)]
}

// splitArgs separates options from positionals, and keeps message text out of
// the flag parser entirely.
//
// Two properties, and they pull against each other, which is why this is not
// just `flag.Parse`:
//
//   - BOTH spellings work: `--count=10` and `--count 10`. A CLI that silently
//     accepts only one of them is a CLI whose documented examples fail, and
//     "flag needs an argument" is a particularly unhelpful way to learn that.
//   - Message text is VERBATIM. For `host deliver <name> <text>` and
//     `respond <id> <text>`, everything after the positional is the message,
//     including words that look exactly like flags. A message beginning with a
//     dash is ordinary traffic on a real channel, and a CLI that chokes on it
//     is a CLI that drops messages.
func splitArgs(cmd string, rest []string) (positional, flags []string) {
	// scanFlags consumes leading options, stopping at the first non-flag.
	// It returns the index it stopped at.
	scanFlags := func(i int) int {
		for i < len(rest) && isFlag(rest[i]) {
			flags = append(flags, rest[i])
			if takesValue(rest[i]) && i+1 < len(rest) {
				i++
				flags = append(flags, rest[i])
			}
			i++
		}
		return i
	}

	// verbatimAfter takes `n` positionals (scanning flags in between) and then
	// joins everything that is left, untouched, as one final positional.
	verbatimAfter := func(n int) ([]string, []string) {
		i := 0
		for len(positional) < n {
			i = scanFlags(i)
			if i >= len(rest) {
				break
			}
			positional = append(positional, rest[i])
			i++
		}
		if i < len(rest) {
			positional = append(positional, strings.Join(rest[i:], " "))
		}
		return positional, flags
	}

	// scanAll takes options and positionals in any order, to the end.
	scanAll := func() ([]string, []string) {
		for i := 0; i < len(rest); i++ {
			if isFlag(rest[i]) {
				flags = append(flags, rest[i])
				if takesValue(rest[i]) && i+1 < len(rest) {
					i++
					flags = append(flags, rest[i])
				}
				continue
			}
			positional = append(positional, rest[i])
		}
		return positional, flags
	}

	switch cmd {
	case "host":
		// The sub-verb comes first; only `deliver` has free text after it.
		i := scanFlags(0)
		if i >= len(rest) {
			return nil, flags
		}
		sub := rest[i]
		positional = append(positional, sub)
		rest = rest[i+1:]
		if sub == "deliver" {
			// <name>, then everything else is the message, verbatim.
			return verbatimAfter(2)
		}
		return scanAll()

	case "respond":
		// <messageId>, then everything else is the reply, verbatim.
		return verbatimAfter(1)
	}

	return scanAll()
}

// envInt reads a small integer from an environment value, treating anything
// unparseable as unset rather than failing: a malformed tuning knob must not
// stop the tool from running.
func envInt(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (a *App) env(key, def string) string {
	if a.Getenv == nil {
		return def
	}
	if v := a.Getenv(key); v != "" {
		return v
	}
	return def
}

func (a *App) host(o options) (convo.Host, herdr.Env, error) {
	env := herdr.Detect(a.Getenv)
	if a.NewHost != nil {
		h, err := a.NewHost(o.hostKind, env, o.execCmd)
		return h, env, err
	}
	switch o.hostKind {
	case "", "herdr":
		return herdr.New(env), env, nil
	case "exec":
		if o.execCmd == "" {
			return nil, env, convo.Wrap(convo.ErrNotConfigured,
				"--host exec needs --exec-cmd or $CONVO_EXEC_CMD")
		}
		return exechost.New("spawn", o.execCmd), env, nil
	default:
		return nil, env, convo.Wrap(convo.ErrNotConfigured, "unknown host %q", o.hostKind)
	}
}

// channel resolves the transport.
//
// There is NO DEFAULT on purpose. A reply that goes nowhere while reporting
// success is the exact failure this repo exists to prevent, so an unconfigured
// respond fails loudly at exit 65 instead of quietly doing nothing.
func (a *App) channel(o options) (convo.Channel, error) {
	if a.NewChannel != nil {
		return a.NewChannel(o.channel)
	}
	switch o.channel {
	case "memory":
		// In-process only: useful in a test or a demo, useless across
		// processes. Says so rather than pretending.
		return memchan.New(), nil
	case "teams":
		return a.teamsChannel(o)
	case "whatsapp":
		return a.whatsappChannel(o)
	case "":
		return nil, convo.Wrap(convo.ErrNotConfigured,
			"no channel configured — pass --channel or $CONVO_CHANNEL; refusing to "+
				"report a reply as sent when it has nowhere to go")
	default:
		return nil, convo.Wrap(convo.ErrNotConfigured, "unknown channel %q", o.channel)
	}
}

// teamsChannel builds the Graph-shaped channel from flags and environment.
//
// The token is taken BY THE NAME of an environment variable, never by value.
// A CLI that accepts `--teams-token <secret>` puts that secret in the shell
// history, in `ps` output, and in every log line that echoes the command — so
// this one takes `--teams-token-env AUTH_VAR` and reads the value itself. The
// value is never printed, and no error message below ever contains it.
func (a *App) teamsChannel(o options) (convo.Channel, error) {
	// Mode 1: apl. The broker holds the OAuth grant and refreshes it, so no
	// token exists in a flag, an env var, this process's memory, `ps` output or
	// the shell history. A handle is a LABEL — safe everywhere a token is not.
	if h := strings.TrimSpace(o.teamsAplHandle); h != "" {
		cfg := teamschan.Config{
			BaseURL:        o.teamsBaseURL, // empty -> real Graph
			ReplyScanDepth: o.teamsScan,
		}
		// Said out loud rather than silently preferred: two credentials
		// configured at once is a misunderstanding, and the safe reading is
		// always "use the one that does not put a secret in this process".
		if o.teamsTokenEnv != "" && a.Stderr != nil {
			fmt.Fprintf(a.Stderr,
				"convo: --teams-apl-handle %s is set, so --teams-token-env %s is ignored\n",
				h, o.teamsTokenEnv)
		}
		return teamschan.NewViaAPL(cfg, h, splitList(o.teamsAplScopes)...)
	}

	// Mode 2: a bearer token, by the NAME of an env var. Kept for a
	// Graph-shaped simulator, which has no apl identity to speak of.
	if strings.TrimSpace(o.teamsBaseURL) == "" {
		return nil, convo.Wrap(convo.ErrNotConfigured,
			"--channel teams needs --teams-apl-handle (recommended: an identity "+
				"from `apl accounts`, and no credential ever reaches this process) "+
				"or --teams-base-url (or $CONVO_TEAMS_BASE_URL), "+
				"e.g. http://127.0.0.1:4000/v1.0 for a simulator")
	}
	cfg := teamschan.Config{
		BaseURL:        o.teamsBaseURL,
		UserName:       o.teamsUser,
		ReplyScanDepth: o.teamsScan,
	}
	if o.teamsTokenEnv != "" {
		// ADR-002 (Tightened): a raw token is kept ONLY for the local
		// simulator, and is not to be used against a real tenant. That is a
		// rule, not a suggestion, so it is enforced here rather than left to a
		// teammate's memory — a real tenant goes through apl
		// (--teams-apl-handle), full stop.
		if !isLoopbackURL(o.teamsBaseURL) {
			return nil, convo.Wrap(convo.ErrNotConfigured,
				"--teams-token-env is for the local simulator only (ADR-002, "+
					"Tightened) — %q is not loopback; use --teams-apl-handle "+
					"for a real tenant", o.teamsBaseURL)
		}
		tok := a.env(o.teamsTokenEnv, "")
		if tok == "" {
			// Name the VARIABLE, never the value. Failing loudly here is the
			// point: an unauthenticated listener against a real tenant gets
			// 401s that look exactly like a quiet channel.
			return nil, convo.Wrap(convo.ErrNotConfigured,
				"$%s is empty — refusing to talk to Teams with no credential", o.teamsTokenEnv)
		}
		if !strings.Contains(tok, " ") {
			tok = "Bearer " + tok
		}
		cfg.Authorization = tok
	}
	return teamschan.New(cfg)
}

// whatsappChannel builds the wacli/apl-backed channel.
//
// There is no credential to pass and no token flag to get wrong: `apl` owns the
// identity and injects the account, so the only configuration is WHICH account
// — and naming one is mandatory. Defaulting to wacli's own default account
// would mean an unattended listener could start answering from whichever number
// happened to be configured, which is the kind of mistake that is only found
// after a message has gone out.
func (a *App) whatsappChannel(o options) (convo.Channel, error) {
	if strings.TrimSpace(o.waHandle) == "" {
		return nil, convo.Wrap(convo.ErrNotConfigured,
			"--channel whatsapp needs --whatsapp-handle (or $CONVO_WHATSAPP_HANDLE), "+
				"e.g. whatsapp:personal — refusing to guess which account speaks")
	}
	return whatsappchan.New(whatsappchan.Config{
		Handle:       o.waHandle,
		ChatLimit:    o.waChatLimit,
		MessageLimit: o.waMessageLimit,
	})
}

func (a *App) store(o options) (*filestore.Store, error) {
	return filestore.New(o.home, o.tag)
}

func (a *App) print(o options, human string, v any) error {
	if o.asJSON {
		enc := json.NewEncoder(a.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	fmt.Fprintln(a.Stdout, human)
	return nil
}

// isLoopbackURL reports whether raw's host is loopback — localhost, 127.0.0.1
// or ::1 — which is the ONLY place ADR-002 (Tightened) allows a raw
// --teams-token-env to be used. A URL that fails to parse is treated as not
// loopback: refusing is the safe direction for a malformed base URL, never
// silently allowing a raw token through.
func isLoopbackURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// splitList turns a comma-separated flag into a slice, dropping blanks so a
// trailing comma is not a scope named "".
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

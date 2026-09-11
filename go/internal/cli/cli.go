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
	"os"
	"strings"
	"time"

	memchan "github.com/muthuishere/agent-conversations/go/internal/channel/memory"
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
}

const usage = `convo — a generic agent-conversation CLI.

  convo self                          am I inside an agent host, and which agent am I?
  convo host list                     discover addressable agents
  convo host state <name>             one agent's state
  convo host deliver <name> <text>    hand a message over, respecting backpressure
  convo journal [--new]               look at the durable log (never consumes)
  convo next [--count n] [--ack]      hand outstanding messages to this consumer
  convo ack <id...> | --all           mark messages processed
  convo respond <messageId> <text>    reply, routed from the message's own source
  convo version

Global flags:
  --host herdr|exec   which agent host (default herdr, or $CONVO_HOST)
  --exec-cmd '<cmd>'  command for the exec host (or $CONVO_EXEC_CMD)
  --tag <t>           partition the journal/cursors (default $CONVO_TAG or "default")
  --home <dir>        state directory (default $AGENT_CONVERSATIONS_HOME)
  --channel <name>    the transport (built in: memory). No default: a reply with
                      nowhere to go must fail loudly, not silently succeed.
  --json              machine-readable output

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
	case "journal":
		return a.cmdJournal(ctx, o)
	case "next":
		return a.cmdNext(ctx, o)
	case "ack":
		return a.cmdAck(ctx, o, args)
	case "respond":
		return a.cmdRespond(ctx, o, args)
	default:
		fmt.Fprint(a.Stdout, usage)
		return convo.Wrap(convo.ErrNotConfigured, "unknown command %q", cmd)
	}
}

// splitArgs keeps message text out of the flag parser.
//
// For `host deliver <name> <text>` everything after the name is the message,
// verbatim. A message beginning with a dash is ordinary traffic on a real
// channel, and a CLI that chokes on it is a CLI that drops messages.
func splitArgs(cmd string, rest []string) (positional, flags []string) {
	if cmd == "host" && len(rest) >= 1 {
		// find the sub-verb
		var sub string
		var i int
		for ; i < len(rest); i++ {
			if !strings.HasPrefix(rest[i], "-") {
				sub = rest[i]
				break
			}
			flags = append(flags, rest[i])
		}
		if sub == "" {
			return nil, flags
		}
		positional = append(positional, sub)
		i++
		if sub == "deliver" {
			// name, then everything else is text (minus leading flags)
			for ; i < len(rest); i++ {
				if strings.HasPrefix(rest[i], "--") && len(positional) < 2 {
					flags = append(flags, rest[i])
					continue
				}
				break
			}
			if i < len(rest) {
				positional = append(positional, rest[i])
				i++
			}
			if i < len(rest) {
				positional = append(positional, strings.Join(rest[i:], " "))
			}
			return positional, flags
		}
		for ; i < len(rest); i++ {
			if strings.HasPrefix(rest[i], "-") {
				flags = append(flags, rest[i])
				continue
			}
			positional = append(positional, rest[i])
		}
		return positional, flags
	}
	if cmd == "respond" {
		// <messageId> then everything else is the reply text, verbatim.
		var i int
		for ; i < len(rest); i++ {
			if strings.HasPrefix(rest[i], "-") && len(positional) == 0 {
				flags = append(flags, rest[i])
				continue
			}
			break
		}
		if i < len(rest) {
			positional = append(positional, rest[i])
			i++
		}
		if i < len(rest) {
			positional = append(positional, strings.Join(rest[i:], " "))
		}
		return positional, flags
	}
	for _, s := range rest {
		if strings.HasPrefix(s, "-") {
			flags = append(flags, s)
		} else {
			positional = append(positional, s)
		}
	}
	return positional, flags
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
	case "":
		return nil, convo.Wrap(convo.ErrNotConfigured,
			"no channel configured — pass --channel or $CONVO_CHANNEL; refusing to "+
				"report a reply as sent when it has nowhere to go")
	default:
		return nil, convo.Wrap(convo.ErrNotConfigured, "unknown channel %q", o.channel)
	}
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

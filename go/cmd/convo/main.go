// Command convo is a generic agent-conversation CLI.
//
// It is mostly an interface. The commands here are thin; the value is in
// internal/convo, where three seams are defined — Channel (where messages come
// from), Host (where agents live) and Store (what survives a crash) — and in
// the fact that two very different hosts and a fake channel already implement
// them.
//
// See go/README.md for the exit codes, and reference/skill/HERDR.md for how an
// agent running inside a Herdr pane is supposed to use this.
package main

import (
	"context"
	"os"

	"github.com/muthuishere/agent-conversations/go/internal/cli"
)

func main() {
	os.Exit(cli.NewApp().Run(context.Background(), os.Args[1:]))
}

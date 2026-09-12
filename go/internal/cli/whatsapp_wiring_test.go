package cli

import (
	"strings"
	"testing"
)

// An implementation nothing can select is not shipped: `--channel whatsapp`
// must actually resolve.
func TestWhatsAppChannelIsReachable(t *testing.T) {
	a := &App{Getenv: func(string) string { return "" }}
	ch, err := a.channel(options{channel: "whatsapp", waHandle: "whatsapp:personal"})
	if err != nil {
		t.Fatalf("whatsapp channel did not resolve: %v", err)
	}
	if ch == nil {
		t.Fatal("nil channel")
	}
}

// The handle also comes from the environment, which is how a long-running
// listener is configured without putting it in every command line.
func TestWhatsAppHandleFromEnvironment(t *testing.T) {
	a := &App{Getenv: func(k string) string {
		if k == "CONVO_WHATSAPP_HANDLE" {
			return "whatsapp:personal"
		}
		return ""
	}}
	if _, err := a.channel(options{channel: "whatsapp", waHandle: a.env("CONVO_WHATSAPP_HANDLE", "")}); err != nil {
		t.Fatalf("handle from the environment was rejected: %v", err)
	}
}

// With no handle there is no account, and defaulting to wacli's own default
// would mean an unattended listener could answer from the wrong phone number.
// That fails at exit 65 instead.
func TestWhatsAppNeedsAHandle(t *testing.T) {
	a := &App{Getenv: func(string) string { return "" }}
	_, err := a.channel(options{channel: "whatsapp"})
	if err == nil {
		t.Fatal("a whatsapp channel with no handle was accepted")
	}
	if !strings.Contains(err.Error(), "whatsapp-handle") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

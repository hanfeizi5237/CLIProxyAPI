package synthesizer

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestConfigSynthesizer_CodexKeys_ChatModeDisablesWebsockets(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			CodexKey: []config.CodexKey{{
				APIKey:      "codex-key-123",
				BaseURL:     "https://api.example.com",
				RequestMode: "chat",
				Websockets:  true,
			}},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	if got := auths[0].Attributes["request_mode"]; got != "chat" {
		t.Fatalf("request_mode = %q, want chat", got)
	}
	if got := auths[0].Attributes["websockets"]; got != "" {
		t.Fatalf("websockets = %q, want empty for chat request mode", got)
	}
}

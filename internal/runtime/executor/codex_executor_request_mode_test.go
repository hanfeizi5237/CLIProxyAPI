package executor

import (
	"context"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestCodexExecutorExecuteChatModeRejectsCodexDownstream(t *testing.T) {
	exec := &CodexExecutor{}
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"request_mode": "chat"}}
	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5-codex"}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); got == "" || got == "<nil>" {
		t.Fatalf("unexpected empty error: %#v", err)
	}
}

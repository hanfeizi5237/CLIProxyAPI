package openai

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestWebsocketUpstreamSupportsIncrementalInputFalseForChatMode(t *testing.T) {
	attrs := map[string]string{
		"request_mode": "chat",
		"websockets":   "true",
	}
	if websocketUpstreamSupportsIncrementalInput(attrs, nil) {
		t.Fatal("expected incremental input to be disabled for chat request mode")
	}
}

func TestResponsesWebsocketAuthSupportsCompactionReplayFalseForChatMode(t *testing.T) {
	auth := &coreauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"request_mode": "chat",
		},
	}
	if responsesWebsocketAuthSupportsCompactionReplay(auth) {
		t.Fatal("expected compaction replay disabled for chat request mode")
	}
}

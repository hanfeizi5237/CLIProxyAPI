package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
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

func TestCodexExecutorChatModeRoutesOpenAIChatToChatCompletions(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	exec := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"request_mode": "chat",
		"base_url":     server.URL + "/v1",
		"api_key":      "test-key",
	}}
	payload := []byte(`{"model":"qwen-coder-plan","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "qwen-coder-plan",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", gotPath)
	}
	if !gjson.GetBytes(gotBody, "messages").Exists() {
		t.Fatalf("expected chat messages in upstream body: %s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "input").Exists() {
		t.Fatalf("unexpected responses input in upstream body: %s", string(gotBody))
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "ok" {
		t.Fatalf("response content = %q, want ok; payload=%s", got, string(resp.Payload))
	}
}

func TestCodexExecutorChatModeResolvesConfigModelAlias(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:      "test-key",
				BaseURL:     server.URL + "/v1",
				RequestMode: "chat",
				Models: []config.CodexModel{
					{Name: "qwen3.6-plus", Alias: "gpt-5.4"},
				},
			},
		},
	}
	exec := NewCodexExecutor(cfg)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"request_mode": "chat",
		"base_url":     server.URL + "/v1",
		"api_key":      "test-key",
	}}
	payload := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := gjson.GetBytes(gotBody, "model").String(); got != "qwen3.6-plus" {
		t.Fatalf("upstream model = %q, want qwen3.6-plus; body=%s", got, string(gotBody))
	}
}

func TestCodexExecutorChatModeRoutesResponsesToChatCompletions(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","created":1700000000,"model":"qwen-coder-plan","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	exec := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"request_mode": "chat",
		"base_url":     server.URL + "/v1",
		"api_key":      "test-key",
	}}
	payload := []byte(`{"model":"qwen-coder-plan","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":false}`)
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "qwen-coder-plan",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", gotPath)
	}
	if !gjson.GetBytes(gotBody, "messages").Exists() {
		t.Fatalf("expected converted chat messages in upstream body: %s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "input").Exists() {
		t.Fatalf("unexpected responses input in upstream body: %s", string(gotBody))
	}
	if got := gjson.GetBytes(resp.Payload, "object").String(); got != "response" {
		t.Fatalf("response object = %q, want response; payload=%s", got, string(resp.Payload))
	}
}

func TestCodexWebsocketsEnabledFalseForChatMode(t *testing.T) {
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"request_mode": "chat",
		"websockets":   "true",
	}}
	if codexWebsocketsEnabled(auth) {
		t.Fatal("expected codex websockets disabled for chat request mode")
	}
}

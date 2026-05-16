package openai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

func TestOpenAIChatCompletions_CodexAPIKeyAliasRoutesUpstreamAndRestoresVisibleModel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var upstreamPath string
	var upstreamAuthorization string
	var upstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		upstreamAuthorization = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		upstreamBody = body

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_alias","object":"chat.completion","model":"qwen3.6-plus","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	cfg := &internalconfig.Config{
		CodexKey: []internalconfig.CodexKey{
			{
				APIKey:      "codex-key",
				BaseURL:     upstream.URL + "/v1",
				RequestMode: "chat",
				Models: []internalconfig.CodexModel{
					{Name: "qwen3.6-plus", Alias: "gpt-5.4"},
				},
			},
		},
	}

	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	manager.RegisterExecutor(runtimeexecutor.NewCodexExecutor(cfg))

	authID := "codex-alias-integration-" + strings.ReplaceAll(t.Name(), "/", "-")
	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":      "codex-key",
			"base_url":     upstream.URL + "/v1",
			"request_mode": "chat",
		},
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "gpt-5.4"}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth.ID)
	})

	if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	handler := NewOpenAIAPIHandler(base)

	reqBody := `{"model":"gpt-5.4","messages":[{"role":"user","content":"请只回复 ok"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = req

	handler.ChatCompletions(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if upstreamPath != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q, want /v1/chat/completions", upstreamPath)
	}
	if upstreamAuthorization != "Bearer codex-key" {
		t.Fatalf("upstream Authorization = %q, want Bearer codex-key", upstreamAuthorization)
	}
	if got := gjson.GetBytes(upstreamBody, "model").String(); got != "qwen3.6-plus" {
		t.Fatalf("upstream model = %q, want qwen3.6-plus; body=%s", got, string(upstreamBody))
	}
	if got := gjson.Get(rec.Body.String(), "model").String(); got != "gpt-5.4" {
		t.Fatalf("client visible model = %q, want gpt-5.4; body=%s", got, rec.Body.String())
	}
	if got := gjson.Get(rec.Body.String(), "choices.0.message.content").String(); got != "ok" {
		t.Fatalf("client content = %q, want ok; body=%s", got, rec.Body.String())
	}
}

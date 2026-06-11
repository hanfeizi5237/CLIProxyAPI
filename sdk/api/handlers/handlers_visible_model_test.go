package handlers

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

type visibleModelExecutor struct {
	payload []byte
}

func (e *visibleModelExecutor) Identifier() string { return "codex" }

func (e *visibleModelExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{Payload: bytes.Clone(e.payload)}, nil
}

func (e *visibleModelExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	ch := make(chan coreexecutor.StreamChunk, 2)
	ch <- coreexecutor.StreamChunk{Payload: append([]byte("event: response.completed\ndata: "), append(bytes.Clone(e.payload), '\n')...)}
	ch <- coreexecutor.StreamChunk{Payload: []byte("data: [DONE]\n")}
	close(ch)
	return &coreexecutor.StreamResult{Chunks: ch}, nil
}

func (e *visibleModelExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{Payload: bytes.Clone(e.payload)}, nil
}

func (e *visibleModelExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *visibleModelExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "HttpRequest not implemented", HTTPStatus: http.StatusNotImplemented}
}

func TestBaseAPIHandlerRestoresClientVisibleModelNonStreamAndCount(t *testing.T) {
	handler := newVisibleModelTestHandler(t, []byte(`{"model":"qwen3.6-plus","response":{"model":"qwen3.6-plus","modelVersion":"qwen3.6-plus"},"message":{"model":"qwen3.6-plus"},"modelVersion":"qwen3.6-plus"}`))

	resp, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", "gpt-5.4", []byte(`{"model":"gpt-5.4"}`), "")
	if errMsg != nil {
		t.Fatalf("ExecuteWithAuthManager error: %+v", errMsg)
	}
	assertVisibleModelFields(t, resp, "gpt-5.4")

	countResp, _, errMsg := handler.ExecuteCountWithAuthManager(context.Background(), "openai", "gpt-5.4", []byte(`{"model":"gpt-5.4"}`), "")
	if errMsg != nil {
		t.Fatalf("ExecuteCountWithAuthManager error: %+v", errMsg)
	}
	assertVisibleModelFields(t, countResp, "gpt-5.4")
}

func TestBaseAPIHandlerRestoresClientVisibleModelStream(t *testing.T) {
	handler := newVisibleModelTestHandler(t, []byte(`{"model":"qwen3.6-plus","response":{"model":"qwen3.6-plus","modelVersion":"qwen3.6-plus"},"message":{"model":"qwen3.6-plus"},"modelVersion":"qwen3.6-plus"}`))

	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "openai-response", "gpt-5.4", []byte(`{"model":"gpt-5.4"}`), "")
	if dataChan == nil || errChan == nil {
		t.Fatal("expected non-nil stream channels")
	}

	var first []byte
	for chunk := range dataChan {
		if len(first) == 0 && !bytes.Contains(chunk, []byte("[DONE]")) {
			first = bytes.Clone(chunk)
		}
	}
	for errMsg := range errChan {
		if errMsg != nil {
			t.Fatalf("unexpected stream error: %+v", errMsg)
		}
	}

	lines := bytes.Split(first, []byte("\n"))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		assertVisibleModelFields(t, bytes.TrimSpace(line[len("data:"):]), "gpt-5.4")
		return
	}
	t.Fatalf("missing data line in stream payload: %s", string(first))
}

func TestBaseAPIHandlerRestoresClientVisibleModelAfterResponseInterceptor(t *testing.T) {
	handler := newVisibleModelTestHandler(t, []byte(`{"model":"qwen3.6-plus","response":{"model":"qwen3.6-plus","modelVersion":"qwen3.6-plus"},"message":{"model":"qwen3.6-plus"},"modelVersion":"qwen3.6-plus"}`))
	handler.SetPluginHost(&handlerInterceptorTestHost{
		interceptResponse: func(_ context.Context, req pluginapi.ResponseInterceptRequest) pluginapi.ResponseInterceptResponse {
			return pluginapi.ResponseInterceptResponse{Body: bytes.Clone(req.Body)}
		},
	})

	resp, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", "gpt-5.4", []byte(`{"model":"gpt-5.4"}`), "")
	if errMsg != nil {
		t.Fatalf("ExecuteWithAuthManager error: %+v", errMsg)
	}
	assertVisibleModelFields(t, resp, "gpt-5.4")

	countResp, _, errMsg := handler.ExecuteCountWithAuthManager(context.Background(), "openai", "gpt-5.4", []byte(`{"model":"gpt-5.4"}`), "")
	if errMsg != nil {
		t.Fatalf("ExecuteCountWithAuthManager error: %+v", errMsg)
	}
	assertVisibleModelFields(t, countResp, "gpt-5.4")
}

func TestBaseAPIHandlerRestoresClientVisibleModelAfterStreamInterceptor(t *testing.T) {
	handler := newVisibleModelTestHandler(t, []byte(`{"model":"qwen3.6-plus","response":{"model":"qwen3.6-plus","modelVersion":"qwen3.6-plus"},"message":{"model":"qwen3.6-plus"},"modelVersion":"qwen3.6-plus"}`))
	handler.SetPluginHost(&handlerInterceptorTestHost{
		interceptStreamChunk: func(_ context.Context, req pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
			return pluginapi.StreamChunkInterceptResponse{
				Headers: cloneHeader(req.ResponseHeaders),
				Body:    cloneBytes(req.Body),
			}
		},
	})

	dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "openai-response", "gpt-5.4", []byte(`{"model":"gpt-5.4"}`), "")
	if dataChan == nil || errChan == nil {
		t.Fatal("expected non-nil stream channels")
	}

	var first []byte
	for chunk := range dataChan {
		if len(first) == 0 && !bytes.Contains(chunk, []byte("[DONE]")) {
			first = bytes.Clone(chunk)
		}
	}
	for errMsg := range errChan {
		if errMsg != nil {
			t.Fatalf("unexpected stream error: %+v", errMsg)
		}
	}

	lines := bytes.Split(first, []byte("\n"))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		assertVisibleModelFields(t, bytes.TrimSpace(line[len("data:"):]), "gpt-5.4")
		return
	}
	t.Fatalf("missing data line in stream payload: %s", string(first))
}

func newVisibleModelTestHandler(t *testing.T, payload []byte) *BaseAPIHandler {
	t.Helper()

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&visibleModelExecutor{payload: payload})

	auth := &coreauth.Auth{
		ID:       "visible-model-auth",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"email": "visible-model@example.com"},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("manager.Register: %v", err)
	}

	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "gpt-5.4"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	return NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
}

func assertVisibleModelFields(t *testing.T, payload []byte, want string) {
	t.Helper()

	for _, path := range clientVisibleModelPaths() {
		got := gjson.GetBytes(payload, path).String()
		if got != want {
			t.Fatalf("%s = %q, want %q; payload=%s", path, got, want, string(payload))
		}
	}
}

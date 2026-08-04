package executor

import (
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/sjson"
)

// executeChatMode routes a Codex credential configured with request-mode=chat to the
// OpenAI-compatible /chat/completions (or /v1/responses) upstream via the compat executor.
func (e *CodexExecutor) executeChatMode(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if opts.SourceFormat == sdktranslator.FromString("codex") {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusBadRequest, msg: "request-mode=chat does not support codex downstream format; use OpenAI /v1/chat/completions or /v1/responses"}
	}
	req = e.resolveChatModeAliasRequest(req, auth)
	compat := NewOpenAICompatExecutor(e.Identifier(), e.cfg)
	return compat.Execute(ctx, auth, req, opts)
}

func (e *CodexExecutor) executeChatModeStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if opts.SourceFormat == sdktranslator.FromString("codex") {
		return nil, statusErr{code: http.StatusBadRequest, msg: "request-mode=chat does not support codex downstream format; use OpenAI /v1/chat/completions or /v1/responses"}
	}
	req = e.resolveChatModeAliasRequest(req, auth)
	compat := NewOpenAICompatExecutor(e.Identifier(), e.cfg)
	return compat.ExecuteStream(ctx, auth, req, opts)
}

func (e *CodexExecutor) countTokensChatMode(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if opts.SourceFormat == sdktranslator.FromString("codex") {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusBadRequest, msg: "request-mode=chat does not support codex downstream format for token counting"}
	}
	req = e.resolveChatModeAliasRequest(req, auth)
	compat := NewOpenAICompatExecutor(e.Identifier(), e.cfg)
	return compat.CountTokens(ctx, auth, req, opts)
}

// codexRequestMode resolves the effective request mode for a Codex auth, preferring the
// per-auth attribute and falling back to the credential configuration.
func (e *CodexExecutor) codexRequestMode(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Attributes != nil {
		if mode := config.NormalizeCodexRequestMode(auth.Attributes["request_mode"]); mode != "responses" || strings.TrimSpace(auth.Attributes["request_mode"]) != "" {
			return mode
		}
	}
	if resolved := e.resolveCodexConfig(auth); resolved != nil {
		return config.NormalizeCodexRequestMode(resolved.RequestMode)
	}
	return "responses"
}

func (e *CodexExecutor) resolveChatModeAliasRequest(req cliproxyexecutor.Request, auth *cliproxyauth.Auth) cliproxyexecutor.Request {
	resolved := e.resolveCodexChatModeModel(req.Model, auth)
	if resolved == "" || resolved == req.Model {
		return req
	}
	req.Model = resolved
	if len(req.Payload) == 0 {
		return req
	}
	updated, errSet := sjson.SetBytes(req.Payload, "model", resolved)
	if errSet == nil {
		req.Payload = updated
	}
	return req
}

func (e *CodexExecutor) resolveCodexChatModeModel(model string, auth *cliproxyauth.Auth) string {
	entry := e.resolveCodexConfig(auth)
	if entry == nil || len(entry.Models) == 0 {
		return model
	}
	suffix := thinking.ParseSuffix(model)
	requestedModel := strings.TrimSpace(suffix.ModelName)
	if requestedModel == "" {
		requestedModel = strings.TrimSpace(model)
	}
	if requestedModel == "" {
		return model
	}
	for _, candidate := range entry.Models {
		if !strings.EqualFold(strings.TrimSpace(candidate.Alias), requestedModel) {
			continue
		}
		resolved := strings.TrimSpace(candidate.Name)
		if resolved == "" {
			break
		}
		if suffix.HasSuffix {
			return resolved + "(" + suffix.RawSuffix + ")"
		}
		return resolved
	}
	return model
}

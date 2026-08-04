package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// executeChatMode routes an xAI credential configured with request-mode=chat to the
// upstream OpenAI-compatible /chat/completions endpoint using OpenAI chat translations.
func (e *XAIExecutor) executeChatMode(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	token, _ := xaiCreds(auth)
	baseURL := xaiChatBaseURL(auth)
	logXAIResolvedBaseURL(ctx, baseURL)

	prepared, err := e.prepareChatCompletionsRequest(ctx, req, opts, false)
	if err != nil {
		return resp, err
	}

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), prepared.baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)
	reporter.SetTranslatedReasoningEffort(prepared.body, e.Identifier())

	url := strings.TrimSuffix(baseURL, "/") + "/chat/completions"
	e.logXAIDiagnostic(auth, req, opts, prepared, "/chat/completions", false)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(prepared.body))
	if err != nil {
		return resp, err
	}
	applyXAIChatHeaders(httpReq, auth, token, false, "")
	e.recordXAIRequest(ctx, auth, url, httpReq.Header.Clone(), prepared.body)

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("xai executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, errRead := io.ReadAll(httpResp.Body)
		if errRead != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errRead)
			return resp, errRead
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, data)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		return resp, xaiStatusErr(httpResp.StatusCode, data)
	}

	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	reporter.Publish(ctx, helps.ParseOpenAIUsage(data))
	reporter.EnsurePublished(ctx)

	var param any
	out := sdktranslator.TranslateNonStream(ctx, prepared.to, prepared.from, req.Model, prepared.originalPayload, prepared.body, data, &param)
	return cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}, nil
}

// executeChatModeStream routes a streaming xAI credential configured with request-mode=chat
// to the upstream OpenAI-compatible /chat/completions SSE endpoint.
func (e *XAIExecutor) executeChatModeStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	token, _ := xaiCreds(auth)
	baseURL := xaiChatBaseURL(auth)
	logXAIResolvedBaseURL(ctx, baseURL)

	prepared, err := e.prepareChatCompletionsRequest(ctx, req, opts, true)
	if err != nil {
		return nil, err
	}
	prepared.body, _ = sjson.SetBytes(prepared.body, "stream_options.include_usage", true)

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), prepared.baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)
	reporter.SetTranslatedReasoningEffort(prepared.body, e.Identifier())

	url := strings.TrimSuffix(baseURL, "/") + "/chat/completions"
	e.logXAIDiagnostic(auth, req, opts, prepared, "/chat/completions", true)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(prepared.body))
	if err != nil {
		return nil, err
	}
	applyXAIChatHeaders(httpReq, auth, token, true, "")
	httpReq.Header.Set("Cache-Control", "no-cache")
	e.recordXAIRequest(ctx, auth, url, httpReq.Header.Clone(), prepared.body)

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, errRead := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("xai executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errRead)
			return nil, errRead
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, data)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		return nil, xaiStatusErr(httpResp.StatusCode, data)
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("xai executor: close response body error: %v", errClose)
			}
			reporter.EnsurePublished(ctx)
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800)
		var param any
		for scanner.Scan() {
			line := scanner.Bytes()
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			trimmedLine := bytes.TrimSpace(line)
			if len(trimmedLine) == 0 {
				continue
			}
			if detail, ok := helps.ParseOpenAIStreamUsage(trimmedLine); ok {
				reporter.Publish(ctx, detail)
			}
			if !bytes.HasPrefix(trimmedLine, []byte("data:")) {
				if bytes.HasPrefix(trimmedLine, []byte(":")) || bytes.HasPrefix(trimmedLine, []byte("event:")) ||
					bytes.HasPrefix(trimmedLine, []byte("id:")) || bytes.HasPrefix(trimmedLine, []byte("retry:")) {
					continue
				}
				if bytes.HasPrefix(trimmedLine, []byte("{")) || bytes.HasPrefix(trimmedLine, []byte("[")) {
					streamErr := statusErr{code: http.StatusBadGateway, msg: string(trimmedLine)}
					helps.RecordAPIResponseError(ctx, e.cfg, streamErr)
					reporter.PublishFailure(ctx, streamErr)
					select {
					case out <- cliproxyexecutor.StreamChunk{Err: streamErr}:
					case <-ctx.Done():
					}
					return
				}
				continue
			}

			chunks := sdktranslator.TranslateStream(ctx, prepared.to, prepared.from, req.Model, prepared.originalPayload, prepared.body, bytes.Clone(trimmedLine), &param)
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					return
				}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.PublishFailure(ctx, errScan)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
			case <-ctx.Done():
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// countTokensChatMode estimates token count for xAI chat-mode requests using the
// OpenAI chat tokenizer.
func (e *XAIExecutor) countTokensChatMode(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	prepared, err := e.prepareChatCompletionsRequest(ctx, req, opts, false)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	enc, err := helps.TokenizerForModel(prepared.baseModel)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("xai executor: tokenizer init failed: %w", err)
	}
	count, err := helps.CountOpenAIChatTokens(enc, prepared.body)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("xai executor: token counting failed: %w", err)
	}
	usageJSON := helps.BuildOpenAIUsageJSON(count)
	translated := sdktranslator.TranslateTokenCount(ctx, prepared.to, prepared.from, count, usageJSON)
	return cliproxyexecutor.Response{Payload: translated}, nil
}

// prepareChatCompletionsRequest builds the upstream /chat/completions payload from the
// incoming request by translating it into the OpenAI chat format.
func (e *XAIExecutor) prepareChatCompletionsRequest(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) (*xaiPreparedRequest, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := sanitizeXAIChatModeSourcePayload(bytes.Clone(originalPayloadSource), from, baseModel)
	requestPayload := sanitizeXAIChatModeSourcePayload(bytes.Clone(req.Payload), from, baseModel)
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, stream)
	body := sdktranslator.TranslateRequest(from, to, baseModel, requestPayload, stream)

	var err error
	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	body = sanitizeXAIChatCompletionsBody(body, baseModel)
	body, _ = sjson.SetBytes(body, "model", baseModel)
	body, _ = sjson.SetBytes(body, "stream", stream)

	return &xaiPreparedRequest{
		baseModel:       baseModel,
		from:            from,
		to:              to,
		originalPayload: originalPayload,
		body:            body,
	}, nil
}

// xaiRequestMode resolves the effective upstream protocol for an xAI credential.
// Supported values: "responses" (default) and "chat".
func (e *XAIExecutor) xaiRequestMode(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Attributes == nil {
		return "responses"
	}
	switch strings.ToLower(strings.TrimSpace(auth.Attributes["request_mode"])) {
	case "chat":
		return "chat"
	default:
		return "responses"
	}
}

// logXAIDiagnostic emits a structured diagnostic line describing the route decision
// for an xAI request (selected request mode, model mapping, source/translated shapes).
func (e *XAIExecutor) logXAIDiagnostic(auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, prepared *xaiPreparedRequest, upstreamPath string, stream bool) {
	if prepared == nil {
		return
	}
	authID := ""
	authLabel := ""
	attrMode := ""
	metadataMode := ""
	if auth != nil {
		authID = strings.TrimSpace(auth.ID)
		authLabel = strings.TrimSpace(auth.Label)
		if auth.Attributes != nil {
			attrMode = strings.TrimSpace(auth.Attributes["request_mode"])
		}
		metadataMode = xaiMetadataString(auth.Metadata, "request_mode")
	}

	sourceBody := req.Payload
	if len(opts.OriginalRequest) > 0 {
		sourceBody = opts.OriginalRequest
	}
	sessionID := xaiExecutionSessionID(req, opts)
	sessionSummary := ""
	if sessionID != "" {
		sessionSummary = sessionID
		if len(sessionSummary) > 12 {
			sessionSummary = sessionSummary[:12] + "..."
		}
	}

	log.WithFields(log.Fields{
		"provider":                e.Identifier(),
		"auth_id":                 authID,
		"auth_label":              authLabel,
		"selected_request_mode":   e.xaiRequestMode(auth),
		"attr_request_mode":       attrMode,
		"metadata_request_mode":   metadataMode,
		"client_request_path":     helps.PayloadRequestPath(opts),
		"upstream_path":           upstreamPath,
		"source_format":           opts.SourceFormat.String(),
		"request_model":           req.Model,
		"upstream_model":          prepared.baseModel,
		"stream":                  stream,
		"source_has_input":        gjson.GetBytes(sourceBody, "input").Exists(),
		"source_has_messages":     gjson.GetBytes(sourceBody, "messages").Exists(),
		"translated_has_input":    gjson.GetBytes(prepared.body, "input").Exists(),
		"translated_has_messages": gjson.GetBytes(prepared.body, "messages").Exists(),
		"source_tool_count":       len(gjson.GetBytes(sourceBody, "tools").Array()),
		"translated_tool_count":   len(gjson.GetBytes(prepared.body, "tools").Array()),
		"prompt_cache_key":        sessionSummary,
	}).Info("xai diagnostic route decision")
}

// sanitizeXAIChatModeSourcePayload normalizes an incoming request body into a shape the
// OpenAI chat upstream accepts (tool normalization, reasoning-item normalization).
func sanitizeXAIChatModeSourcePayload(body []byte, from sdktranslator.Format, model string) []byte {
	switch from {
	case sdktranslator.FormatOpenAIResponse:
		body = normalizeXAITools(body)
		body = normalizeXAINamespaceToolChoice(body)
		body = pruneXAIOrphanedToolChoice(body)
		body = normalizeXAIToolChoiceForTools(body)
		body = normalizeXAIInputReasoningItems(body)
		body = sanitizeXAIResponsesBody(body, model)
	case sdktranslator.Format("codex"):
		body = normalizeXAITools(body)
		body = normalizeXAINamespaceToolChoice(body)
		body = pruneXAIOrphanedToolChoice(body)
		body = normalizeXAIToolChoiceForTools(body)
		body = normalizeXAIInputReasoningItems(body)
		body = sanitizeXAIResponsesBody(body, model)
	}
	return body
}

// sanitizeXAIChatCompletionsBody removes chat-incompatible fields when no tools are
// present and drops reasoning_effort for models that do not support it.
func sanitizeXAIChatCompletionsBody(body []byte, model string) []byte {
	tools := gjson.GetBytes(body, "tools")
	hasTools := tools.Exists() && tools.IsArray() && len(tools.Array()) > 0
	if !hasTools {
		if tools.Exists() {
			body, _ = sjson.DeleteBytes(body, "tools")
		}
		if gjson.GetBytes(body, "tool_choice").Exists() {
			body, _ = sjson.DeleteBytes(body, "tool_choice")
		}
		if gjson.GetBytes(body, "parallel_tool_calls").Exists() {
			body, _ = sjson.DeleteBytes(body, "parallel_tool_calls")
		}
	}
	if !xaiSupportsReasoningEffort(model) && gjson.GetBytes(body, "reasoning_effort").Exists() {
		body, _ = sjson.DeleteBytes(body, "reasoning_effort")
	}
	return body
}

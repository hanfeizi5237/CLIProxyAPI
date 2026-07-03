package auth

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestLookupAPIKeyUpstreamModel(t *testing.T) {
	cfg := &internalconfig.Config{
		GeminiKey: []internalconfig.GeminiKey{
			{
				APIKey:  "k",
				BaseURL: "https://example.com",
				Models: []internalconfig.GeminiModel{
					{Name: "gemini-2.5-pro-exp-03-25", Alias: "g25p"},
					{Name: "gemini-2.5-flash(low)", Alias: "g25f"},
				},
			},
		},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	_, _ = mgr.Register(ctx, &Auth{ID: "a1", Provider: "gemini", Attributes: map[string]string{"api_key": "k", "base_url": "https://example.com"}})

	tests := []struct {
		name   string
		authID string
		input  string
		want   string
	}{
		// Fast path + suffix preservation
		{"alias with suffix", "a1", "g25p(8192)", "gemini-2.5-pro-exp-03-25(8192)"},
		{"alias without suffix", "a1", "g25p", "gemini-2.5-pro-exp-03-25"},

		// Config suffix takes priority
		{"config suffix priority", "a1", "g25f(high)", "gemini-2.5-flash(low)"},
		{"config suffix no user suffix", "a1", "g25f", "gemini-2.5-flash(low)"},

		// Case insensitive
		{"uppercase alias", "a1", "G25P", "gemini-2.5-pro-exp-03-25"},
		{"mixed case with suffix", "a1", "G25p(4096)", "gemini-2.5-pro-exp-03-25(4096)"},

		// Direct name lookup
		{"upstream name direct", "a1", "gemini-2.5-pro-exp-03-25", "gemini-2.5-pro-exp-03-25"},
		{"upstream name with suffix", "a1", "gemini-2.5-pro-exp-03-25(8192)", "gemini-2.5-pro-exp-03-25(8192)"},

		// Cache miss scenarios
		{"non-existent auth", "non-existent", "g25p", ""},
		{"unknown alias", "a1", "unknown-alias", ""},
		{"empty auth ID", "", "g25p", ""},
		{"empty model", "a1", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved := mgr.lookupAPIKeyUpstreamModel(tt.authID, tt.input)
			if resolved != tt.want {
				t.Errorf("lookupAPIKeyUpstreamModel(%q, %q) = %q, want %q", tt.authID, tt.input, resolved, tt.want)
			}
		})
	}
}

func TestAPIKeyModelAlias_ConfigHotReload(t *testing.T) {
	cfg := &internalconfig.Config{
		GeminiKey: []internalconfig.GeminiKey{
			{
				APIKey: "k",
				Models: []internalconfig.GeminiModel{{Name: "gemini-2.5-pro-exp-03-25", Alias: "g25p"}},
			},
		},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	_, _ = mgr.Register(ctx, &Auth{ID: "a1", Provider: "gemini", Attributes: map[string]string{"api_key": "k"}})

	// Initial alias
	if resolved := mgr.lookupAPIKeyUpstreamModel("a1", "g25p"); resolved != "gemini-2.5-pro-exp-03-25" {
		t.Fatalf("before reload: got %q, want %q", resolved, "gemini-2.5-pro-exp-03-25")
	}

	// Hot reload with new alias
	mgr.SetConfig(&internalconfig.Config{
		GeminiKey: []internalconfig.GeminiKey{
			{
				APIKey: "k",
				Models: []internalconfig.GeminiModel{{Name: "gemini-2.5-flash", Alias: "g25p"}},
			},
		},
	})

	// New alias should take effect
	if resolved := mgr.lookupAPIKeyUpstreamModel("a1", "g25p"); resolved != "gemini-2.5-flash" {
		t.Fatalf("after reload: got %q, want %q", resolved, "gemini-2.5-flash")
	}
}

func TestAPIKeyModelAlias_MultipleProviders(t *testing.T) {
	cfg := &internalconfig.Config{
		GeminiKey: []internalconfig.GeminiKey{{APIKey: "gemini-key", Models: []internalconfig.GeminiModel{{Name: "gemini-2.5-pro", Alias: "gp"}}}},
		ClaudeKey: []internalconfig.ClaudeKey{{APIKey: "claude-key", Models: []internalconfig.ClaudeModel{{Name: "claude-sonnet-4", Alias: "cs4"}}}},
		CodexKey:  []internalconfig.CodexKey{{APIKey: "codex-key", Models: []internalconfig.CodexModel{{Name: "o3", Alias: "o"}}}},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	_, _ = mgr.Register(ctx, &Auth{ID: "gemini-auth", Provider: "gemini", Attributes: map[string]string{"api_key": "gemini-key"}})
	_, _ = mgr.Register(ctx, &Auth{ID: "claude-auth", Provider: "claude", Attributes: map[string]string{"api_key": "claude-key"}})
	_, _ = mgr.Register(ctx, &Auth{ID: "codex-auth", Provider: "codex", Attributes: map[string]string{"api_key": "codex-key"}})

	tests := []struct {
		authID, input, want string
	}{
		{"gemini-auth", "gp", "gemini-2.5-pro"},
		{"claude-auth", "cs4", "claude-sonnet-4"},
		{"codex-auth", "o", "o3"},
	}

	for _, tt := range tests {
		if resolved := mgr.lookupAPIKeyUpstreamModel(tt.authID, tt.input); resolved != tt.want {
			t.Errorf("lookupAPIKeyUpstreamModel(%q, %q) = %q, want %q", tt.authID, tt.input, resolved, tt.want)
		}
	}
}

func TestApplyAPIKeyModelAlias(t *testing.T) {
	cfg := &internalconfig.Config{
		GeminiKey: []internalconfig.GeminiKey{
			{APIKey: "k", Models: []internalconfig.GeminiModel{{Name: "gemini-2.5-pro-exp-03-25", Alias: "g25p"}}},
		},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	apiKeyAuth := &Auth{ID: "a1", Provider: "gemini", Attributes: map[string]string{"api_key": "k"}}
	oauthAuth := &Auth{ID: "oauth-auth", Provider: "claude", Attributes: map[string]string{"auth_kind": "oauth"}}
	_, _ = mgr.Register(ctx, apiKeyAuth)

	tests := []struct {
		name       string
		auth       *Auth
		inputModel string
		wantModel  string
	}{
		{
			name:       "api_key auth with alias",
			auth:       apiKeyAuth,
			inputModel: "g25p(8192)",
			wantModel:  "gemini-2.5-pro-exp-03-25(8192)",
		},
		{
			name:       "oauth auth passthrough",
			auth:       oauthAuth,
			inputModel: "some-model",
			wantModel:  "some-model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolvedModel := mgr.applyAPIKeyModelAlias(tt.auth, tt.inputModel)

			if resolvedModel != tt.wantModel {
				t.Errorf("model = %q, want %q", resolvedModel, tt.wantModel)
			}
		})
	}
}

func TestResolveAPIKeyModelAliasWithResult_ForceMapping(t *testing.T) {
	cfg := &internalconfig.Config{
		ClaudeKey: []internalconfig.ClaudeKey{{
			APIKey: "claude-key",
			Models: []internalconfig.ClaudeModel{{
				Name:         "glm-5.2",
				Alias:        "claude-sonnet-latest",
				ForceMapping: true,
			}},
		}},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	auth := &Auth{ID: "claude-auth", Provider: "claude", Attributes: map[string]string{"api_key": "claude-key"}}
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	result := mgr.resolveAPIKeyModelAliasWithResult(auth, "claude-sonnet-latest")
	if result.UpstreamModel != "glm-5.2" || !result.ForceMapping || result.OriginalAlias != "claude-sonnet-latest" {
		t.Fatalf("resolveAPIKeyModelAliasWithResult() = %+v, want upstream glm-5.2 with force mapping", result)
	}

	noRewrite := mgr.resolveAPIKeyModelAliasWithResult(auth, "glm-5.2")
	if noRewrite.UpstreamModel != "glm-5.2" || noRewrite.ForceMapping || noRewrite.OriginalAlias != "" {
		t.Fatalf("resolveAPIKeyModelAliasWithResult() direct upstream = %+v, want passthrough without rewrite", noRewrite)
	}
}

func TestResolveAPIKeyModelAliasWithResult_SameBasePreservesSuffix(t *testing.T) {
	cfg := &internalconfig.Config{
		GeminiKey: []internalconfig.GeminiKey{{
			APIKey: "k",
			Models: []internalconfig.GeminiModel{{
				Name:         "gemini-2.5-pro",
				Alias:        "gemini-2.5-pro(8192)",
				ForceMapping: true,
			}},
		}},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	auth := &Auth{ID: "gemini-auth", Provider: "gemini", Attributes: map[string]string{"api_key": "k"}}
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	result := mgr.resolveAPIKeyModelAliasWithResult(auth, "gemini-2.5-pro(8192)")
	if result.UpstreamModel != "gemini-2.5-pro(8192)" || !result.ForceMapping || result.OriginalAlias != "gemini-2.5-pro(8192)" {
		t.Fatalf("resolveAPIKeyModelAliasWithResult() = %+v, want same-base suffix preserved", result)
	}
}

func TestResolveAPIKeyModelAliasWithResult_ForceMappingUsesConfigAliasNotRequestSuffix(t *testing.T) {
	cfg := &internalconfig.Config{
		CodexKey: []internalconfig.CodexKey{{
			APIKey: "codex-key",
			Models: []internalconfig.CodexModel{{
				Name:         "gpt-5.5",
				Alias:        "claude-sonnet-4-5",
				ForceMapping: true,
			}},
		}},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	auth := &Auth{ID: "codex-auth", Provider: "codex", Attributes: map[string]string{"api_key": "codex-key"}}
	if _, err := mgr.Register(ctx, auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	result := mgr.resolveAPIKeyModelAliasWithResult(auth, "claude-sonnet-4-5(high)")
	if result.UpstreamModel != "gpt-5.5(high)" {
		t.Fatalf("upstream = %q want gpt-5.5(high)", result.UpstreamModel)
	}
	if result.OriginalAlias != "claude-sonnet-4-5" {
		t.Fatalf("OriginalAlias = %q want claude-sonnet-4-5", result.OriginalAlias)
	}
}

func TestManager_CodexAPIKeyAliasUsesUpstreamModelForAvailability(t *testing.T) {
	cfg := &internalconfig.Config{
		CodexKey: []internalconfig.CodexKey{
			{
				APIKey:      "codex-key",
				RequestMode: "chat",
				Models: []internalconfig.CodexModel{
					{Name: "qwen3.6-plus", Alias: "gpt-5.4"},
				},
			},
		},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)
	executor := &openAICompatPoolExecutor{id: "codex"}
	mgr.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "codex-auth",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":      "codex-key",
			"request_mode": "chat",
		},
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "gpt-5.4"}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth.ID)
	})

	ctx := context.Background()
	if _, errRegister := mgr.Register(ctx, auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	if got := mgr.selectionModelForAuth(auth, "gpt-5.4"); got != "qwen3.6-plus" {
		t.Fatalf("selection model = %q, want qwen3.6-plus", got)
	}

	mgr.MarkResult(ctx, Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "gpt-5.4",
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusNotFound,
			Message:    "model gpt-5.4 not found",
		},
	})
	current, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatalf("auth %s not found after mark result", auth.ID)
	}
	if got := mgr.selectionModelForAuth(current, "gpt-5.4"); got != "qwen3.6-plus" {
		t.Fatalf("current selection model = %q, want qwen3.6-plus", got)
	}
	if blocked, reason, _ := isAuthBlockedForModel(current, "qwen3.6-plus", time.Now()); blocked {
		t.Fatalf("current auth blocked for upstream model: reason=%v state=%#v", reason, current.ModelStates)
	}
	picked, _, errPick := mgr.pickNextLegacy(ctx, "codex", "gpt-5.4", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("legacy pick auth with aliased model: %v", errPick)
	}
	if picked == nil || picked.ID != auth.ID {
		t.Fatalf("picked auth = %#v, want %s", picked, auth.ID)
	}
	if got := mgr.prepareExecutionModels(picked, "gpt-5.4"); len(got) != 1 || got[0] != "qwen3.6-plus" {
		t.Fatalf("prepared models = %v, want [qwen3.6-plus]", got)
	}

	resp, errExecute := mgr.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("execute with aliased model: %v", errExecute)
	}
	if got := string(resp.Payload); got != "qwen3.6-plus" {
		t.Fatalf("execute payload = %q, want upstream model qwen3.6-plus", got)
	}

	gotModels := executor.ExecuteModels()
	if len(gotModels) != 1 {
		t.Fatalf("execute models = %v, want one upstream call", gotModels)
	}
	if gotModels[0] != "qwen3.6-plus" {
		t.Fatalf("execute model = %q, want qwen3.6-plus", gotModels[0])
	}
}

func TestManager_CodexAPIKeyAliasDoesNotBypassAuthWideCooldown(t *testing.T) {
	cfg := &internalconfig.Config{
		CodexKey: []internalconfig.CodexKey{
			{
				APIKey:      "codex-key-auth-wide",
				RequestMode: "chat",
				Models: []internalconfig.CodexModel{
					{Name: "qwen3.6-plus", Alias: "gpt-5.4"},
				},
			},
		},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)
	executor := &openAICompatPoolExecutor{id: "codex"}
	mgr.RegisterExecutor(executor)

	auth := &Auth{
		ID:       "codex-auth-wide",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":      "codex-key-auth-wide",
			"request_mode": "chat",
		},
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "gpt-5.4"}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth.ID)
	})

	ctx := context.Background()
	if _, errRegister := mgr.Register(ctx, auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	mgr.MarkResult(ctx, Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusUnauthorized,
			Message:    "unauthorized",
		},
	})

	_, errExecute := mgr.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{})
	if errExecute == nil {
		t.Fatal("expected auth-wide cooldown to block aliased request")
	}
	if got := executor.ExecuteModels(); len(got) != 0 {
		t.Fatalf("execute models = %v, want no upstream call while auth is cooling down", got)
	}
}

func TestResolveCodexAPIKeyConfig_PrefersExactBaseURLWithSharedAPIKey(t *testing.T) {
	cfg := &internalconfig.Config{
		CodexKey: []internalconfig.CodexKey{
			{
				APIKey:  "shared-key",
				BaseURL: "https://api.openai.com/v1",
				Models: []internalconfig.CodexModel{
					{Name: "gpt-5.4", Alias: "gpt-5.4"},
				},
			},
			{
				APIKey:  "shared-key",
				BaseURL: "https://coding.dashscope.aliyuncs.com/v1",
				Models: []internalconfig.CodexModel{
					{Name: "qwen3.6-plus", Alias: "gpt-5.4"},
				},
			},
		},
	}

	auth := &Auth{
		ID:       "shared-auth",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "shared-key",
			"base_url": "https://coding.dashscope.aliyuncs.com/v1/",
		},
	}

	entry := resolveCodexAPIKeyConfig(cfg, auth)
	if entry == nil {
		t.Fatal("resolveCodexAPIKeyConfig returned nil")
	}
	if got := strings.TrimSpace(entry.BaseURL); got != "https://coding.dashscope.aliyuncs.com/v1" {
		t.Fatalf("resolved base_url = %q, want https://coding.dashscope.aliyuncs.com/v1", got)
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)
	upstream := mgr.applyAPIKeyModelAlias(auth, "gpt-5.4")
	if upstream != "qwen3.6-plus" {
		t.Fatalf("applyAPIKeyModelAlias() = %q, want qwen3.6-plus", upstream)
	}
}

func TestResolveCodexAPIKeyConfig_SharedAPIKeyNoBaseURLMatchReturnsNil(t *testing.T) {
	cfg := &internalconfig.Config{
		CodexKey: []internalconfig.CodexKey{
			{APIKey: "shared-key", BaseURL: "https://api.openai.com/v1"},
			{APIKey: "shared-key", BaseURL: "https://coding.dashscope.aliyuncs.com/v1"},
		},
	}
	auth := &Auth{
		ID:       "shared-auth-no-match",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "shared-key",
			"base_url": "https://other.example.com/v1",
		},
	}

	entry := resolveCodexAPIKeyConfig(cfg, auth)
	if entry != nil {
		t.Fatalf("resolveCodexAPIKeyConfig should return nil for ambiguous shared key, got base_url=%q", entry.BaseURL)
	}
}

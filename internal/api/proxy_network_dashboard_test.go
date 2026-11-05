package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestProxyNetworkDashboardRoute(t *testing.T) {
	for _, tc := range []struct {
		name     string
		disabled bool
		want     int
	}{
		{name: "enabled", want: http.StatusOK},
		{name: "disabled", disabled: true, want: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.RemoteManagement.DisableControlPanel = tc.disabled
			server := newTestServerWithConfig(t, cfg)
			req := httptest.NewRequest(http.MethodGet, "/proxy-network", nil)
			rec := httptest.NewRecorder()
			server.engine.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if tc.want == http.StatusOK {
				if got := rec.Header().Get("Cache-Control"); got != "no-store" {
					t.Fatalf("Cache-Control = %q", got)
				}
				if !strings.Contains(rec.Body.String(), "/v8/management/requests/api-call") {
					t.Fatal("dashboard does not use the authenticated v8 management API")
				}
			}
		})
	}
}

func TestProxyNetworkDashboardUsesAuthenticatedManagementCall(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		if r.URL.Path != "/proxies" || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected upstream request: path=%q has authorization=%t", r.URL.Path, r.Header.Get("Authorization") != "")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"proxies":{}}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{}
	cfg.RemoteManagement.SecretKey = "test-configured-secret"
	server := newTestServerWithConfig(t, cfg, WithLocalManagementPassword("test-management-key"))
	body := fmt.Sprintf(`{"method":"GET","url":%q,"proxy_url":"direct"}`, upstream.URL+"/proxies")
	for _, tc := range []struct {
		name       string
		credential string
		want       int
	}{
		{name: "unauthenticated", want: http.StatusUnauthorized},
		{name: "authorized", credential: "test-management-key", want: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v8/management/requests/api-call", strings.NewReader(body))
			req.RemoteAddr = "127.0.0.1:1234"
			req.Header.Set("Content-Type", "application/json")
			if tc.credential != "" {
				req.Header.Set("Authorization", "Bearer "+tc.credential)
			}
			rec := httptest.NewRecorder()
			server.engine.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstream called %d times, want exactly one authorized request", upstreamCalls)
	}
}

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enowdev/antares/internal/agent"
	"github.com/enowdev/antares/internal/config"
)

func TestTerminalSetupPersistsNamedProvider(t *testing.T) {
	t.Setenv("ANTARES_HOME", t.TempDir())
	var receivedHeaders []string

	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Tenant") != "team=a=b" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		receivedHeaders = append(receivedHeaders, r.Header.Get("X-Tenant"))
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"data":   []map[string]string{{"id": "header-model", "owned_by": "test"}},
			})
		case "/v1/chat/completions":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":      "chatcmpl-test",
				"object":  "chat.completion",
				"created": 0,
				"model":   "header-model",
				"choices": []map[string]any{{
					"index":         0,
					"message":       map[string]string{"role": "assistant", "content": "pong"},
					"finish_reason": "stop",
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fixture.Close)

	cfg := config.Default()
	cfg.Agent.Workspace = filepath.Join(t.TempDir(), "workspace")
	cfg.Model.MaxRetries = -1
	cfg.Providers["custom"] = config.Provider{
		Enabled: true,
		Kind:    "openai-compatible",
		BaseURL: "https://legacy.example/v1",
		Label:   "Legacy custom",
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	a := agent.New(cfg, nil, nil, nil, nil)
	rt := &runtimeServices{cfg: cfg, agent: a}
	oldReader := stdinReader
	stdinReader = bufio.NewReader(strings.NewReader(strings.Join([]string{
		"7",                 // Custom provider
		"Named Provider",    // provider name
		fixture.URL + "/v1", // endpoint
		"X-Tenant=team=a=b", // accepted header
		"bad header=value",  // malformed header
		"x-tenant=other",    // duplicate canonical header
		"",                  // header terminator
		"1",                 // first live model or manual fallback
		"",                  // workspace
		"",                  // PostgreSQL
		"",                  // RAG
		"",                  // Telegram
		"",                  // dashboard password
	}, "\n") + "\n"))
	t.Cleanup(func() { stdinReader = oldReader })

	var setupErr error
	output := captureProviderStdout(t, func() {
		setupErr = runTerminalSetup(context.Background(), rt)
	})
	if setupErr != nil {
		t.Fatalf("run terminal setup: %v\noutput:\n%s", setupErr, output)
	}
	after, err := config.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if after.Model.Provider != "named-provider" {
		t.Fatalf("model provider = %q, want named-provider", after.Model.Provider)
	}
	got, ok := after.Providers[after.Model.Provider]
	if !ok {
		t.Fatalf("named provider %q was not saved", after.Model.Provider)
	}
	if got.BaseURL != fixture.URL+"/v1" {
		t.Fatalf("named provider endpoint = %q, want %q", got.BaseURL, fixture.URL+"/v1")
	}
	legacy := after.Providers["custom"]
	if legacy.BaseURL != "https://legacy.example/v1" || legacy.Label != "Legacy custom" {
		t.Fatalf("legacy custom provider changed: %#v", legacy)
	}
	if _, resolved := after.ResolveProvider(after.Model.Provider); resolved.BaseURL != fixture.URL+"/v1" {
		t.Fatalf("resolved provider endpoint = %q, want %q", resolved.BaseURL, fixture.URL+"/v1")
	}
	if got.Headers["X-Tenant"] != "team=a=b" || len(got.Headers) != 1 {
		t.Fatalf("named provider headers = %#v", got.Headers)
	}
	if len(receivedHeaders) < 2 {
		t.Fatalf("provider probes = %d, want model list and final chat", len(receivedHeaders))
	}
	for _, header := range receivedHeaders {
		if header != "team=a=b" {
			t.Fatalf("probe header = %q, want accepted value", header)
		}
	}
}

func TestPromptProviderHeadersAllowsInitialBlank(t *testing.T) {
	oldReader := stdinReader
	stdinReader = bufio.NewReader(strings.NewReader("\n"))
	t.Cleanup(func() { stdinReader = oldReader })

	if headers := promptProviderHeaders(); headers == nil || len(headers) != 0 {
		t.Fatalf("initial blank headers = %#v, want allocated empty map", headers)
	}
}

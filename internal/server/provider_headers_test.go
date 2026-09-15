package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enowdev/antares/internal/agent"
	"github.com/enowdev/antares/internal/config"
	"github.com/enowdev/antares/internal/store"
)

func headerProviderFixture(t *testing.T) (*httptest.Server, *string) {
	t.Helper()
	expected := "team=a=b"
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("X-Tenant") != expected {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"header-model","owned_by":"test"}]}`))
	}))
	t.Cleanup(fixture.Close)
	return fixture, &expected
}

func providerJSONRequest(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func assertProviderOK(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if ok, _ := response["ok"].(bool); !ok {
		t.Fatalf("response ok = %v, body = %s", response["ok"], rr.Body.String())
	}
	return response
}

func TestProviderHeadersCreateAndReconnect(t *testing.T) {
	fixture, expected := headerProviderFixture(t)
	s := newProviderKeyServer(t, func(*config.Config) {})

	rr := httptest.NewRecorder()
	s.handleCreateProvider(rr, providerJSONRequest(http.MethodPost, "/api/providers", `{"name":"Header Gateway","base_url":"`+fixture.URL+`/v1","headers":{"X-Tenant":"team=a=b"}}`))
	response := assertProviderOK(t, rr)
	id := response["id"].(string)

	reloaded, err := config.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Providers[id].Headers["X-Tenant"]; got != *expected {
		t.Fatalf("saved header = %q, want %q", got, *expected)
	}

	rr = postProviderKey(s, id, `{"api_key":"","base_url":"`+fixture.URL+`/v1"}`)
	assertProviderOK(t, rr)
}

func TestProviderHeadersRejectInvalidAndPreserveSettings(t *testing.T) {
	fixture, _ := headerProviderFixture(t)
	s := newProviderKeyServer(t, func(cfg *config.Config) {
		cfg.Providers["gateway"] = config.Provider{
			Kind: "openai-compatible", BaseURL: fixture.URL + "/v1", Enabled: true,
			Headers: map[string]string{"X-Old": "old"},
		}
	})

	for _, body := range []string{
		`{"headers":{"bad header":"value"}}`,
		"{\"headers\":{\"X-Test\":\"bad\\nvalue\"}}",
		`{"headers":{"X-Test":"one","x-test":"two"}}`,
	} {
		rr := httptest.NewRecorder()
		r := providerJSONRequest(http.MethodPatch, "/api/providers/gateway/settings", body)
		r.SetPathValue("id", "gateway")
		s.handleProviderSettings(rr, r)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("invalid headers status = %d, body = %s", rr.Code, rr.Body.String())
		}
		reloaded, err := config.Reload()
		if err != nil {
			t.Fatal(err)
		}
		if got := reloaded.Providers["gateway"].Headers["X-Old"]; got != "old" {
			t.Fatalf("invalid headers changed saved headers: %#v", reloaded.Providers["gateway"].Headers)
		}
	}
}

func TestProviderHeadersSettingsReplacePreserveAndClear(t *testing.T) {
	s := newProviderKeyServer(t, func(cfg *config.Config) {
		cfg.Providers["gateway"] = config.Provider{Headers: map[string]string{"X-Old": "old"}}
	})
	request := func(body string) {
		t.Helper()
		rr := httptest.NewRecorder()
		r := providerJSONRequest(http.MethodPatch, "/api/providers/gateway/settings", body)
		r.SetPathValue("id", "gateway")
		s.handleProviderSettings(rr, r)
		assertProviderOK(t, rr)
	}

	request(`{"label":"Gateway"}`)
	reloaded, err := config.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Providers["gateway"].Headers["X-Old"]; got != "old" {
		t.Fatalf("omitted headers = %#v, want existing map", reloaded.Providers["gateway"].Headers)
	}

	request(`{"headers":{"x-new":" new "}}`)
	reloaded, err = config.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Providers["gateway"].Headers; len(got) != 1 || got["X-New"] != "new" {
		t.Fatalf("replacement headers = %#v", got)
	}

	request(`{"headers":{}}`)
	reloaded, err = config.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Providers["gateway"].Headers; got == nil || len(got) != 0 {
		t.Fatalf("cleared headers = %#v, want allocated empty map", got)
	}
}

func TestProviderHeadersSetupTestAndComplete(t *testing.T) {
	fixture, expected := headerProviderFixture(t)
	home := t.TempDir()
	t.Setenv("ANTARES_HOME", home)
	cfg := config.Default()
	cfg.Model.Default = ""
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), "sqlite", filepath.Join(t.TempDir(), "test.db"), 1, 5000, false)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: cfg, db: db, agent: agent.New(cfg, db, nil, nil, nil)}

	testRequest := providerJSONRequest(http.MethodPost, "/api/setup/test", `{"provider":"custom","base_url":"`+fixture.URL+`/v1","headers":{"X-Tenant":"team=a=b"}}`)
	testRequest.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	s.handleSetupTest(rr, testRequest)
	assertProviderOK(t, rr)

	completeRequest := providerJSONRequest(http.MethodPost, "/api/setup/complete", `{"provider":"custom","name":"Header Setup","base_url":"`+fixture.URL+`/v1","headers":{"X-Tenant":"team=a=b"},"model":"header-model","workspace":"`+filepath.Join(t.TempDir(), "workspace")+`"}`)
	completeRequest.RemoteAddr = "127.0.0.1:12345"
	rr = httptest.NewRecorder()
	s.handleSetupComplete(rr, completeRequest)
	assertProviderOK(t, rr)

	reloaded, err := config.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Providers["header-setup"].Headers["X-Tenant"]; got != *expected {
		t.Fatalf("completed headers = %q, want %q", got, *expected)
	}
}

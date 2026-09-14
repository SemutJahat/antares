package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enowdev/antares/internal/agent"
	"github.com/enowdev/antares/internal/config"
	"github.com/enowdev/antares/internal/store"
	"github.com/enowdev/antares/internal/tools"
)

func runtimeServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	t.Setenv("ANTARES_HOME", t.TempDir())
	t.Setenv("ANTARES_CONFIG", "")
	for _, key := range []string{"ANTARES_HOST", "ANTARES_PORT", "ANTARES_MODEL", "ANTARES_PROVIDER", "ANTARES_AUTH_TOKEN", "ANTARES_AUTH_DISABLED", "ANTARES_API_KEY", "ANTARES_BASE_URL", "DATABASE_URL"} {
		t.Setenv(key, "")
	}
	cfg := config.Default()
	cfg.Server.AuthToken = "fixture-token"
	cfg.MaxConcurrentSessions = 1
	cfg.Agent.SmartTitles = false
	cfg.RAG.Enabled = false
	cfg.Plugins.Enabled = false
	cfg.Providers = map[string]config.Provider{}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), "sqlite", filepath.Join(config.Home(), "fixture.db"), 1, 5000, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a := agent.New(cfg, db, tools.NewRegistry(), nil, nil)
	return New(Options{Config: cfg, Agent: a, Store: db}), db
}

func runtimeRequest(s *Server, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer fixture-token")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestChatAdmissionRejectsBeforePersistence(t *testing.T) {
	s, db := runtimeServer(t)
	held, err := s.agent.Prepare(context.Background(), agent.Request{SessionID: "held"})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"message":"new turn"}`, http.StatusTooManyRequests},
		{`{"session_id":"held","message":"duplicate"}`, http.StatusConflict},
	} {
		w := runtimeRequest(s, "POST", "/api/chat", tc.body)
		if w.Code != tc.status {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		if strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
			t.Fatal("rejection started SSE")
		}
	}
	stats, err := db.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sessions != 0 || stats.Messages != 0 {
		t.Fatalf("rejected turn persisted: %+v", stats)
	}
	held.Close()
	w := runtimeRequest(s, "POST", "/api/chat", `{"message":"missing provider"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"type":"error"`) {
		t.Fatalf("admitted error response: %d %s", w.Code, w.Body.String())
	}
	next, err := s.agent.Prepare(context.Background(), agent.Request{SessionID: "next"})
	if err != nil {
		t.Fatalf("error leaked admission: %v", err)
	}
	next.Close()
}

func TestConfigDesiredPersistsAcrossLiveModelChange(t *testing.T) {
	s, _ := runtimeServer(t)
	old := s.config().Database.DSN
	desired := filepath.Join(config.Home(), "next.db")
	body, _ := json.Marshal(map[string]any{"updates": map[string]any{"database.dsn": desired, "model.default": "next-model"}})
	w := runtimeRequest(s, "POST", "/api/config", string(body))
	if w.Code != 200 {
		t.Fatalf("save: %s", w.Body.String())
	}
	var saved struct {
		RestartRequired bool     `json:"restart_required"`
		RestartFields   []string `json:"restart_fields"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.RestartRequired || len(saved.RestartFields) != 1 || saved.RestartFields[0] != "database.dsn" {
		t.Fatalf("missing pending restart: %+v", saved)
	}
	if s.config().Database.DSN != old || s.config().Model.Default != "next-model" {
		t.Fatal("effective configuration incorrect")
	}
	w = runtimeRequest(s, "POST", "/api/model/set", `{"model":"third-model"}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	got, err := config.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if got.Database.DSN != desired || got.Model.Default != "third-model" {
		t.Fatal("live save discarded desired restart settings")
	}
	if s.config().Database.DSN != old {
		t.Fatal("model switch applied database without restart")
	}
}

func TestPasswordSessionAuthenticatesAlongsideBearer(t *testing.T) {
	s, _ := runtimeServer(t)
	cfg := s.config().Clone()
	hash, err := config.HashPassword("fixture-password")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Server.DashboardPasswordHash = hash
	s.SetConfig(cfg)
	request := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"password":"fixture-password"}`))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, request)
	if w.Code != 200 || len(w.Result().Cookies()) == 0 {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	cookie := w.Result().Cookies()[0]
	request = httptest.NewRequest("GET", "/api/config", nil)
	request.AddCookie(cookie)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, request)
	if w.Code != 200 {
		t.Fatalf("cookie with configured bearer rejected: %d", w.Code)
	}
	request = httptest.NewRequest("GET", "/api/config", nil)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, request)
	if w.Code != 401 {
		t.Fatalf("unauthenticated status=%d", w.Code)
	}
}

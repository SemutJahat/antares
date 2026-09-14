package config

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestValidateListen(t *testing.T) {
	// A bcrypt-shaped hash. DashboardLocked only checks for a non-empty
	// trimmed string; using a syntactically real hash keeps the fixture honest.
	hash, err := HashPassword("hunter2")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	cases := []struct {
		name    string
		server  Server
		wantErr bool
	}{
		{"empty host is loopback", Server{}, false},
		{"ipv4 loopback unprotected", Server{Host: "127.0.0.1"}, false},
		{"ipv4 loopback alt octet unprotected", Server{Host: "127.5.6.7"}, false},
		{"ipv6 loopback unprotected", Server{Host: "::1"}, false},
		{"ipv6 loopback bracketed", Server{Host: "[::1]"}, false},
		{"whitespace host is loopback", Server{Host: "  "}, false},

		{"wildcard bare unprotected rejected", Server{Host: "0.0.0.0"}, true},
		{"ipv6 wildcard unprotected rejected", Server{Host: "::"}, true},
		{"private lan unprotected rejected", Server{Host: "192.168.1.10"}, true},
		{"rfc1918 10 space unprotected rejected", Server{Host: "10.0.0.5"}, true},
		{"global unprotected rejected", Server{Host: "8.8.8.8"}, true},

		{"wildcard with bearer token accepted", Server{Host: "0.0.0.0", AuthToken: "s3cret"}, false},
		{"wildcard with whitespace-only token still rejected", Server{Host: "0.0.0.0", AuthToken: "   "}, true},
		{"public bind with dashboard password accepted", Server{Host: "8.8.8.8", DashboardPasswordHash: hash}, false},
		{"public bind with explicit AuthDisabled accepted", Server{Host: "203.0.113.4", AuthDisabled: true}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.server.ValidateListen()
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %+v", tc.server)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %+v: %v", tc.server, err)
			}
		})
	}
}

// Effective must hold restart-bound fields at their currently-running values
// while letting live fields flow through, without mutating either input.
func TestEffectiveHoldsRestartFields(t *testing.T) {
	current := Default()
	current.Database.DSN = "/var/lib/antares/live.db"
	current.Database.Driver = "sqlite"
	current.Server.Host = "127.0.0.1"
	current.Server.Port = 8787
	current.Model.Default = "gpt-live"

	desired := current.Clone()
	desired.Database.DSN = "/var/lib/antares/next.db" // restart-required
	desired.Database.Driver = "postgres"              // restart-required
	desired.Server.Host = "0.0.0.0"                   // restart-required
	desired.Server.Port = 9090                        // restart-required
	desired.Model.Default = "gpt-next"                // live

	beforeCurrent := current.Clone()
	beforeDesired := desired.Clone()

	next, pending := Effective(current, desired)

	if next.Database.DSN != current.Database.DSN {
		t.Errorf("database.dsn: want held at %q, got %q", current.Database.DSN, next.Database.DSN)
	}
	if next.Database.Driver != current.Database.Driver {
		t.Errorf("database.driver: want held at %q, got %q", current.Database.Driver, next.Database.Driver)
	}
	if next.Server.Host != current.Server.Host {
		t.Errorf("server.host: want held at %q, got %q", current.Server.Host, next.Server.Host)
	}
	if next.Server.Port != current.Server.Port {
		t.Errorf("server.port: want held at %d, got %d", current.Server.Port, next.Server.Port)
	}
	if next.Model.Default != desired.Model.Default {
		t.Errorf("model.default is live; want %q, got %q", desired.Model.Default, next.Model.Default)
	}

	wantPending := map[string]bool{
		"database.dsn":    true,
		"database.driver": true,
		"server.host":     true,
		"server.port":     true,
	}
	if len(pending) != len(wantPending) {
		t.Errorf("pending length: want %d, got %d (%v)", len(wantPending), len(pending), pending)
	}
	for _, p := range pending {
		if !wantPending[p] {
			t.Errorf("unexpected pending path %q", p)
		}
		delete(wantPending, p)
	}
	for missing := range wantPending {
		t.Errorf("missing pending path %q", missing)
	}

	if !reflect.DeepEqual(current, beforeCurrent) {
		t.Error("Effective mutated the current config")
	}
	if !reflect.DeepEqual(desired, beforeDesired) {
		t.Error("Effective mutated the desired config")
	}
}

// The cloned effective view must own its maps: mutating a map on the returned
// config must never leak into the caller's inputs.
func TestEffectiveClonesMapsIndependently(t *testing.T) {
	current := Default()
	desired := current.Clone()
	desired.Providers["fresh"] = Provider{Kind: "openai-compatible", BaseURL: "https://example"}

	next, _ := Effective(current, desired)

	next.Providers["fresh"] = Provider{Kind: "mutated"}
	next.Providers["injected"] = Provider{Kind: "sneak"}

	if got := desired.Providers["fresh"].Kind; got != "openai-compatible" {
		t.Errorf("desired.Providers mutated via effective clone: got kind %q", got)
	}
	if _, leaked := desired.Providers["injected"]; leaked {
		t.Error("new key on effective leaked into desired.Providers")
	}
	if _, leaked := current.Providers["fresh"]; leaked {
		t.Error("desired-only provider bled into current.Providers")
	}
}

// A desired change that matches the running value is not a change at all: no
// pending entry should be produced for a field that has been restored.
func TestEffectiveNoPendingWhenDesiredMatchesCurrent(t *testing.T) {
	current := Default()
	current.Database.DSN = "/live.db"
	desired := current.Clone()
	desired.Database.DSN = "/next.db"

	if _, pending := Effective(current, desired); len(pending) == 0 {
		t.Fatal("baseline mismatch: expected pending before restoration")
	}

	desired.Database.DSN = current.Database.DSN
	next, pending := Effective(current, desired)

	for _, p := range pending {
		if strings.HasPrefix(p, "database.") {
			t.Errorf("restoring desired should clear pending, still has %q", p)
		}
	}
	if next.Database.DSN != current.Database.DSN {
		t.Errorf("database.dsn: want %q, got %q", current.Database.DSN, next.Database.DSN)
	}
}

// yaml.Unmarshal on top of Default() overlays only fields the user specified;
// unrelated defaults must survive. This is how Reload merges profile YAML.
func TestYAMLUnmarshalOntoDefaultKeepsUnsetFields(t *testing.T) {
	cfg := Default()

	defaultDBDriver := cfg.Database.Driver
	defaultDBDSN := cfg.Database.DSN
	defaultTerminal := cfg.Terminal
	defaultMaxSessions := cfg.MaxConcurrentSessions

	overlay := []byte("server:\n  host: 0.0.0.0\n  port: 9000\n  auth_token: hunter2\n")
	if err := yaml.Unmarshal(overlay, cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if cfg.Server.Host != "0.0.0.0" {
		t.Errorf("server.host: want overlay value, got %q", cfg.Server.Host)
	}
	if cfg.Server.Port != 9000 {
		t.Errorf("server.port: want 9000, got %d", cfg.Server.Port)
	}
	if cfg.Server.AuthToken != "hunter2" {
		t.Errorf("server.auth_token: want overlay value, got %q", cfg.Server.AuthToken)
	}

	if cfg.Database.Driver != defaultDBDriver {
		t.Errorf("database.driver clobbered by overlay: want %q, got %q", defaultDBDriver, cfg.Database.Driver)
	}
	if cfg.Database.DSN != defaultDBDSN {
		t.Errorf("database.dsn clobbered by overlay: want %q, got %q", defaultDBDSN, cfg.Database.DSN)
	}
	if !reflect.DeepEqual(cfg.Terminal, defaultTerminal) {
		t.Error("terminal defaults clobbered by unrelated overlay")
	}
	if cfg.MaxConcurrentSessions != defaultMaxSessions {
		t.Errorf("max_concurrent_sessions clobbered: want %d, got %d", defaultMaxSessions, cfg.MaxConcurrentSessions)
	}
}

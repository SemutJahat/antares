package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// resetLoadedForTest wipes the package cache so each Reload goes through the
// disk seed path again.
func resetLoadedForTest(t *testing.T) {
	t.Helper()
	mu.Lock()
	loaded = nil
	mu.Unlock()
}

// isolateConfigHome points ANTARES_HOME at a fresh temp dir and clears the
// cache. Returns the home path.
func isolateConfigHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ANTARES_HOME", dir)
	t.Setenv("ANTARES_PROFILE", "default")
	t.Setenv("ANTARES_CONFIG", "")
	resetLoadedForTest(t)
	return dir
}

// TestReloadDoesNotPersistTransientHostEnv is the regression for the
// ANTARES_HOST-persisting bug. First boot with env=0.0.0.0 on bare metal:
// runtime wins the env value, on-disk seed stays loopback. Second boot with
// the env cleared: runtime falls back to the persisted loopback, not the
// previous env value.
func TestReloadDoesNotPersistTransientHostEnv(t *testing.T) {
	origProbe := containerProbe
	t.Cleanup(func() { containerProbe = origProbe })
	containerProbe = func() bool { return false }

	home := isolateConfigHome(t)
	cfgPath := filepath.Join(home, "config.yaml")

	// Boot 1: env=0.0.0.0. Env wins in memory; disk is the loopback seed.
	t.Setenv("ANTARES_HOST", "0.0.0.0")
	cfg, err := Reload()
	if err != nil {
		t.Fatalf("first Reload: %v", err)
	}
	if cfg.Server.Host != "0.0.0.0" {
		t.Fatalf("boot 1 runtime host = %q, want %q", cfg.Server.Host, "0.0.0.0")
	}
	if got := readHostFromDisk(t, cfgPath); got != "127.0.0.1" {
		t.Fatalf("boot 1 persisted host = %q, want %q — ANTARES_HOST leaked into config.yaml", got, "127.0.0.1")
	}

	// Boot 2: env cleared. Runtime falls back to the persisted loopback.
	os.Unsetenv("ANTARES_HOST")
	resetLoadedForTest(t)
	cfg, err = Reload()
	if err != nil {
		t.Fatalf("second Reload: %v", err)
	}
	if cfg.Server.Host != "127.0.0.1" {
		t.Fatalf("boot 2 runtime host = %q, want %q — env value persisted across boots", cfg.Server.Host, "127.0.0.1")
	}
	if got := readHostFromDisk(t, cfgPath); got != "127.0.0.1" {
		t.Fatalf("boot 2 persisted host = %q, want %q", got, "127.0.0.1")
	}
}

// TestReloadPreservesYAMLHost proves an operator-authored server.host wins
// across boots and is not overwritten by defaultHost() or by an unset env.
func TestReloadPreservesYAMLHost(t *testing.T) {
	origProbe := containerProbe
	t.Cleanup(func() { containerProbe = origProbe })
	containerProbe = func() bool { return false }

	home := isolateConfigHome(t)
	cfgPath := filepath.Join(home, "config.yaml")
	writeYAMLHost(t, cfgPath, "192.168.42.10")

	os.Unsetenv("ANTARES_HOST")
	cfg, err := Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if cfg.Server.Host != "192.168.42.10" {
		t.Fatalf("runtime host = %q, want the pinned YAML value", cfg.Server.Host)
	}
	if got := readHostFromDisk(t, cfgPath); got != "192.168.42.10" {
		t.Fatalf("persisted host = %q, want %q — operator edit was overwritten", got, "192.168.42.10")
	}
}

// TestReloadEnvOverridesYAMLHost pins the documented precedence: ANTARES_HOST
// beats the stored server.host for the current process, without rewriting it.
func TestReloadEnvOverridesYAMLHost(t *testing.T) {
	origProbe := containerProbe
	t.Cleanup(func() { containerProbe = origProbe })
	containerProbe = func() bool { return false }

	home := isolateConfigHome(t)
	cfgPath := filepath.Join(home, "config.yaml")
	writeYAMLHost(t, cfgPath, "192.168.42.10")

	t.Setenv("ANTARES_HOST", "10.0.0.1")
	cfg, err := Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if cfg.Server.Host != "10.0.0.1" {
		t.Fatalf("runtime host = %q, want the env override", cfg.Server.Host)
	}
	if got := readHostFromDisk(t, cfgPath); got != "192.168.42.10" {
		t.Fatalf("persisted host = %q, want %q — env override rewrote the YAML", got, "192.168.42.10")
	}
}

// TestReloadWhitespaceHostEnvIgnored: applyEnv treats whitespace-only exports
// as unset. A stray `export ANTARES_HOST=" "` must not blank the host.
func TestReloadWhitespaceHostEnvIgnored(t *testing.T) {
	origProbe := containerProbe
	t.Cleanup(func() { containerProbe = origProbe })
	containerProbe = func() bool { return false }

	home := isolateConfigHome(t)
	cfgPath := filepath.Join(home, "config.yaml")
	writeYAMLHost(t, cfgPath, "192.168.42.10")

	t.Setenv("ANTARES_HOST", "   ")
	cfg, err := Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if cfg.Server.Host != "192.168.42.10" {
		t.Fatalf("runtime host = %q, want the pinned YAML value — whitespace env bypassed the trim check", cfg.Server.Host)
	}
	if got := readHostFromDisk(t, cfgPath); got != "192.168.42.10" {
		t.Fatalf("persisted host = %q, want %q", got, "192.168.42.10")
	}
}

// TestReloadContainerSeedPersists: the container heuristic runs once, its
// wildcard is written to disk, and a later boot where the heuristic no
// longer trips still reads that wildcard verbatim. Reload never re-probes.
func TestReloadContainerSeedPersists(t *testing.T) {
	origProbe := containerProbe
	t.Cleanup(func() { containerProbe = origProbe })

	home := isolateConfigHome(t)
	cfgPath := filepath.Join(home, "config.yaml")

	containerProbe = func() bool { return true }
	os.Unsetenv("ANTARES_HOST")
	cfg, err := Reload()
	if err != nil {
		t.Fatalf("first Reload: %v", err)
	}
	if cfg.Server.Host != "0.0.0.0" {
		t.Fatalf("boot 1 runtime host = %q, want %q", cfg.Server.Host, "0.0.0.0")
	}
	if got := readHostFromDisk(t, cfgPath); got != "0.0.0.0" {
		t.Fatalf("boot 1 persisted host = %q, want %q", got, "0.0.0.0")
	}

	containerProbe = func() bool { return false }
	resetLoadedForTest(t)
	cfg, err = Reload()
	if err != nil {
		t.Fatalf("second Reload: %v", err)
	}
	if cfg.Server.Host != "0.0.0.0" {
		t.Fatalf("boot 2 runtime host = %q, want %q — persisted seed lost", cfg.Server.Host, "0.0.0.0")
	}
	if got := readHostFromDisk(t, cfgPath); got != "0.0.0.0" {
		t.Fatalf("boot 2 persisted host = %q, want %q — seed rewritten on re-probe", got, "0.0.0.0")
	}
}

// readHostFromDisk decodes config.yaml directly so a bug that only shows up
// in the persisted value cannot hide behind applyEnv.
func readHostFromDisk(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc struct {
		Server struct {
			Host string `yaml:"host"`
		} `yaml:"server"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return strings.TrimSpace(doc.Server.Host)
}

// writeYAMLHost stages a minimal config.yaml with a pinned server.host,
// mirroring an operator's direct edit.
func writeYAMLHost(t *testing.T, path string, host string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "server:\n  host: " + host + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

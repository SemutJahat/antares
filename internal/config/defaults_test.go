package config

import "testing"

// TestDefaultHost pins the seed: loopback on bare metal, wildcard in a
// container. ANTARES_HOST must NOT influence the seed — that is applyEnv's
// job, and mixing the two is what let a transient env value persist into
// config.yaml.
func TestDefaultHost(t *testing.T) {
	origProbe := containerProbe
	t.Cleanup(func() { containerProbe = origProbe })
	t.Setenv("ANTARES_HOST", "203.0.113.9") // must be ignored

	tests := []struct {
		name      string
		container bool
		want      string
	}{
		{name: "bare metal loopback", container: false, want: "127.0.0.1"},
		{name: "container wildcard", container: true, want: "0.0.0.0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			containerProbe = func() bool { return tc.container }
			if got := defaultHost(); got != tc.want {
				t.Fatalf("defaultHost() = %q, want %q — env leaked into the seed", got, tc.want)
			}
		})
	}
}

// TestDefaultServerHostIgnoresEnv is the direct regression: Default() must
// not consult ANTARES_HOST, otherwise the value gets written to config.yaml
// on first boot and every later boot binds that address even after the env
// export is gone.
func TestDefaultServerHostIgnoresEnv(t *testing.T) {
	origProbe := containerProbe
	t.Cleanup(func() { containerProbe = origProbe })
	containerProbe = func() bool { return false }
	t.Setenv("ANTARES_HOST", "10.9.8.7")

	cfg := Default()
	if cfg.Server.Host != "127.0.0.1" {
		t.Fatalf("Default().Server.Host = %q, want %q — ANTARES_HOST leaked into the seed", cfg.Server.Host, "127.0.0.1")
	}
}

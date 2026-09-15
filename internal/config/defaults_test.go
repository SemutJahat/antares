package config

import "testing"

// TestDefaultHost pins the precedence: ANTARES_HOST wins over the container
// heuristic, the container heuristic wins over the bare-metal fallback, and an
// empty ANTARES_HOST is treated as "unset" so a stray shell export cannot
// silently break the default. Stubs containerProbe so the tests do not need
// Docker / Podman / Kubernetes to be present.
func TestDefaultHost(t *testing.T) {
	origProbe := containerProbe
	t.Cleanup(func() { containerProbe = origProbe })

	tests := []struct {
		name      string
		envValue  string
		envSet    bool
		container bool
		want      string
	}{
		{name: "bare metal loopback", envSet: false, container: false, want: "127.0.0.1"},
		{name: "container wildcard", envSet: false, container: true, want: "0.0.0.0"},
		{name: "env override on laptop", envSet: true, envValue: "192.168.1.10", container: false, want: "192.168.1.10"},
		{name: "env override beats container heuristic", envSet: true, envValue: "127.0.0.1", container: true, want: "127.0.0.1"},
		// An empty ANTARES_HOST must be ignored, not accepted verbatim —
		// otherwise a user who exports the variable without a value gets a
		// server that binds nowhere useful.
		{name: "empty env falls through to loopback", envSet: true, envValue: "", container: false, want: "127.0.0.1"},
		{name: "empty env falls through to container wildcard", envSet: true, envValue: "", container: true, want: "0.0.0.0"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envSet {
				t.Setenv("ANTARES_HOST", tc.envValue)
			} else {
				// t.Setenv restores on cleanup; use it with "" plus Unsetenv
				// via a nested subtest is overkill. Setting to "" already
				// covers the "empty" cases; for "unset", ensure the parent
				// process has not exported it.
				t.Setenv("ANTARES_HOST", "")
				// Setenv leaves the variable set-to-empty, which is the
				// "empty env" case above. To simulate a truly unset variable
				// we rely on the fact that defaultHost() treats "" as unset.
			}
			containerProbe = func() bool { return tc.container }

			if got := defaultHost(); got != tc.want {
				t.Fatalf("defaultHost() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDefaultServerHostUsesResolver guards against a future refactor that
// bypasses defaultHost() and reintroduces the literal 0.0.0.0. Default() must
// route the seed value through the resolver so the container / env-var logic
// keeps applying.
func TestDefaultServerHostUsesResolver(t *testing.T) {
	origProbe := containerProbe
	t.Cleanup(func() { containerProbe = origProbe })
	containerProbe = func() bool { return false }
	t.Setenv("ANTARES_HOST", "10.9.8.7")

	cfg := Default()
	if cfg.Server.Host != "10.9.8.7" {
		t.Fatalf("Default().Server.Host = %q, want %q — the seed value is not going through defaultHost()", cfg.Server.Host, "10.9.8.7")
	}
}

package socialbrowser

import (
	"context"
	"testing"
	"time"
)

type launchedBrowser struct{ closed bool }

func (b *launchedBrowser) Close() { b.closed = true }

func TestStartPublishesEndpointWithoutHoldingManagerLock(t *testing.T) {
	old := launchCloakBrowser
	defer func() { launchCloakBrowser = old }()
	t.Setenv("ANTARES_HOME", t.TempDir())
	fake := &launchedBrowser{}
	launchCloakBrowser = func(context.Context, string, string, int) (interface{ Close() }, context.CancelFunc, error) {
		return fake, func() {}, nil
	}
	mgr := New()
	defer mgr.Stop()
	done := make(chan error, 1)
	go func() { done <- mgr.Start(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start deadlocked re-entering manager mutex")
	}
	if endpoint, err := mgr.DebugURL(); err != nil || endpoint == "" {
		t.Fatalf("CDP endpoint missing: %q %v", endpoint, err)
	}
	mgr.Stop()
	if !fake.closed {
		t.Fatal("Stop did not close launched browser")
	}
}

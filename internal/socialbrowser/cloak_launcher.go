package socialbrowser

import (
	"context"
	"fmt"

	"github.com/chromedp/chromedp"
	cloak "github.com/enowdev/cloak-go"
)

func init() {
	launchCloakBrowser = realLaunchCloak
}

// realLaunchCloak launches a stealth Chromium via cloak-go with a persistent
// user-data-dir, stable fingerprint seed, and a fixed CDP debug port so the
// generic browser tool can attach as a second controller against the same
// browser process.
//
// The debug port is passed through cloak's ExtraArgs (bind to loopback only —
// never expose CDP to non-local traffic). BuildArgs in cloak-go dedups by
// key, so if a caller ever supplied their own remote-debugging-port it
// would collide; we don't, and this is the only launch site.
func realLaunchCloak(ctx context.Context, profileDir, seed string, debugPort int) (interface{ Close() }, context.CancelFunc, error) {
	extra := []string{
		fmt.Sprintf("--remote-debugging-port=%d", debugPort),
		"--remote-debugging-address=127.0.0.1",
	}
	opts := cloak.LaunchOptions{
		Headless:        false,
		StealthArgs:     true,
		StartMaximized:  true,
		UserDataDir:     profileDir,
		FingerprintSeed: seed,
		ExtraArgs:       extra,
	}

	browser, err := cloak.Launch(ctx, opts)
	if err != nil {
		return nil, nil, fmt.Errorf("cloak launch: %w", err)
	}
	// cloak.Launch only builds chromedp contexts; the first Run starts Chrome.
	if err := chromedp.Run(browser.Ctx); err != nil {
		browser.Close()
		return nil, nil, fmt.Errorf("start social browser: %w", err)
	}

	// The browser context is torn down by Browser.Close(); expose a no-op
	// cancel so the Manager's Stop path stays uniform.
	cancel := context.CancelFunc(func() {})

	return browser, cancel, nil
}

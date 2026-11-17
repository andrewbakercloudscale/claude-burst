package admin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/desktop"
	"github.com/andrewbakercloudscale/claude-burst/internal/tlsca"
)

// The support console shows a Claude Desktop row only where Desktop is
// installed, and it goes red when Desktop's own CA bundle lacks Burst's
// CA: the "Claude Code trusts Burst" row checks the terminal's bundle,
// which Desktop never reads.
func TestConsoleChecksClaudeDesktop(t *testing.T) {
	c, _, _ := newTestConsole(t)
	home, _ := os.UserHomeDir()
	cfgDir, _ := config.ConfigDir()
	caDir := filepath.Join(cfgDir, "ca")
	bundle := filepath.Join(home, ".claude", "certs", "node-extra-ca-certs.pem")
	_, caPEM, err := tlsca.LoadOrCreate(caDir, "api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := tlsca.EnsureInBundle(bundle, caPEM); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Intercept = config.InterceptConfig{Mode: config.InterceptTransparent, Host: "api.anthropic.com", CADir: caDir, CABundle: bundle}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}

	row := func() (consoleCheck, bool) {
		rec := consoleDo(t, c.Handler(), "GET", "/api/console")
		var st consoleStatus
		if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		for _, ch := range st.Checks {
			if ch.Name == "Claude Desktop" {
				return ch, true
			}
		}
		return consoleCheck{}, false
	}
	if _, ok := row(); ok {
		t.Fatal("a Claude Desktop row with no Claude Desktop installed")
	}

	apps := os.Getenv(desktop.ApplicationsDirEnv)
	app := filepath.Join(apps, "Claude.app", "Contents", "Info.plist")
	if err := os.MkdirAll(filepath.Dir(app), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(app, []byte("<key>CFBundleShortVersionString</key><string>5.11492.5</string>"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(filepath.Join(apps, "Claude.app")) })
	paths := desktop.DefaultPaths(home)
	if err := os.MkdirAll(paths.Support, 0o755); err != nil {
		t.Fatal(err)
	}
	// Desktop started before the CA was trusted system-wide: its bundle has
	// only the public roots.
	if err := os.WriteFile(paths.CABundle(), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ch, ok := row()
	if !ok || ch.OK {
		t.Fatalf("stale Desktop bundle: %+v", ch)
	}
	if err := os.WriteFile(paths.CABundle(), caPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	mpath, _ := config.MetricsPath()
	line := `{"time":"` + time.Now().UTC().Format(time.RFC3339) + `","route":"anthropic","client":"claude-desktop"}` + "\n"
	if err := os.WriteFile(mpath, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if ch, ok = row(); !ok || !ch.OK {
		t.Fatalf("trusted and seen: %+v", ch)
	}
}

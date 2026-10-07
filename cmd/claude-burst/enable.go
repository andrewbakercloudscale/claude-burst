package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/admin"
	"github.com/andrewbakercloudscale/claude-burst/internal/claudesettings"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/tlsca"
)

func enable(args []string) {
	force := false
	for _, a := range args {
		if a != "--force" {
			fatal(fmt.Errorf("enable: unknown argument %q (only --force)", a))
		}
		force = true
	}
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	p, err := claudesettings.Path()
	if err != nil {
		fatal(err)
	}
	root, err := claudesettings.Read(p)
	if err != nil {
		fatal(err)
	}

	if cfg.Intercept.Transparent() {
		// Transparent mode's entire purpose is that this variable stays unset:
		// Claude Code disables Remote Control whenever it names another host.
		// Setting it here would silently defeat the feature.
		if claudesettings.ClearBaseURL(root, cfg.Listen) {
			if err := claudesettings.Write(p, root); err != nil {
				fatal(err)
			}
			fmt.Printf("removed ANTHROPIC_BASE_URL from %s (transparent mode needs it unset)\n", p)
		} else if cur := claudesettings.BaseURL(root); cur != "" {
			// An enterprise gateway (Portkey and similar): Claude Code sends
			// to that host, never to the intercepted one, so a redirect would
			// catch nothing while looking installed. Base-url mode adopts it.
			fatal(fmt.Errorf("%s sets ANTHROPIC_BASE_URL=%s, so Claude Code never contacts %s and transparent mode would see nothing. "+
				"Use base-url mode instead (set intercept.mode to \"base-url\" and run enable): it puts Claude Burst in front of that gateway and keeps it as the primary", p, cur, cfg.Intercept.Host))
		} else {
			fmt.Printf("ANTHROPIC_BASE_URL already unset in %s\n", p)
		}

		_, caPEM, err := tlsca.LoadOrCreate(cfg.Intercept.CADir, cfg.Intercept.Host)
		if err != nil {
			fatal(err)
		}
		// Checked before the bundle is touched, which would move the date.
		if stale := sessionsNotTrusting(cfg.Intercept.CABundle, caPEM); len(stale) > 0 && !force {
			fatal(fmt.Errorf("%d Claude Code session(s) running (pid %s) started before this gateway's CA was in %s. "+
				"Node reads that file once, at startup, so once traffic is redirected every one of them fails each request "+
				"(UNABLE_TO_VERIFY_LEAF_SIGNATURE or SELF_SIGNED_CERT_IN_CHAIN) until restarted. "+
				"Close them all and run this again from a plain terminal, or pass --force and restart them yourself",
				len(stale), strings.Join(stale, ", "), cfg.Intercept.CABundle))
		}
		if err := tlsca.EnsureInBundle(cfg.Intercept.CABundle, caPEM); err != nil {
			fatal(err)
		}
		fmt.Printf("local CA in %s\ntrusted via %s\n", cfg.Intercept.CADir, cfg.Intercept.CABundle)

		helper := rootHelperPath()
		fmt.Printf(`
Two machine-wide steps remain, and they need root. They are deliberately NOT
run for you: they affect every process on this Mac, not just Claude Code.

  1. make sure the gateway is running and healthy
  2. sudo %s install --host %s --gateway-port %s

Undo at any time with:
     sudo %s remove

Then restart Claude Code. Verify with: claude-burst status
`, helper, cfg.Intercept.Host, portOf(cfg.Listen), helper)
		return
	}

	// An enterprise gateway (Portkey, a corporate LLM proxy) already set as
	// ANTHROPIC_BASE_URL is adopted, not overwritten: it becomes the primary,
	// so Claude Code -> Burst -> that gateway, with every header Claude Code
	// sends (its auth and ANTHROPIC_CUSTOM_HEADERS) passed through unchanged.
	// disable restores it.
	if err := refuseManagedBaseURL(); err != nil {
		fatal(err)
	}
	if cur := claudesettings.BaseURL(root); cur != "" && !claudesettings.OwnBaseURL(cur, cfg.Listen) {
		cfg.Primary.BaseURL = strings.TrimRight(cur, "/")
		cfg.AdoptedBaseURL = cur
		if err := config.Save(cfg); err != nil {
			fatal(err)
		}
		fmt.Printf("adopted %s as the primary: Claude Code -> Claude Burst -> %s. disable puts it back.\n", cur, cur)
	}

	// Important: do NOT set a gateway API credential here. In
	// oauth-passthrough mode this preserves the saved Max OAuth
	// subscription; in anthropic-api-key mode, Claude Code's own
	// ANTHROPIC_API_KEY (set separately) is what gets forwarded unchanged.
	claudesettings.SetBaseURL(root, "http://"+cfg.Listen)
	if err := claudesettings.Write(p, root); err != nil {
		fatal(err)
	}
	fmt.Printf("enabled Claude Burst in %s\n", p)
	startGatewayAgent()
	fmt.Println("Claude Code sessions started from now on go through Burst; sessions already open keep their direct route until restarted.")
}

func disable(args []string) {
	rejectArgs("disable", args)
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	p, err := claudesettings.Path()
	if err != nil {
		fatal(err)
	}
	// A token-shunting hook left by an earlier install runs this binary on
	// every Read and Bash call; disabling Claude Burst takes it out too.
	if removed, err := removeLegacyShunt(); err != nil {
		fatal(err)
	} else if removed {
		fmt.Println("removed the token-shunting hook and skill left by an earlier install")
	}
	if _, err := os.Stat(p); os.IsNotExist(err) && !cfg.Intercept.Transparent() {
		fmt.Println("already disabled")
		return
	}
	root, err := claudesettings.Read(p)
	if err != nil {
		fatal(err)
	}
	if claudesettings.Release(root, cfg.Listen, cfg.AdoptedBaseURL) {
		if err := claudesettings.Write(p, root); err != nil {
			fatal(err)
		}
		if cfg.AdoptedBaseURL != "" {
			fmt.Printf("restored ANTHROPIC_BASE_URL=%s\n", cfg.AdoptedBaseURL)
		}
	}

	if cfg.Intercept.Transparent() {
		if err := tlsca.RemoveFromBundle(cfg.Intercept.CABundle); err != nil {
			fatal(err)
		}
		fmt.Printf("removed the local CA from %s (other certificates left untouched)\n", cfg.Intercept.CABundle)

		helper := rootHelperPath()
		fmt.Printf(`
The machine-wide redirect is still in place and needs root to remove.
Until you run this, traffic to %s on this Mac still goes to the gateway:

  sudo %s remove
`, cfg.Intercept.Host, helper)
		return
	}
	fmt.Println("disabled Claude Burst: Claude Code sessions started from now on go straight to " + passthroughTarget(cfg))
	handOverToPassthrough(cfg)
}

// uninstallHooks removes everything Burst installed in ~/.claude, through
// each feature's own remover, and leaves config.json alone. install.sh
// uninstall runs it while the binary still exists, after stopping the
// gateway: a gateway that starts reinstalls its hooks.
func uninstallHooks(args []string) {
	rejectArgs("uninstall-hooks", args)
	_, err := removeLegacyShunt()
	err = errors.Join(err, admin.RemoveAllHooks())
	left, lerr := admin.HookLeftovers()
	if lerr != nil {
		err = errors.Join(err, lerr)
	}
	if len(left) > 0 {
		fmt.Fprintln(os.Stderr, "still naming claude-burst after removal (remove these by hand):")
		for _, l := range left {
			fmt.Fprintln(os.Stderr, "  "+l)
		}
	}
	if err != nil {
		fatal(err)
	}
	if len(left) > 0 {
		os.Exit(1)
	}
	fmt.Println("removed Claude Burst's hooks, skill and /compact-async from ~/.claude (config.json unchanged)")
}

// caRotate replaces an unconstrained local CA with one constrained to the
// intercepted host. The gateway no longer does this by itself: every Claude
// Code session running at that moment trusts only the old CA and fails TLS
// until restarted (2026-10-02, see internal/tlsca), so this refuses while
// any is running unless --force is given.
func caRotate(args []string) {
	force := false
	for _, a := range args {
		if a != "--force" {
			fatal(fmt.Errorf("ca-rotate: unknown argument %q (only --force)", a))
		}
		force = true
	}
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	if out, _ := exec.Command("pgrep", "-x", "claude").Output(); len(strings.TrimSpace(string(out))) > 0 && !force {
		n := len(strings.Fields(string(out)))
		fatal(fmt.Errorf("%d Claude Code process(es) running. Each trusts only the current CA and would fail every request with SELF_SIGNED_CERT_IN_CHAIN until restarted. Close them all and run this again, or pass --force and restart them yourself", n))
	}
	caPEM, err := tlsca.Rotate(cfg.Intercept.CADir, cfg.Intercept.Host)
	if err != nil {
		fatal(err)
	}
	if err := tlsca.EnsureInBundle(cfg.Intercept.CABundle, caPEM); err != nil {
		fatal(err)
	}
	fmt.Printf(`new CA in %s, constrained to %s, and in %s.
Next:
  1. restart the gateway: launchctl kickstart -k gui/$UID/ninja.andrewbaker.claude-burst
  2. if the old CA was trusted system-wide: sudo %s
  3. start Claude Code sessions again
`, cfg.Intercept.CADir, cfg.Intercept.Host, cfg.Intercept.CABundle, filepath.Join(filepath.Dir(rootHelperPath()), "trust-ca-systemwide.sh"))
}

// managedSettingsPath is where an organisation's managed Claude Code
// settings live on macOS. They override ~/.claude/settings.json, so a base
// URL set there cannot be taken over.
var managedSettingsPath = "/Library/Application Support/ClaudeCode/managed-settings.json"

// refuseManagedBaseURL fails when managed settings set ANTHROPIC_BASE_URL:
// enable would write a URL Claude Code never uses and look as if it worked.
func refuseManagedBaseURL() error {
	root, err := claudesettings.Read(managedSettingsPath)
	if err != nil {
		return nil // unreadable or invalid: not ours to judge, and not a base URL we can see
	}
	if u := claudesettings.BaseURL(root); u != "" {
		return fmt.Errorf("%s (managed by your organisation) sets ANTHROPIC_BASE_URL=%s, which overrides ~/.claude/settings.json, so Claude Burst cannot sit in front of it. Ask whoever manages it, or leave Claude Burst disabled", managedSettingsPath, u)
	}
	return nil
}

// runningClaude lists Claude Code processes as pid and start time. A
// variable so tests never see this Mac's real sessions.
var runningClaude = func() map[string]time.Time {
	out, err := exec.Command("ps", "-axo", "pid=,etime=,comm=").Output()
	if err != nil {
		return nil
	}
	now := time.Now()
	procs := map[string]time.Time{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || filepath.Base(strings.Join(f[2:], " ")) != "claude" {
			continue
		}
		if age, ok := parseEtime(f[1]); ok {
			procs[f[0]] = now.Add(-age)
		}
	}
	return procs
}

// parseEtime reads ps's elapsed time, [[dd-]hh:]mm:ss.
func parseEtime(s string) (time.Duration, bool) {
	days := 0
	if i := strings.Index(s, "-"); i >= 0 {
		d, err := strconv.Atoi(s[:i])
		if err != nil {
			return 0, false
		}
		days, s = d, s[i+1:]
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	total := 0
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, false
		}
		total = total*60 + n
	}
	return time.Duration(days)*24*time.Hour + time.Duration(total)*time.Second, true
}

// sessionsNotTrusting returns the pids of running Claude Code sessions that
// would refuse the gateway's certificate: every one when the bundle does not
// hold caPEM, else those started before it did. On 2026-10-02 13:22 a
// re-enable with sessions open from before the CA changed failed all of them.
func sessionsNotTrusting(bundle string, caPEM []byte) []string {
	since, ok := tlsca.TrustedSince(bundle, caPEM)
	var pids []string
	for pid, started := range runningClaude() {
		if !ok || started.Before(since) {
			pids = append(pids, pid)
		}
	}
	sort.Strings(pids)
	return pids
}

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
	"github.com/andrewbakercloudscale/claude-burst/internal/router"
	"github.com/andrewbakercloudscale/claude-burst/internal/tlsca"
)

func status() {
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	statePath, _ := config.StatePath()
	metricsPath, _ := config.MetricsPath()
	srv, err := router.New(cfg, statePath, metricsPath, log.New(os.Stderr, "", 0))
	if err != nil {
		fatal(err)
	}
	// What is running, asked of it: everything below is read from the files
	// and is the same whether or not a gateway is up, which made "status"
	// say PRIMARY with nothing running at all.
	fmt.Println(runningLine(cfg))
	if pid := runningPassthrough(); pid > 0 {
		fmt.Printf("pass-through: running (pid %d): Burst is off, and sessions started while it was on go straight to %s\n", pid, passthroughTarget(cfg))
	}
	st := srv.Status()
	now := time.Now().Unix()
	if st.OverflowUntil > now {
		fmt.Printf("route: SECONDARY (%s) forced\nuntil: %s\nclaim: %s\nreason: %s\n",
			cfg.Secondary.Provider, time.Unix(st.OverflowUntil, 0).Format(time.RFC3339), st.LimitClaim, st.LastReason)
	} else {
		fmt.Printf("route: PRIMARY (%s)\n", cfg.Primary.Provider)
	}
	// Per-model windows are printed whatever the line above says. Without
	// them "route: PRIMARY" is true of the account and wrong about the model
	// you are actually using, which is the only one you care about while a
	// limit is open.
	var refused []string
	for model, until := range st.ModelOverflow {
		if until > now {
			refused = append(refused, model)
		}
	}
	sort.Strings(refused)
	for _, model := range refused {
		dest := cfg.Secondary.Provider + " (secondary)"
		if !st.DowngradeDisabled {
			for _, rung := range cfg.FallbackChain[model] {
				if rung != model && st.ModelOverflow[rung] <= now {
					dest = rung + " (still on the subscription)"
					break
				}
			}
		}
		fmt.Printf("refused: %s until %s -> served by %s\n", model, time.Unix(st.ModelOverflow[model], 0).Format(time.RFC3339), dest)
	}
	if len(refused) > 0 && st.DowngradeDisabled {
		fmt.Println("downgrade: OFF (fallback_chain is configured but switched off from the dashboard)")
	}
	scheme := "http"
	if cfg.Intercept.Transparent() {
		scheme = "https"
	}
	fmt.Printf("gateway: %s://%s\nprimary: %s (%s)\nsecondary: %s (%s)\n",
		scheme, cfg.Listen, cfg.Primary.Provider, cfg.Primary.BaseURL, cfg.Secondary.Provider, cfg.Secondary.BaseURL)
	reportIntercept(cfg)
	if legacyShuntHookInstalled() {
		fmt.Println("shunt: a token-shunting hook from an earlier install is still in settings.json (the feature was removed). Run: claude-burst shunt disable")
	}
	reportKeepAwake(cfg)
}

// runningLine says whether a gateway is answering and which version it is,
// beside this binary's: after an upgrade that did not restart it, or a
// rollback, the two differ and nothing else says so.
func runningLine(cfg config.Config) string {
	if cfg.AdminListen == "" || cfg.AdminListen == "off" {
		return "running: unknown (the dashboard is off, so the gateway cannot be asked); this binary is " + version
	}
	u, err := adminURL(cfg.AdminListen, "/api/state")
	if err != nil {
		return "running: unknown (" + err.Error() + "); this binary is " + version
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(u)
	if err != nil {
		return "running: NO gateway is answering on " + cfg.AdminListen + " (claude-burst open shows why); this binary is " + version
	}
	defer resp.Body.Close()
	var st struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&st); err != nil || st.Version == "" {
		return fmt.Sprintf("running: something answers on %s (HTTP %d) but not as Claude Burst; this binary is %s", cfg.AdminListen, resp.StatusCode, version)
	}
	if st.Version != version {
		return "running: version " + st.Version + ", which is NOT this binary (" + version + "): restart the gateway to run it"
	}
	return "running: version " + st.Version
}

// reportIntercept surfaces the facts that decide whether transparent mode is
// actually working. Each can break independently and none of them announce
// themselves: a missing CA looks like a TLS error, a missing hosts entry looks
// like the feature silently not being on.
func reportIntercept(cfg config.Config) {
	if !cfg.Intercept.Transparent() {
		fmt.Println("intercept: base-url (Remote Control disabled while enabled -- see --intercept-mode transparent)")
		return
	}
	fmt.Printf("intercept: transparent (%s)\n", cfg.Intercept.Host)

	ok := func(b bool) string {
		if b {
			return "yes"
		}
		return "NO"
	}

	bundle, berr := os.ReadFile(cfg.Intercept.CABundle)
	fmt.Printf("  CA in trust bundle: %s (%s)\n", ok(berr == nil && tlsca.HasBlock(string(bundle))), cfg.Intercept.CABundle)

	hosts, herr := os.ReadFile("/etc/hosts")
	hostsEntry := herr == nil && config.HostsRedirectActive(hosts, cfg.Intercept.Host)
	fmt.Printf("  /etc/hosts entry:   %s\n", ok(hostsEntry))

	if _, _, err := tlsca.LoadOrCreate(cfg.Intercept.CADir, cfg.Intercept.Host); err != nil {
		fmt.Printf("  certificate:        NO (%v)\n", err)
	} else {
		fmt.Printf("  certificate:        yes (%s)\n", cfg.Intercept.CADir)
	}

	if !hostsEntry {
		fmt.Println("  -> transparent mode is configured but not installed; run: claude-burst enable")
	}
	fmt.Println("  pf redirect state:  sudo scripts/transparent-root.sh status")
}

// adminURL builds a URL for the RUNNING gateway's admin listener. A listen
// address with no host, or a wildcard one, is dialled on loopback: the admin
// server's own Host-header guard only accepts loopback names, so "0.0.0.0"
// would be rejected by the very server we are trying to reach.
func adminURL(adminListen, path string) (string, error) {
	host, port, err := net.SplitHostPort(adminListen)
	if err != nil {
		return "", fmt.Errorf("admin_listen %q is not a host:port address: %w", adminListen, err)
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + path, nil
}

// errAdminUnreachable distinguishes "no gateway is running" from "the running
// gateway answered and said no". The two need opposite handling: the first is
// a legitimate reason to fall back to writing state.json, the second must
// never be overridden by writing the file behind the running process's back.
type errAdminUnreachable struct{ err error }

func (e *errAdminUnreachable) Error() string { return e.err.Error() }

// adminPost sends a mutating request to the running gateway's admin API and
// returns its "ok" message.
//
// This exists because every state-changing CLI subcommand used to build its
// OWN router.Server, mutate that, and write state.json -- which does nothing
// at all to the gateway process actually serving traffic, since router.New
// reads state.json exactly once, at startup. `claude-burst reset` therefore
// printed "overflow state cleared" while the live gateway went on routing to
// the paid secondary until it happened to restart, and `force-secondary`
// silently failed to exercise the secondary it exists to exercise. The
// confident message was the dangerous half: a claim about a process the
// command had never spoken to.
func adminPost(adminListen, path string, body any) (string, error) {
	if adminListen == "" {
		return "", &errAdminUnreachable{fmt.Errorf("the admin listener is disabled (admin_listen is off)")}
	}
	u, err := adminURL(adminListen, path)
	if err != nil {
		return "", err
	}
	var buf io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return "", err
		}
		buf = bytes.NewReader(b)
	}
	req, err := http.NewRequest(http.MethodPost, u, buf)
	if err != nil {
		return "", err
	}
	// The admin server requires this header on every mutation; a cross-origin
	// page cannot set it without a preflight the server never answers.
	req.Header.Set("X-Claude-Burst-Admin", "cli")
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return "", &errAdminUnreachable{err}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Reached it, and it refused. Report that verbatim and do NOT fall
		// back -- e.g. /api/force refuses when the running process has no
		// secondary built, and writing an overflow window to state.json
		// anyway would arm a window the gateway cannot serve.
		return "", fmt.Errorf("the running gateway refused: %s", strings.TrimSpace(string(respBody)))
	}
	var out map[string]string
	if json.Unmarshal(respBody, &out) == nil && out["ok"] != "" {
		return out["ok"], nil
	}
	return strings.TrimSpace(string(respBody)), nil
}

func reset() {
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}

	msg, err := adminPost(cfg.AdminListen, "/api/reset", nil)
	if err == nil {
		fmt.Printf("running gateway: %s\n", msg)
		return
	}
	var unreachable *errAdminUnreachable
	if !errors.As(err, &unreachable) {
		fatal(err)
	}

	// No gateway answering, so there is no in-memory state to clear -- only
	// the file a future start will read. Say exactly that rather than
	// implying something live was changed.
	statePath, _ := config.StatePath()
	metricsPath, _ := config.MetricsPath()
	srv, err := router.New(cfg, statePath, metricsPath, log.New(os.Stderr, "", 0))
	if err != nil {
		fatal(err)
	}
	srv.ClearOverflow()
	fmt.Printf("could not reach a running gateway on %s (%v)\n"+
		"cleared the saved overflow state in %s instead -- a gateway starting from now on will come up on the primary.\n"+
		"If one IS running, it keeps its own in-memory state until it restarts: launchctl kickstart -k gui/$UID/ninja.andrewbaker.claude-burst\n",
		cfg.AdminListen, unreachable, statePath)
}

// forceSecondary makes the untestable testable: a subscription primary only
// fails over on real exhaustion signals, so without this the secondary path
// stays unexercised until the day it is needed.
func forceSecondary(args []string) {
	fs := flag.NewFlagSet("force-secondary", flag.ExitOnError)
	minutes := fs.Int("minutes", 15, "how long to stay on the secondary")
	_ = fs.Parse(args)
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	if cfg.Secondary.Provider == "" || cfg.Secondary.Provider == config.ProviderNone {
		fatal(fmt.Errorf("no secondary provider configured, so there is nothing to fail over to"))
	}
	// Same reasoning as reset(): the running gateway is the thing that has to
	// change, and it is the only thing that knows whether it actually built a
	// secondary Provider (see router.Server.HasSecondary). The config check
	// above is a courtesy for the no-gateway case; /api/force is the check
	// that counts.
	msg, err := adminPost(cfg.AdminListen, "/api/force", map[string]int{"minutes": *minutes})
	if err == nil {
		fmt.Printf("running gateway: %s\nback to primary at any time with: claude-burst reset\n", msg)
		return
	}
	var unreachable *errAdminUnreachable
	if !errors.As(err, &unreachable) {
		fatal(err)
	}

	statePath, _ := config.StatePath()
	metricsPath, _ := config.MetricsPath()
	srv, err := router.New(cfg, statePath, metricsPath, log.New(os.Stderr, "", 0))
	if err != nil {
		fatal(err)
	}
	until := srv.ForceOverflow(time.Duration(*minutes)*time.Minute, "forced from the CLI")
	fmt.Printf("could not reach a running gateway on %s (%v)\n"+
		"wrote the forced-overflow window to disk instead: a gateway starting from now until %s will come up on %s (%s).\n"+
		"If one IS running, it is unaffected until it restarts.\nback to primary at any time with: claude-burst reset\n",
		cfg.AdminListen, unreachable, until.Format(time.RFC3339), cfg.Secondary.Provider, cfg.Secondary.Model)
}

func stats(args []string) {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	days := fs.Int("days", 30, "days to summarize (0 = all)")
	_ = fs.Parse(args)
	p, _ := config.MetricsPath()
	if cfg, err := config.Load(); err == nil {
		metrics.SetPricer(cfg.PriceTokens)
		metrics.SetLongWritePricer(cfg.LongWriteExtraUSD)
	}
	var since time.Time
	if *days > 0 {
		since = time.Now().Add(-time.Duration(*days) * 24 * time.Hour)
	}
	s, err := metrics.Summarize(p, since)
	if err != nil {
		fatal(err)
	}
	fmt.Println(s.String())
}

package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/admin"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/keychain"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
	"github.com/andrewbakercloudscale/claude-burst/internal/router"
	"github.com/andrewbakercloudscale/claude-burst/internal/tlsca"
)

const version = "0.2.0"

func main() {
	if len(os.Args) < 2 {
		serve(os.Args[1:])
		return
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "configure":
		configure(os.Args[2:])
	case "keychain-set":
		keychainSet(os.Args[2:])
	case "enable":
		enable(os.Args[2:])
	case "disable":
		disable(os.Args[2:])
	case "status":
		status()
	case "reset":
		reset()
	case "force-secondary":
		forceSecondary(os.Args[2:])
	case "stats":
		stats(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`claude-burst - route Claude Code through a primary provider, with a configurable secondary provider for overflow

Commands:
  serve             Run the local gateway (default: 127.0.0.1:7777)
  configure         Write/update config.json
  keychain-set      Store $AWS_BEARER_TOKEN_BEDROCK in macOS Keychain (Bedrock secondary)
  enable            Point Claude Code at the local gateway via ~/.claude/settings.json
  disable           Remove Claude Burst from Claude Code settings
  status            Show routing state
  reset             Clear overflow state immediately (back to primary)
  force-secondary   Route inference to the secondary for a while (testing)
  stats             Summarize local routing/token metrics
  version           Print version

Admin UI:
  A local control panel runs alongside the gateway on 127.0.0.1:7788 by
  default -- recent requests, response headers that drive failover, config
  changes, and a one-click revert to stock Claude. It binds loopback and has
  no login; disable it with: claude-burst configure --admin-listen off

Keeping Claude Code's Remote Control (optional):
  Claude Code disables Remote Control whenever ANTHROPIC_BASE_URL names a host
  other than api.anthropic.com, so the default "base-url" mode costs you that
  feature. "transparent" mode instead redirects at the DNS layer and terminates
  TLS locally, leaving the variable unset. It needs root once, for an
  /etc/hosts entry and a pf rule:
    claude-burst configure --intercept-mode transparent
    claude-burst enable          # prints the one sudo step that remains
  Undo with: sudo scripts/transparent-root.sh remove

Setup with a Claude Max/Pro subscription (default), Bedrock overflow:
  export AWS_BEARER_TOKEN_BEDROCK='...'
  claude-burst keychain-set
  claude-burst configure --region us-east-1
  claude-burst enable
  claude-burst serve

Setup with no subscription (metered Anthropic API key primary), Bedrock overflow:
  export AWS_BEARER_TOKEN_BEDROCK='...'
  claude-burst keychain-set
  claude-burst configure --primary anthropic-api-key --secondary bedrock --region us-east-1
  claude-burst enable
  # Then set ANTHROPIC_API_KEY in Claude Code's own settings env -- the
  # gateway never stores or injects an Anthropic credential itself, it only
  # forwards whatever auth header Claude Code already sent.
  claude-burst serve
`)
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "", "override listen address")
	_ = fs.Parse(args)
	if err := config.EnsureDir(); err != nil {
		fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	statePath, _ := config.StatePath()
	metricsPath, _ := config.MetricsPath()
	logPath, _ := config.LogPath()
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		fatal(err)
	}
	logger := log.New(lf, "", log.LstdFlags|log.LUTC)
	srv, err := router.New(cfg, statePath, metricsPath, logger)
	if err != nil {
		fatal(err)
	}

	scheme := "http"
	var tlsConfig *tls.Config
	if cfg.Intercept.Transparent() {
		leaf, caPEM, err := tlsca.LoadOrCreate(cfg.Intercept.CADir, cfg.Intercept.Host)
		if err != nil {
			fatal(err)
		}
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{*leaf},
			MinVersion:   tls.VersionTLS12,
		}
		scheme = "https"

		// A CA that is not in the trust bundle produces a TLS error at the
		// client that looks like a network fault. Corporate tooling can
		// regenerate that file and drop our block, so say so plainly at
		// startup rather than leaving it to be discovered.
		if b, rerr := os.ReadFile(cfg.Intercept.CABundle); rerr != nil || !tlsca.HasBlock(string(b)) {
			msg := fmt.Sprintf("WARNING: the local CA is not present in %s -- Claude Code will reject this gateway's certificate. Run: claude-burst enable", cfg.Intercept.CABundle)
			fmt.Fprintln(os.Stderr, msg)
			logger.Print(msg)
		}
		_ = caPEM
	}

	logger.Printf("claude-burst %s listening on %s (%s)", version, cfg.Listen, scheme)
	fmt.Printf("claude-burst %s listening on %s://%s\n", version, scheme, cfg.Listen)
	fmt.Printf("primary: %s (%s)\nsecondary: %s (%s)\n", cfg.Primary.Provider, cfg.Primary.BaseURL, cfg.Secondary.Provider, cfg.Secondary.BaseURL)
	if cfg.Intercept.Transparent() {
		fmt.Printf("intercept: transparent (serving TLS for %s)\n", cfg.Intercept.Host)
	}

	if cfg.AdminListen != "" {
		a := admin.New(srv, metricsPath, version, cfg.AdminHostname, rootHelperPath())
		fmt.Printf("admin:  %s\n", admin.Describe(cfg.AdminListen))
		if cfg.AdminHostname != "" {
			_, port, _ := strings.Cut(cfg.AdminListen, ":")
			fmt.Printf("        http://%s:%s\n", cfg.AdminHostname, port)
		}
		// Runs alongside the gateway. A bind failure is logged and surfaced
		// rather than swallowed -- an admin panel that silently is not there
		// is worse than one that says why.
		go func() {
			if err := a.ListenAndServe(cfg.AdminListen); err != nil {
				logger.Printf("admin server stopped: %v", err)
				fmt.Fprintf(os.Stderr, "admin server stopped: %v\n", err)
			}
		}()
	}

	// No ReadTimeout/WriteTimeout/IdleTimeout on purpose. Claude Code's Remote
	// Control registers and then long-polls for work, holding a connection
	// open with nothing on it; a server-side deadline would sever exactly that
	// and present as Remote Control dropping repeatedly for no visible reason.
	server := &http.Server{Addr: cfg.Listen, Handler: srv, TLSConfig: tlsConfig}
	if tlsConfig != nil {
		err = server.ListenAndServeTLS("", "") // certificates come from TLSConfig
	} else {
		err = server.ListenAndServe()
	}
	if err != nil {
		fatal(err)
	}
}

func configure(args []string) {
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	fs := flag.NewFlagSet("configure", flag.ExitOnError)
	region := fs.String("region", "", "AWS Bedrock region")
	listen := fs.String("listen", "", "listen address")
	adminListen := fs.String("admin-listen", "", "admin UI address, or \"off\" to disable")
	adminHostname := fs.String("admin-hostname", "", "friendly hostname for the admin UI, or \"off\" to clear")
	bedrockBase := fs.String("bedrock-base-url", "", "override Bedrock Anthropic Messages base URL")
	primary := fs.String("primary", "", "primary provider: oauth-passthrough | anthropic-api-key")
	failoverStrategy := fs.String("failover-strategy", "", "override primary failover_strategy: subscription-limit | metered-failures | subscription-limit+metered-failures | none")
	secondary := fs.String("secondary", "", "secondary provider: bedrock | openai-compatible | none")
	secondaryBaseURL := fs.String("secondary-base-url", "", "base URL for an openai-compatible secondary, e.g. https://api.together.xyz/v1 or https://openrouter.ai/api/v1")
	secondaryModel := fs.String("secondary-model", "", "target model for an openai-compatible secondary, e.g. zai-org/GLM-5.3 or z-ai/glm-5.3")
	secondaryKeychainService := fs.String("secondary-keychain-service", "", "keychain service name for an openai-compatible secondary, e.g. claude-burst-openrouter (default claude-burst-together, for backward compatibility) -- also fixes the API-key env var keychain-set reads, e.g. claude-burst-openrouter -> OPENROUTER_API_KEY")
	minFailures := fs.Int("metered-min-failures", 0, "consecutive-window failures before failing over in anthropic-api-key mode")
	windowSeconds := fs.Int("metered-window-seconds", 0, "sliding window in seconds for metered failover")
	interceptMode := fs.String("intercept-mode", "", "how Claude Code reaches the gateway: base-url (default) | transparent")
	interceptHost := fs.String("intercept-host", "", "hostname to intercept in transparent mode (default api.anthropic.com)")
	_ = fs.Parse(args)

	if *region != "" {
		u := "https://bedrock-runtime." + *region + ".amazonaws.com/anthropic"
		cfg.BedrockBaseURL = u
		if cfg.Secondary.Provider == "bedrock" {
			cfg.Secondary.BaseURL = u
		}
	}
	if *bedrockBase != "" {
		u := strings.TrimRight(*bedrockBase, "/")
		cfg.BedrockBaseURL = u
		if cfg.Secondary.Provider == "bedrock" {
			cfg.Secondary.BaseURL = u
		}
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *adminHostname == "off" {
		cfg.AdminHostname = ""
	} else if *adminHostname != "" {
		cfg.AdminHostname = strings.ToLower(*adminHostname)
	}
	if *adminListen == "off" {
		cfg.AdminListen = ""
	} else if *adminListen != "" {
		cfg.AdminListen = *adminListen
	}
	if *primary != "" {
		baseURL, strategy, err := baseURLForProvider(cfg, *primary)
		if err != nil {
			fatal(fmt.Errorf("invalid --primary: %w", err))
		}
		cfg.Primary = config.RouteConfig{Provider: *primary, BaseURL: baseURL, FailoverStrategy: strategy}
	}
	if *failoverStrategy != "" {
		switch *failoverStrategy {
		case "subscription-limit", "metered-failures", "subscription-limit+metered-failures", "none":
			cfg.Primary.FailoverStrategy = *failoverStrategy
		default:
			fatal(fmt.Errorf("invalid --failover-strategy %q (must be subscription-limit, metered-failures, subscription-limit+metered-failures, or none)", *failoverStrategy))
		}
	}
	if *secondary != "" {
		switch *secondary {
		case "none":
			// Explicit marker, not the zero value: see config.ProviderNone.
			cfg.Secondary = config.RouteConfig{Provider: config.ProviderNone}
			cfg.BedrockBaseURL = ""
		case "openai-compatible":
			base := *secondaryBaseURL
			if base == "" {
				base = cfg.Secondary.BaseURL // allow re-running configure without repeating it
			}
			model := *secondaryModel
			if model == "" {
				model = cfg.Secondary.Model
			}
			if base == "" || model == "" {
				fatal(fmt.Errorf("--secondary openai-compatible requires --secondary-base-url and --secondary-model"))
			}
			ks := *secondaryKeychainService
			if ks == "" {
				ks = cfg.Secondary.KeychainService // allow re-running configure without repeating it
			}
			if ks == "" {
				ks = "claude-burst-together" // backward-compatible default; not a hardcoded vendor requirement
			}
			cfg.Secondary = config.RouteConfig{
				Provider: "openai-compatible", BaseURL: strings.TrimRight(base, "/"), Model: model,
				KeychainService: ks,
			}
		default:
			// baseURLForProvider always derives the base URL from the
			// chosen provider's own field (cfg.AnthropicBaseURL or
			// cfg.BedrockBaseURL) rather than reusing whatever was
			// previously in cfg.Secondary.BaseURL -- so
			// `configure --secondary bedrock` can never leave a slot
			// pointed at the wrong vendor's endpoint.
			baseURL, _, err := baseURLForProvider(cfg, *secondary)
			if err != nil {
				fatal(fmt.Errorf("invalid --secondary: %w", err))
			}
			cfg.Secondary = config.RouteConfig{
				Provider: *secondary, BaseURL: baseURL,
				KeychainService: cfg.KeychainService, ModelMap: cfg.ModelMap,
			}
		}
	}
	if *minFailures > 0 {
		cfg.MeteredFailover.MinFailures = *minFailures
	}
	if *windowSeconds > 0 {
		cfg.MeteredFailover.WindowSeconds = *windowSeconds
	}
	if *interceptMode != "" {
		cfg.Intercept.Mode = *interceptMode
		if err := cfg.ValidateIntercept(); err != nil {
			fatal(fmt.Errorf("invalid --intercept-mode: %w", err))
		}
	}
	if *interceptHost != "" {
		cfg.Intercept.Host = *interceptHost
	}
	cfg.ResolveRoutes()

	if err := config.Save(cfg); err != nil {
		fatal(err)
	}
	p, _ := config.ConfigPath()
	fmt.Printf("wrote %s\n", p)
}

// baseURLForProvider derives the correct base URL and (for a primary slot)
// failover strategy for a named provider, from that provider's own
// legacy-field default -- never from whatever another slot's config
// previously held. This is the fix for a real bug/misconfiguration risk: a
// naive `cfg.Primary.BaseURL = cfg.AnthropicBaseURL` regardless of which
// provider name was chosen would let `configure --primary bedrock` point
// the Bedrock provider's Prepare() at api.anthropic.com, sending the
// Keychain-stored Bedrock credential to the wrong host.
func baseURLForProvider(cfg config.Config, provider string) (baseURL, failoverStrategy string, err error) {
	switch provider {
	case "oauth-passthrough":
		return cfg.AnthropicBaseURL, "subscription-limit", nil
	case "anthropic-api-key":
		return cfg.AnthropicBaseURL, "metered-failures", nil
	case "bedrock":
		return cfg.BedrockBaseURL, "none", nil
	default:
		return "", "", fmt.Errorf("unknown provider %q (must be oauth-passthrough, anthropic-api-key, or bedrock)", provider)
	}
}

// keychainTarget resolves which Keychain service, env var, and display
// label `keychain-set --provider <provider>` should use, given an optional
// explicit --service override. Pure and side-effect-free so the
// vendor-collision class of bug this replaced (see the doc comment below)
// has a real regression test rather than only living in a shell transcript.
//
// Earlier this derived a non-bedrock provider's default service by sniffing
// cfg.Secondary.KeychainService whenever the active secondary happened to
// be openai-compatible, on the theory that a customized service name must
// belong to whichever provider is currently configured. That reasoning only
// held back when "together" was the only possible openai-compatible
// provider; with several, it actively overwrote one provider's stored key
// with another's the first time it was exercised for real (2026-08-31, see
// commit history) -- and even fixing the guard to compare labels couldn't
// save it, because a genuinely custom service name (one not shaped
// "claude-burst-<label>") has no label to derive in the first place. An
// explicit --service flag replaces the guesswork: no config is consulted,
// so storing several providers' keys side by side and swapping which one is
// active is just independent config edits, never a Keychain write that can
// clobber a different provider's entry.
func keychainTarget(provider, explicitService, bedrockDefaultService string) (service, envVar, label string) {
	if provider == "bedrock" {
		service = explicitService
		if service == "" {
			service = bedrockDefaultService // cfg.KeychainService -- a real, documented config.json field, unlike the openai-compatible case below
		}
		return service, "AWS_BEARER_TOKEN_BEDROCK", "Bedrock"
	}
	service = explicitService
	if service == "" {
		service = "claude-burst-" + provider
	}
	return service, router.EnvVarForProvider(provider), provider
}

func keychainSet(args []string) {
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	fs := flag.NewFlagSet("keychain-set", flag.ExitOnError)
	provider := fs.String("provider", "bedrock", "which secret to store: bedrock, or the vendor label of an openai-compatible secondary (e.g. together, openrouter)")
	serviceFlag := fs.String("service", "", "Keychain service name to store under (default claude-burst-<provider>, or cfg.keychain_service for bedrock) -- set explicitly to reuse a customized name; never inferred from the currently-configured secondary")
	_ = fs.Parse(args)

	service, envVar, label := keychainTarget(*provider, *serviceFlag, cfg.KeychainService)

	key := os.Getenv(envVar)
	if key == "" {
		fatal(fmt.Errorf("%s is not set", envVar))
	}
	if err := keychain.Store(service, key); err != nil {
		fatal(err)
	}
	fmt.Printf("stored %s key in macOS Keychain service %q\n", label, service)
}

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
	st := srv.Status()
	if st.OverflowUntil > time.Now().Unix() {
		fmt.Printf("route: SECONDARY (%s) overflow\nuntil: %s\nclaim: %s\nreason: %s\n",
			cfg.Secondary.Provider, time.Unix(st.OverflowUntil, 0).Format(time.RFC3339), st.LimitClaim, st.LastReason)
	} else {
		fmt.Printf("route: PRIMARY (%s)\n", cfg.Primary.Provider)
	}
	scheme := "http"
	if cfg.Intercept.Transparent() {
		scheme = "https"
	}
	fmt.Printf("gateway: %s://%s\nprimary: %s (%s)\nsecondary: %s (%s)\n",
		scheme, cfg.Listen, cfg.Primary.Provider, cfg.Primary.BaseURL, cfg.Secondary.Provider, cfg.Secondary.BaseURL)
	reportIntercept(cfg)
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
	hostsEntry := herr == nil && strings.Contains(string(hosts), "# BEGIN claude-burst hosts")
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

func reset() {
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
	srv.ClearOverflow()
	fmt.Println("overflow state cleared; next inference request will try the primary provider")
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
	statePath, _ := config.StatePath()
	metricsPath, _ := config.MetricsPath()
	srv, err := router.New(cfg, statePath, metricsPath, log.New(os.Stderr, "", 0))
	if err != nil {
		fatal(err)
	}
	until := srv.ForceOverflow(time.Duration(*minutes)*time.Minute, "forced from the CLI")
	fmt.Printf("inference now goes to %s (%s) until %s\nback to primary at any time with: claude-burst reset\n",
		cfg.Secondary.Provider, cfg.Secondary.Model, until.Format(time.RFC3339))
}

func stats(args []string) {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	days := fs.Int("days", 30, "days to summarize (0 = all)")
	_ = fs.Parse(args)
	p, _ := config.MetricsPath()
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

// settingsPath returns ~/.claude/settings.json.
func settingsPath() string {
	h, err := os.UserHomeDir()
	if err != nil {
		fatal(err)
	}
	return filepath.Join(h, ".claude", "settings.json")
}

// readSettings parses settings.json into a generic map so unknown keys survive
// a round trip. Note that Go marshals map keys in sorted order, so rewriting
// this file reorders it -- harmless, but it is why the file looks churned
// after an enable.
func readSettings(p string) map[string]any {
	root := map[string]any{}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return root
	}
	if err != nil {
		fatal(err)
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &root); err != nil {
			fatal(fmt.Errorf("refusing to edit invalid %s: %w", p, err))
		}
	}
	return root
}

func writeSettings(p string, root map[string]any) {
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		fatal(err)
	}
	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile(p, append(b, '\n'), 0600); err != nil {
		fatal(err)
	}
}

// clearBaseURL removes our ANTHROPIC_BASE_URL if it points at this gateway,
// leaving any value the user set deliberately. Matching the configured listen
// address as well as the loopback prefix matters: with a non-loopback --listen
// the old prefix-only check left the key stranded, silently keeping Claude
// Code pointed at a gateway the user had just disabled.
func clearBaseURL(root map[string]any, listen string) bool {
	env, _ := root["env"].(map[string]any)
	if env == nil {
		return false
	}
	v, ok := env["ANTHROPIC_BASE_URL"].(string)
	if !ok {
		return false
	}
	ours := v == "http://"+listen || v == "https://"+listen || strings.HasPrefix(v, "http://127.0.0.1:")
	if !ours {
		return false
	}
	delete(env, "ANTHROPIC_BASE_URL")
	if len(env) == 0 {
		delete(root, "env")
	} else {
		root["env"] = env
	}
	return true
}

func enable(args []string) {
	rejectArgs("enable", args)
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	p := settingsPath()
	root := readSettings(p)

	if cfg.Intercept.Transparent() {
		// Transparent mode's entire purpose is that this variable stays unset:
		// Claude Code disables Remote Control whenever it names another host.
		// Setting it here would silently defeat the feature.
		if clearBaseURL(root, cfg.Listen) {
			writeSettings(p, root)
			fmt.Printf("removed ANTHROPIC_BASE_URL from %s (transparent mode needs it unset)\n", p)
		} else {
			fmt.Printf("ANTHROPIC_BASE_URL already unset in %s\n", p)
		}

		_, caPEM, err := tlsca.LoadOrCreate(cfg.Intercept.CADir, cfg.Intercept.Host)
		if err != nil {
			fatal(err)
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

	env, _ := root["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	env["ANTHROPIC_BASE_URL"] = "http://" + cfg.Listen
	// Important: do NOT set a gateway API credential here. In
	// oauth-passthrough mode this preserves the saved Max OAuth
	// subscription; in anthropic-api-key mode, Claude Code's own
	// ANTHROPIC_API_KEY (set separately) is what gets forwarded unchanged.
	root["env"] = env
	writeSettings(p, root)
	fmt.Printf("enabled Claude Burst in %s\nRestart Claude Code.\n", p)
}

func disable(args []string) {
	rejectArgs("disable", args)
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	p := settingsPath()
	if _, err := os.Stat(p); os.IsNotExist(err) && !cfg.Intercept.Transparent() {
		fmt.Println("already disabled")
		return
	}
	root := readSettings(p)
	if clearBaseURL(root, cfg.Listen) {
		writeSettings(p, root)
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
	fmt.Println("disabled Claude Burst; restart Claude Code")
}

// rootHelperPath locates transparent-root.sh for the instructions we print.
//
// The binary is installed to ~/.local/bin but the script lives in the repo, so
// deriving the path from the executable alone printed a command that does not
// exist -- an instruction the user cannot run is worse than no instruction,
// because they trust it and then have to work out why it failed.
func rootHelperPath() string {
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "transparent-root.sh"))
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, "scripts", "transparent-root.sh"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, "Desktop", "github", "claude-burst", "scripts", "transparent-root.sh"))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	// Nothing found: name the file rather than a path that would not work.
	return "scripts/transparent-root.sh (in the claude-burst repo)"
}

// portOf returns the port from a host:port listen address.
func portOf(listen string) string {
	if _, port, ok := strings.Cut(listen, ":"); ok {
		return port
	}
	return listen
}

// rejectArgs fails on stray arguments. These subcommands take none, and
// parsing nothing meant `claude-burst enable --help` silently EXECUTED enable
// instead of printing help.
func rejectArgs(cmd string, args []string) {
	if len(args) > 0 {
		fatal(fmt.Errorf("%s takes no arguments, got %q", cmd, strings.Join(args, " ")))
	}
}

func fatal(err error) { fmt.Fprintln(os.Stderr, "error:", err); os.Exit(1) }

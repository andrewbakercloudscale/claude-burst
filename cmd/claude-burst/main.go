package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/keychain"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
	"github.com/andrewbakercloudscale/claude-burst/internal/router"
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
		enable()
	case "disable":
		disable()
	case "status":
		status()
	case "reset":
		reset()
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
  reset             Clear overflow state immediately
  stats             Summarize local routing/token metrics
  version           Print version

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
	logger.Printf("claude-burst %s listening on %s", version, cfg.Listen)
	fmt.Printf("claude-burst %s listening on http://%s\n", version, cfg.Listen)
	fmt.Printf("primary: %s (%s)\nsecondary: %s (%s)\n", cfg.Primary.Provider, cfg.Primary.BaseURL, cfg.Secondary.Provider, cfg.Secondary.BaseURL)
	if err := http.ListenAndServe(cfg.Listen, srv); err != nil {
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
	bedrockBase := fs.String("bedrock-base-url", "", "override Bedrock Anthropic Messages base URL")
	primary := fs.String("primary", "", "primary provider: oauth-passthrough | anthropic-api-key")
	secondary := fs.String("secondary", "", "secondary provider: bedrock | none")
	minFailures := fs.Int("metered-min-failures", 0, "consecutive-window failures before failing over in anthropic-api-key mode")
	windowSeconds := fs.Int("metered-window-seconds", 0, "sliding window in seconds for metered failover")
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
	if *primary != "" {
		baseURL, strategy, err := baseURLForProvider(cfg, *primary)
		if err != nil {
			fatal(fmt.Errorf("invalid --primary: %w", err))
		}
		cfg.Primary = config.RouteConfig{Provider: *primary, BaseURL: baseURL, FailoverStrategy: strategy}
	}
	if *secondary != "" {
		if *secondary == "none" {
			cfg.Secondary = config.RouteConfig{}
			cfg.BedrockBaseURL = ""
		} else {
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

func keychainSet(args []string) {
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	key := os.Getenv("AWS_BEARER_TOKEN_BEDROCK")
	if key == "" {
		fatal(fmt.Errorf("AWS_BEARER_TOKEN_BEDROCK is not set"))
	}
	if err := keychain.Store(cfg.KeychainService, key); err != nil {
		fatal(err)
	}
	fmt.Printf("stored Bedrock key in macOS Keychain service %q\n", cfg.KeychainService)
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
	fmt.Printf("gateway: http://%s\nprimary: %s (%s)\nsecondary: %s (%s)\n",
		cfg.Listen, cfg.Primary.Provider, cfg.Primary.BaseURL, cfg.Secondary.Provider, cfg.Secondary.BaseURL)
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

func enable() {
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	h, err := os.UserHomeDir()
	if err != nil {
		fatal(err)
	}
	p := filepath.Join(h, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		fatal(err)
	}
	root := map[string]any{}
	if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
		if err := json.Unmarshal(b, &root); err != nil {
			fatal(fmt.Errorf("refusing to edit invalid %s: %w", p, err))
		}
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
	b, _ := json.MarshalIndent(root, "", "  ")
	if err := os.WriteFile(p, append(b, '\n'), 0600); err != nil {
		fatal(err)
	}
	fmt.Printf("enabled Claude Burst in %s\nRestart Claude Code.\n", p)
}

func disable() {
	h, err := os.UserHomeDir()
	if err != nil {
		fatal(err)
	}
	p := filepath.Join(h, ".claude", "settings.json")
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		fmt.Println("already disabled")
		return
	}
	if err != nil {
		fatal(err)
	}
	root := map[string]any{}
	if err := json.Unmarshal(b, &root); err != nil {
		fatal(err)
	}
	env, _ := root["env"].(map[string]any)
	if env != nil {
		if v, ok := env["ANTHROPIC_BASE_URL"].(string); ok && strings.HasPrefix(v, "http://127.0.0.1:") {
			delete(env, "ANTHROPIC_BASE_URL")
		}
		if len(env) == 0 {
			delete(root, "env")
		} else {
			root["env"] = env
		}
	}
	out, _ := json.MarshalIndent(root, "", "  ")
	if err := os.WriteFile(p, append(out, '\n'), 0600); err != nil {
		fatal(err)
	}
	fmt.Println("disabled Claude Burst; restart Claude Code")
}

func fatal(err error) { fmt.Fprintln(os.Stderr, "error:", err); os.Exit(1) }

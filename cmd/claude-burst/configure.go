package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/keychain"
	"github.com/andrewbakercloudscale/claude-burst/internal/router"
)

func configure(args []string) {
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
	interceptMode := fs.String("intercept-mode", "", "how Claude Code reaches the gateway: transparent (what install.sh sets up) | base-url")
	interceptHost := fs.String("intercept-host", "", "hostname to intercept in transparent mode (default api.anthropic.com)")
	keepAwake := fs.String("keep-awake-lid-closed", "", "true | false: keep the Mac (and Claude Code in Ghostty, and Remote Control) running with the lid shut")
	keepAwakePower := fs.String("keep-awake-power", "", "when --keep-awake-lid-closed applies: ac (default, only while plugged in) | always")
	keepAwakeIdle := fs.Int("keep-awake-idle-minutes", -1, "stay awake with the lid shut only this many minutes after Claude Code was last used or the lid was last open; 0 = as long as the power mode applies")
	_ = fs.Parse(args)

	// Under config.Update's lock: `configure` racing a dashboard save would
	// otherwise drop whichever change landed first. See config.Update.
	var saved config.Config
	err := config.Update(func(cfg *config.Config) error {
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
			baseURL, strategy, err := baseURLForProvider(*cfg, *primary)
			if err != nil {
				return fmt.Errorf("invalid --primary: %w", err)
			}
			cfg.Primary = config.RouteConfig{Provider: *primary, BaseURL: baseURL, FailoverStrategy: strategy}
		}
		if *failoverStrategy != "" {
			switch *failoverStrategy {
			case "subscription-limit", "metered-failures", "subscription-limit+metered-failures", "none":
				cfg.Primary.FailoverStrategy = *failoverStrategy
			default:
				return fmt.Errorf("invalid --failover-strategy %q (must be subscription-limit, metered-failures, subscription-limit+metered-failures, or none)", *failoverStrategy)
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
					return fmt.Errorf("--secondary openai-compatible requires --secondary-base-url and --secondary-model")
				}
				ks := *secondaryKeychainService
				if ks == "" && cfg.Secondary.Provider == "openai-compatible" {
					// Only carried forward when the slot was ALREADY
					// openai-compatible. Inheriting it from any secondary hands
					// this provider the previous vendor's credential name --
					// switching from bedrock derived "claude-burst-bedrock",
					// i.e. $BEDROCK_API_KEY -- which is the vendor collision
					// keychainTarget's doc comment below describes, one slot
					// further along.
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
				baseURL, _, err := baseURLForProvider(*cfg, *secondary)
				if err != nil {
					return fmt.Errorf("invalid --secondary: %w", err)
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
				return fmt.Errorf("invalid --intercept-mode: %w", err)
			}
		}
		if *interceptHost != "" {
			cfg.Intercept.Host = *interceptHost
		}
		if *keepAwake != "" {
			switch *keepAwake {
			case "true":
				cfg.KeepAwakeLidClosed = true
			case "false":
				cfg.KeepAwakeLidClosed = false
			default:
				return fmt.Errorf("invalid --keep-awake-lid-closed %q (must be true or false)", *keepAwake)
			}
		}
		if *keepAwakePower != "" {
			if err := config.ValidateKeepAwakePower(*keepAwakePower); err != nil {
				return fmt.Errorf("invalid --keep-awake-power: %w", err)
			}
			cfg.KeepAwakeLidClosedPower = *keepAwakePower
		}
		if *keepAwakeIdle >= 0 {
			if *keepAwakeIdle > config.MaxKeepAwakeIdleMinutes {
				return fmt.Errorf("invalid --keep-awake-idle-minutes %d (0 to %d)", *keepAwakeIdle, config.MaxKeepAwakeIdleMinutes)
			}
			cfg.KeepAwakeIdleMinutes = *keepAwakeIdle
		}
		cfg.ResolveRoutes()
		saved = *cfg
		return nil
	})
	if err != nil {
		fatal(err)
	}
	cfg := saved
	p, _ := config.ConfigPath()
	fmt.Printf("wrote %s\n", p)
	// A power-mode change only needs applying while the feature is on; while
	// it is off it is just remembered for the next time it is switched on.
	if *keepAwake != "" || ((*keepAwakePower != "" || *keepAwakeIdle >= 0) && cfg.KeepAwakeLidClosed) {
		applyKeepAwake(cfg.KeepAwakeLidClosed, cfg.KeepAwakeLidClosedPower, cfg.KeepAwakeIdleMinutes)
	}
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

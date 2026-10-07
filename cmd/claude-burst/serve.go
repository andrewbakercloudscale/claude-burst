package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/admin"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/hotspot"
	"github.com/andrewbakercloudscale/claude-burst/internal/logline"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
	"github.com/andrewbakercloudscale/claude-burst/internal/rotate"
	"github.com/andrewbakercloudscale/claude-burst/internal/router"
	"github.com/andrewbakercloudscale/claude-burst/internal/tlsca"
	"github.com/andrewbakercloudscale/claude-burst/internal/tlswatch"
)

// claude-burst.log had no rotation before this and grew forever for as long
// as the gateway ran, which for a LaunchAgent means indefinitely. Not (yet)
// configurable via config.json -- generous defaults for a single-machine
// personal gateway (current + logMaxBackups old files, 200MB total circular
// budget -- oldest file is discarded as soon as a new one would exceed it,
// per rotate.shiftBackups), and a config field is cheap to add later if
// tuning this turns out to matter. Bumped from 10MB/5 backups (60MB total)
// once router.go started logging a network-snapshot line on every transport
// error, which pushed real incident windows past the old budget.
// metrics.jsonl has the same fix, with its own same-shaped constants next
// to its Writer in internal/metrics -- that package owns its own rotation
// rather than taking these as parameters, since it has exactly one real
// caller (this file) and every other reference is a test constructing a
// Writer directly.
const (
	logMaxBytes   = 10 * 1024 * 1024
	logMaxBackups = 19 // 20 files * 10MB = 200MB total
)

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
	// Local time, not UTC. The dashboard renders every timestamp local, and a log
	// file in UTC beside it is a trap: on 2026-09-08 the log's newest line read
	// 06:11 while the requests table read 08:11, which looks exactly like a
	// logger that has stopped. It had not -- the machine is UTC+2. One clock,
	// the reader's.
	//
	// logline.Writer starts every line with that local time and its zone,
	// and the line's level. The standard logger goes to the same file: a
	// stray log.Printf used to land, undated by zone, in launchd.err.log.
	logOut := &logline.Writer{W: rotate.NewWriter(logPath, logMaxBytes, logMaxBackups)}
	logger := log.New(logOut, "", 0)
	log.SetFlags(0)
	log.SetOutput(logOut)
	srv, err := router.New(cfg, statePath, metricsPath, logger)
	if err != nil {
		fatal(err)
	}
	metrics.SetPricer(srv.PriceTokens)

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
		} else if !strings.Contains(string(b), strings.TrimSpace(string(caPEM))) {
			// The CA was regenerated (expiry, a changed intercept host, or
			// ca-rotate): Claude Code sessions started from now on need the
			// new one in the bundle. Sessions already running keep the
			// bundle they read at startup and must be restarted (see tlsca).
			if err := tlsca.EnsureInBundle(cfg.Intercept.CABundle, caPEM); err != nil {
				logger.Printf("WARNING: the CA was renewed but %s could not be updated: %v. Run: claude-burst enable", cfg.Intercept.CABundle, err)
			} else {
				logger.Printf("CA renewed: updated the Claude Burst block in %s; the System keychain still needs: sudo scripts/trust-ca-systemwide.sh", cfg.Intercept.CABundle)
			}
		}
	}

	// net.Listen is split out from Serve (below) rather than calling the
	// combined ListenAndServe[TLS] deliberately: that call is blocking, so
	// there was previously no way to distinguish "the bind itself failed or
	// was slow" from "the process is up and listening, but something
	// downstream (the network, pf) is why a client still can't reach it" --
	// exactly the ambiguity that made an intermittent, hard-to-reproduce
	// connectivity gap to 127.0.0.1:7777 impossible to root-cause from logs
	// alone (2026-08-31). The log line below now means what it says: the
	// listener is bound and the kernel will accept connections on this
	// socket from this timestamp on, not "about to try."
	// A pass-through left by disable or rollback holds this port; Burst being
	// started again means it is wanted back in front.
	stopPassthrough(logger)
	// A leftover holding either port (portclaim.go). The dashboard's port
	// is not worth refusing to start over: its server says why it stopped.
	if launchedByAgent() {
		if err := claimPort(cfg.Listen, logger); err != nil {
			logger.Printf("FATAL: %v", err)
			fatal(err)
		}
		if cfg.AdminListen != "" {
			if err := claimPort(cfg.AdminListen, logger); err != nil {
				logger.Printf("port claim: %v", err)
			}
		}
	}
	bindStart := time.Now()
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		logger.Printf("FATAL: failed to bind %s after %s: %v", cfg.Listen, time.Since(bindStart), err)
		fatal(fmt.Errorf("bind %s: %w", cfg.Listen, err))
	}
	logger.Printf("claude-burst %s bound %s (%s) in %s -- accepting connections now", version, cfg.Listen, scheme, time.Since(bindStart))
	if dir, err := config.ConfigDir(); err == nil {
		notice.SetDefault(notice.New(notice.Path(dir), logger))
		// The stderr log: say when the last run ended in a crash, keep the
		// file to a size, and date what this run writes to it.
		for _, name := range []string{"launchd.err.log", "launchd.out.log"} {
			if trimmed, err := trimStderrLog(filepath.Join(dir, name), stderrLogMax, stderrLogKeep); err != nil {
				logger.Printf("warn stage=startup could not cut back %s: %v", name, err)
			} else if trimmed {
				logger.Printf("startup: %s was over %dMB and was cut back to its last %dMB", name, stderrLogMax>>20, stderrLogKeep>>20)
			}
		}
		if crash := previousCrash(filepath.Join(dir, "launchd.err.log")); crash != "" {
			logger.Printf("error stage=startup the previous run ended in a crash: %s (the stack is in launchd.err.log, before this run's start line)", crash)
			notice.Record("crash", notice.Warn, "The gateway crashed and was restarted", crash+". The stack is in launchd.err.log.")
		}
		fmt.Fprintln(os.Stderr, startLine(version, time.Now()))
		srv.ResumeAlerts()
		announceReady(version)
	}
	if os.Getenv("CLAUDE_BURST_LOG_TLS_PEERS") != "" {
		ln = &peerLoggingListener{Listener: ln, logger: logger}
		logger.Print("peer-log: diagnostic peer attribution ENABLED via CLAUDE_BURST_LOG_TLS_PEERS (lsof per connection)")
	}
	fmt.Printf("claude-burst %s listening on %s://%s\n", version, scheme, cfg.Listen)
	fmt.Printf("primary: %s (%s)\nsecondary: %s (%s)\n", cfg.Primary.Provider, cfg.Primary.BaseURL, cfg.Secondary.Provider, cfg.Secondary.BaseURL)
	if cfg.Intercept.Transparent() {
		fmt.Printf("intercept: transparent (serving TLS for %s)\n", cfg.Intercept.Host)
	}

	// Counts the client side of every TLS handshake. Without it a Claude
	// Code session that distrusts the gateway's certificate is visible only
	// as stderr lines nothing reads (the 2026-10-02 CA rotation). See tlswatch.
	var handshakes *tlswatch.Watcher
	if tlsConfig != nil {
		handshakes = tlswatch.New(os.Stderr)
	}

	// Before the dashboard, which reports on it.
	codexGW := startCodexGateway(cfg, logger)

	if cfg.AdminListen != "" {
		a := admin.New(srv, metricsPath, version, cfg.AdminHostname, rootHelperPath())
		a.SetHandshakes(handshakes)
		a.SetCodex(codexGW, codexStartErr)
		go a.StartNotifier(context.Background())
		go a.StartLearner(context.Background())
		if err := admin.SyncPromptNoticeHook(cfg); err != nil {
			logger.Printf("error stage=prompt_notice_hook err=%v", err)
		}
		if err := admin.SyncCoordinationHooks(cfg); err != nil {
			logger.Printf("error stage=coordination_hooks err=%v", err)
		}
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
	// Joins the chosen hotspot when offline; idle unless one is chosen.
	go superviseHotspot(logger)
	go srv.WatchNetwork(context.Background())
	// ReadHeaderTimeout bounds only the request headers, never a long
	// streaming reply, so it cannot cut a slow model off; it stops a client
	// that opens a connection and never finishes its headers from holding it.
	server := &http.Server{Addr: cfg.Listen, Handler: srv, TLSConfig: tlsConfig, ReadHeaderTimeout: 30 * time.Second}
	if handshakes != nil {
		// Same destination and format as net/http's default (the standard
		// logger on stderr), so launchd.err.log reads exactly as before.
		server.ErrorLog = log.New(handshakes, "", log.LstdFlags)
		server.ConnState = handshakes.ConnState
	}
	inflight := srv.InFlight
	if codexGW != nil {
		inflight = func() int64 { return srv.InFlight() + codexGW.InFlight() }
	}
	go exitWhenIdleOnSignal(inflight, logger, srv.SaveCompaction)
	if tlsConfig != nil {
		err = server.ServeTLS(ln, "", "") // certificates come from TLSConfig; ln is already bound above
	} else {
		err = server.Serve(ln)
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Printf("FATAL: server stopped accepting on %s after being bound %s: %v", cfg.Listen, time.Since(bindStart), err)
		fatal(err)
	}
}

// superviseHotspot runs the hotspot watcher, restarting it a minute after a
// panic rather than letting one bad check take the whole gateway down: a
// goroutine's panic is not recovered by anything else in the process.
func superviseHotspot(logger *log.Logger) {
	for {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Printf("hotspot watcher PANIC err=%v (restarting in 1m)\n%s", rec, debug.Stack())
				}
			}()
			hotspot.Watch(context.Background())
		}()
		time.Sleep(time.Minute)
	}
}

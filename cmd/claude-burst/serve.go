package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
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
	// What this run could not use and started without. Each is logged here
	// and put on screen once the notices have somewhere to go.
	var problems []router.StartupProblem
	cfg, err := config.Load()
	if err != nil {
		fixed, found, ok := startableConfig(cfg, err)
		if !ok {
			fatal(err)
		}
		cfg, problems = fixed, found
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	// A misspelt key is ignored and its default applies, which until now
	// nothing said.
	if keys, _ := config.UnknownKeys(); len(keys) > 0 {
		problems = append(problems, router.StartupProblem{
			Title:  "config.json has settings Burst does not read",
			Detail: config.UnknownKeysMessage(keys),
		})
	}
	if !config.LoopbackListen(cfg.Listen) {
		// Warned about, not refused. Listening on every interface is a
		// setup the request guard has rules for (router/guard.go), so a
		// gateway already run that way would stop carrying its sessions the
		// day it was upgraded; but the docs say not to, and nothing said so
		// at the one moment it could be seen.
		problems = append(problems, router.StartupProblem{
			Title:  "The gateway is listening beyond this Mac",
			Detail: fmt.Sprintf("listen is %q, which is not a loopback address: other machines on the network can reach the gateway, which has no login, and send requests on your credential. Set it back with: claude-burst configure --listen 127.0.0.1:7777", cfg.Listen),
		})
	}
	// launchd.err.log, when launchd started this: see stderrLog.
	configDir, _ := config.ConfigDir()
	errOut := newStderrLog(os.Stderr, filepath.Join(configDir, "launchd.err.log"))
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
	for _, p := range problems {
		logger.Printf("WARNING: started without a setting it could not use. %s: %s", p.Title, p.Detail)
	}
	// What is left to refuse a start over is what the primary path cannot
	// work without (the primary provider itself). A secondary that cannot be
	// built or an unknown failover strategy is started without, and comes
	// back in StartupProblems, already logged.
	srv, err := router.New(cfg, statePath, metricsPath, logger)
	if err != nil {
		logger.Printf("FATAL: %v", err)
		fatal(err)
	}
	problems = append(problems, srv.StartupProblems()...)
	metrics.SetPricer(srv.PriceTokens)
	metrics.SetLongWritePricer(srv.LongWriteExtraUSD)

	scheme := "http"
	var tlsConfig *tls.Config
	if cfg.Intercept.Transparent() {
		leaf, caPEM, err := tlsca.LoadOrCreate(cfg.Intercept.CADir, cfg.Intercept.Host)
		if err != nil {
			fatal(err)
		}
		// GetCertificate and not Certificates: the leaf lasts 397 days and a
		// LaunchAgent can run longer, so the listener asks leafSource, which
		// has a new one issued before the old one ends.
		leaves := &leafSource{
			leaf: leaf, caPEM: caPEM, now: time.Now, logger: logger,
			load: func() (*tls.Certificate, []byte, error) {
				return tlsca.LoadOrCreate(cfg.Intercept.CADir, cfg.Intercept.Host)
			},
			newCA: func(caPEM []byte) {
				notice.Publish("tls", notice.Warn, "The gateway's certificate authority was renewed",
					"Claude Code sessions already open will refuse the gateway until they are restarted. Run: claude-burst enable")
				checkCABundle(cfg.Intercept.CABundle, caPEM, logger, errOut)
			},
		}
		tlsConfig = &tls.Config{
			GetCertificate: leaves.get,
			MinVersion:     tls.VersionTLS12,
		}
		scheme = "https"
		checkCABundle(cfg.Intercept.CABundle, caPEM, logger, errOut)
		if w := tlsca.Unconstrained(cfg.Intercept.CADir, cfg.Intercept.Host); w != nil {
			logger.Printf("WARNING: %s. %s", w.Title, w.Detail)
			problems = append(problems, router.StartupProblem{Title: w.Title, Detail: w.Detail})
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
		for _, p := range problems {
			notice.Publish("startup", notice.Warn, p.Title, p.Detail)
		}
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
		// Through errOut, so it is dated the way every line of the log is;
		// previousCrash looks for the marker, wherever in the line it is.
		fmt.Fprintf(errOut, "%s%s started pid %d ===\n", startMarker, version, os.Getpid())
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
		handshakes = tlswatch.New(errOut)
	}

	// Before the dashboard, which reports on it.
	codexGW := startCodexGateway(cfg, logger)
	if codexGW != nil {
		codexGW.SetMasker(srv)
	}

	if cfg.AdminListen != "" {
		a := admin.New(srv, metricsPath, version, cfg.AdminHostname, rootHelperPath())
		a.SetHandshakes(handshakes)
		a.SetCodex(codexGW, codexStartErr)
		go background(logger, "dashboard notifier", time.Minute, func() { a.StartNotifier(context.Background()) })
		go background(logger, "compaction learner", time.Minute, func() { a.StartLearner(context.Background()) })
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
		// Not restarted after a panic: its listener may still hold the
		// port, and a dashboard that is gone is reported, where one that
		// comes and goes would not be.
		go background(logger, "dashboard server", 0, func() {
			if err := a.ListenAndServe(cfg.AdminListen); err != nil {
				logger.Printf("admin server stopped: %v", err)
				fmt.Fprintf(errOut, "admin server stopped: %v\n", err)
			}
		})
	}

	// No ReadTimeout/WriteTimeout/IdleTimeout on purpose. Claude Code's Remote
	// Control registers and then long-polls for work, holding a connection
	// open with nothing on it; a server-side deadline would sever exactly that
	// and present as Remote Control dropping repeatedly for no visible reason.
	// Joins the chosen hotspot when offline; idle unless one is chosen.
	go background(logger, "hotspot watcher", time.Minute, func() { hotspot.Watch(context.Background()) })
	go background(logger, "network watcher", time.Minute, func() { srv.WatchNetwork(context.Background()) })
	go background(logger, "idle session watcher", time.Minute, func() { srv.WatchIdleSessions(context.Background()) })
	go background(logger, "memory watcher", time.Minute, func() { watchMemory(context.Background(), logger) })
	// ReadHeaderTimeout bounds only the request headers, never a long
	// streaming reply, so it cannot cut a slow model off; it stops a client
	// that opens a connection and never finishes its headers from holding it.
	server := &http.Server{Addr: cfg.Listen, Handler: srv, TLSConfig: tlsConfig, ReadHeaderTimeout: 30 * time.Second}
	if handshakes != nil {
		// The destination net/http's default had (stderr, so
		// launchd.err.log), dated by errOut like the gateway's own log
		// where net/http's default had a format of its own.
		server.ErrorLog = log.New(handshakes, "", 0)
		server.ConnState = handshakes.ConnState
	}
	inflight := srv.InFlight
	if codexGW != nil {
		inflight = func() int64 { return srv.InFlight() + codexGW.InFlight() }
	}
	// Not restarted: a drain that died has used up the signal it waits for,
	// and launchd ends the process itself when its exit time runs out.
	go background(logger, "shutdown drain", 0, func() { exitWhenIdleOnSignal(inflight, logger, srv.SaveCompaction) })
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

// startableConfig decides whether a config.json that config.Load rejected
// can be started on all the same, and returns it with the rejected setting
// put back to its default and named.
//
// Load rejects a file for three kinds of reason. A file that cannot be read
// or parsed is not the user's configuration at all, and an unknown
// intercept.mode decides what Claude Code connects to (see
// config.ValidateIntercept): neither can be guessed around, so both still
// stop the start. The keep-awake settings are the third kind. They decide
// when the Mac may sleep, the primary path works whatever they say, and
// until 12 Oct 2026 a mistyped one kept the gateway, and with it every
// Claude Code session, from starting.
//
// The checks are the ones Load makes, repeated here because Load reports
// only the first failure and says it in text. Anything this does not
// positively recognise is not startable.
func startableConfig(cfg config.Config, loadErr error) (config.Config, []router.StartupProblem, bool) {
	var syntax *json.SyntaxError
	var badType *json.UnmarshalTypeError
	var unread *fs.PathError
	if errors.As(loadErr, &syntax) || errors.As(loadErr, &badType) || errors.As(loadErr, &unread) {
		return cfg, nil, false
	}
	if !strings.Contains(loadErr.Error(), "keep_awake") || cfg.ValidateIntercept() != nil {
		return cfg, nil, false
	}
	var found []router.StartupProblem
	if err := config.ValidateKeepAwakePower(cfg.KeepAwakeLidClosedPower); err != nil {
		found = append(found, router.StartupProblem{
			Title:  "The keep-awake power setting was ignored",
			Detail: fmt.Sprintf("%v. Running as if it were %q until config.json is corrected and the gateway restarted.", err, config.KeepAwakeOnAC),
		})
		cfg.KeepAwakeLidClosedPower = config.KeepAwakeOnAC
	}
	if n := cfg.KeepAwakeIdleMinutes; n < 0 || n > config.MaxKeepAwakeIdleMinutes {
		def := config.Default().KeepAwakeIdleMinutes
		found = append(found, router.StartupProblem{
			Title:  "The keep-awake idle time was ignored",
			Detail: fmt.Sprintf("keep_awake_idle_minutes must be 0 to %d, got %d. Running as if it were %d until config.json is corrected and the gateway restarted.", config.MaxKeepAwakeIdleMinutes, n, def),
		})
		cfg.KeepAwakeIdleMinutes = def
	}
	return cfg, found, len(found) > 0
}

// checkCABundle says so, plainly, when the local CA is not in the trust
// bundle Claude Code reads, and puts a renewed CA into a bundle that holds
// the old one.
//
// A CA that is not in the trust bundle produces a TLS error at the client
// that looks like a network fault. Corporate tooling can regenerate that
// file and drop our block, so say so plainly rather than leaving it to be
// discovered.
func checkCABundle(bundle string, caPEM []byte, logger *log.Logger, errOut io.Writer) {
	b, rerr := os.ReadFile(bundle)
	if rerr != nil || !tlsca.HasBlock(string(b)) {
		msg := fmt.Sprintf("WARNING: the local CA is not present in %s -- Claude Code will reject this gateway's certificate. Run: claude-burst enable", bundle)
		fmt.Fprintln(errOut, msg)
		logger.Print(msg)
		return
	}
	if strings.Contains(string(b), strings.TrimSpace(string(caPEM))) {
		return
	}
	// The CA was regenerated (expiry, a changed intercept host, or
	// ca-rotate): Claude Code sessions started from now on need the
	// new one in the bundle. Sessions already running keep the
	// bundle they read at startup and must be restarted (see tlsca).
	if err := tlsca.EnsureInBundle(bundle, caPEM); err != nil {
		logger.Printf("WARNING: the CA was renewed but %s could not be updated: %v. Run: claude-burst enable", bundle, err)
	} else {
		logger.Printf("CA renewed: updated the Claude Burst block in %s; the System keychain still needs: sudo scripts/trust-ca-systemwide.sh", bundle)
	}
}

const (
	// leafRenewWithin is how long before the served leaf expires the
	// gateway starts asking tlsca for the next one. It is tlsca's own
	// margin (renewBefore): asking earlier gets the same leaf back.
	leafRenewWithin = 30 * 24 * time.Hour
	// leafRecheck is how often it asks once inside that time, for as long
	// as the answer is still the old leaf or an error.
	leafRecheck = 12 * time.Hour
)

// leafSource is the TLS listener's certificate for the life of the process.
//
// The leaf was loaded once at start and served until the process ended. A
// LaunchAgent runs until something restarts it, the leaf lasts 397 days,
// and tlsca reissues one only when asked: a gateway left running past that
// would have failed every handshake at once, with an error at the client
// that reads as a network fault. Now, inside the leaf's last 30 days, a
// handshake asks tlsca again (at most every leafRecheck), which issues the
// next leaf from the same CA, so no session has anything new to trust.
type leafSource struct {
	// load is tlsca.LoadOrCreate for this gateway's directory and host.
	load func() (*tls.Certificate, []byte, error)
	// newCA is told when what load returned is signed by another CA than
	// the one this run started with (the CA itself reached its last 30
	// days, or was rotated on disk): open sessions do not trust it.
	newCA  func(caPEM []byte)
	now    func() time.Time
	logger *log.Logger

	mu      sync.Mutex
	leaf    *tls.Certificate
	caPEM   []byte
	checked time.Time
}

// get is tls.Config.GetCertificate.
func (l *leafSource) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	expires := leafExpiry(l.leaf)
	if now.Before(expires.Add(-leafRenewWithin)) || now.Sub(l.checked) < leafRecheck {
		return l.leaf, nil
	}
	l.checked = now
	next, caPEM, err := l.load()
	if err != nil {
		// The one in hand is served for as long as it lasts; a handshake
		// with it beats none.
		l.logger.Printf("error stage=tls_leaf the gateway's certificate expires %s and a new one could not be issued: %v (asking again in %s)",
			expires.Local().Format(time.RFC3339), err, leafRecheck)
		return l.leaf, nil
	}
	if len(next.Certificate) > 0 && len(l.leaf.Certificate) > 0 && bytes.Equal(next.Certificate[0], l.leaf.Certificate[0]) {
		return l.leaf, nil
	}
	l.leaf = next
	l.logger.Printf("tls: the gateway's certificate was due to expire %s; now serving a new one, valid until %s",
		expires.Local().Format(time.RFC3339), leafExpiry(next).Local().Format(time.RFC3339))
	if !bytes.Equal(bytes.TrimSpace(caPEM), bytes.TrimSpace(l.caPEM)) {
		l.caPEM = caPEM
		l.logger.Printf("WARNING: the new certificate is signed by a new CA: Claude Code sessions already open will refuse the gateway until they are restarted")
		if l.newCA != nil {
			l.newCA(caPEM)
		}
	}
	return l.leaf, nil
}

// leafExpiry is when c stops being valid; the zero time when that cannot be
// read, which get takes as already due.
func leafExpiry(c *tls.Certificate) time.Time {
	if c.Leaf != nil {
		return c.Leaf.NotAfter
	}
	if len(c.Certificate) > 0 {
		if x, err := x509.ParseCertificate(c.Certificate[0]); err == nil {
			return x.NotAfter
		}
	}
	return time.Time{}
}

// stderrCheckEvery is how much this run writes to its stderr between looks
// at the size of launchd.err.log.
const stderrCheckEvery = 256 << 10

// stderrLog is where the gateway writes its own stderr lines: the start
// line, TLS handshake errors, a warning at startup.
//
// Two things were wrong with writing them to os.Stderr directly. They were
// dated three ways (net/http's "2006/01/02 15:04:05", the start line's own
// layout, or not at all) in a file read beside a log that has one layout;
// logline.Writer gives every line that one. And launchd.err.log was cut
// back only at a start, so a gateway that ran for months through a storm of
// handshake errors (75 a minute on 2 Oct 2026) had no bound on it; the same
// cut is now made while running, whenever this run has written another
// stderrCheckEvery. Only when stderr is that file: in a terminal there is
// nothing to bound.
//
// The Go runtime's last words at a crash do not come through here, and are
// neither dated nor counted; the start line above them dates them.
type stderrLog struct {
	w         io.Writer
	path      string // launchd.err.log when w is that file, else ""
	max, keep int64

	mu      sync.Mutex
	written int64
}

// newStderrLog wraps stderr. path is where launchd.err.log would be.
func newStderrLog(stderr *os.File, path string) io.Writer {
	l := &stderrLog{w: stderr, max: stderrLogMax, keep: stderrLogKeep}
	if a, err := stderr.Stat(); err == nil {
		if b, err := os.Stat(path); err == nil && os.SameFile(a, b) {
			l.path = path
		}
	}
	return &logline.Writer{W: l}
}

func (l *stderrLog) Write(p []byte) (int, error) {
	n, err := l.w.Write(p)
	if l.path == "" {
		return n, err
	}
	l.mu.Lock()
	l.written += int64(n)
	due := l.written >= stderrCheckEvery
	if due {
		l.written = 0
	}
	l.mu.Unlock()
	if due {
		// Its error is not worth a line here: the next look tries again,
		// and the next start does too and logs it.
		_, _ = trimStderrLog(l.path, l.max, l.keep)
	}
	return n, err
}

// backgroundPause is time.Sleep, a variable so a test does not wait a minute.
var backgroundPause = time.Sleep

// background runs one of the gateway's long-lived loops so that a panic in
// it is not the end of the gateway: nothing else in the process recovers a
// goroutine's panic, and the gateway going down takes every Claude Code
// session's connection with it. Only the hotspot watcher had this.
//
// The panic is logged with its stack and put on screen. With restartAfter
// set the loop is started again after that pause, as often as it takes;
// with 0 it is left stopped, for a loop that cannot sensibly be run twice.
// A loop that returns without a panic has finished and is not restarted.
func background(logger *log.Logger, name string, restartAfter time.Duration, run func()) {
	for {
		if !panicked(logger, name, restartAfter, run) || restartAfter <= 0 {
			return
		}
		backgroundPause(restartAfter)
	}
}

// panicked runs run and reports whether it ended in a panic, having logged
// and published it.
func panicked(logger *log.Logger, name string, restartAfter time.Duration, run func()) (did bool) {
	defer func() {
		rec := recover()
		if rec == nil {
			return
		}
		did = true
		next, title := "it is not restarted", "A background task of the gateway stopped"
		if restartAfter > 0 {
			next, title = "restarting in "+restartAfter.String(), "A background task of the gateway failed and was restarted"
		}
		logger.Printf("%s PANIC err=%v (%s)\n%s", name, rec, next, debug.Stack())
		notice.Publish("background", notice.Warn, title,
			fmt.Sprintf("The %s failed (%v): %s. Requests are still served. The stack is in claude-burst.log.", name, rec, next))
	}()
	run()
	return false
}

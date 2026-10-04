package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// The pass-through: what keeps running Claude Code sessions alive after
// Claude Burst is turned off.
//
// In base-url mode each Claude Code session reads ANTHROPIC_BASE_URL once,
// when it starts, and keeps sending to the gateway's port for the rest of
// its life. disable only changes the setting for sessions started later, and
// rollback.sh and the dashboard's Revert stopped the gateway as well, so
// every session still open lost its API at once: "all apis are failing" on
// a second laptop, 2026-10-04, fixable only by restarting each session.
//
// So turning Burst off now leaves this in the gateway's place: a bare
// reverse proxy on the same port that forwards every request unchanged to
// where Claude Code would go without Burst (an adopted corporate gateway,
// else api.anthropic.com). No failover, compaction, masking or logging of
// content: Burst is off. It exits on its own once nothing has used it for
// passthroughIdle, and a gateway that starts stops it first (serve).
//
// Transparent mode needs none of this: once the redirect is removed, running
// sessions reach api.anthropic.com directly with no restart.

const passthroughIdle = 60 * time.Minute

// passthroughTick is how often idleness is checked; a variable for tests.
var passthroughTick = 15 * time.Second

// passthroughBindWait is how long a pass-through waits for the gateway to let
// go of the port. rollback.sh starts it before its sudo prompts, so this
// covers a person typing a password as well as the gateway's 50s drain.
const passthroughBindWait = 10 * time.Minute

// homeIsThisUsers reports whether HOME is the account's real home directory.
// The steps below act on the Mac rather than on HOME: a detached process on
// the configured port, and launchctl on the gui/<uid> gateway agent. Tests
// run disable and install.sh with a temporary HOME, which does not stop
// either, and on 2026-10-04 a test run left pass-throughs on this Mac's
// gateway port and booted out its live gateway: an outage until they were
// killed. A temporary HOME is not this user's Burst, so leave the Mac alone.
// Asked of Directory Services, because os/user falls back to $HOME itself.
var homeIsThisUsers = func() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	out, err := exec.Command("dscl", ".", "-read", "/Users/"+os.Getenv("USER"), "NFSHomeDirectory").Output()
	if err != nil {
		return false
	}
	real := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "NFSHomeDirectory:"))
	return real != "" && filepath.Clean(real) == filepath.Clean(home)
}

const notThisMac = "HOME is not this account's home directory (a test?): leaving this Mac's gateway alone"

func passthroughPidPath() (string, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "passthrough.pid"), nil
}

// passthroughTarget is where Claude Code sends when ANTHROPIC_BASE_URL is
// not Burst's: the gateway it had before enable adopted it, else Anthropic.
func passthroughTarget(cfg config.Config) string {
	if cfg.AdoptedBaseURL != "" {
		return strings.TrimRight(cfg.AdoptedBaseURL, "/")
	}
	return "https://api.anthropic.com"
}

// passthroughCmd is `claude-burst passthrough`. With --detach it starts the
// pass-through in its own session, so it outlives the terminal, the Revert
// window and the gateway's LaunchAgent, and returns at once.
func passthroughCmd(args []string) {
	fs := flag.NewFlagSet("passthrough", flag.ExitOnError)
	detach := fs.Bool("detach", false, "start in the background and return")
	listen := fs.String("listen", "", "address to serve (default: the gateway's)")
	target := fs.String("target", "", "where to forward (default: where Claude Code goes without Burst)")
	idle := fs.Duration("idle", passthroughIdle, "exit after this long with no requests")
	_ = fs.Parse(args)
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	if *listen == "" {
		*listen = cfg.Listen
	}
	if *target == "" {
		*target = passthroughTarget(cfg)
	}
	if *detach {
		started, why, err := startPassthrough(cfg, *listen, *target, *idle)
		if err != nil {
			fatal(err)
		}
		if started {
			fmt.Printf("pass-through starting on %s -> %s: running Claude Code sessions keep working once the gateway stops\n", *listen, *target)
		} else {
			fmt.Println("no pass-through needed:", why)
		}
		return
	}
	if err := runPassthrough(*listen, *target, *idle); err != nil {
		fatal(err)
	}
}

// startPassthrough starts a detached pass-through in base-url mode, or
// explains why none is needed. The child waits for the port, so the caller
// stops the gateway afterwards. A gateway that is already down is no reason
// to skip it: its sessions are failing now, and this is what rescues them.
func startPassthrough(cfg config.Config, listen, target string, idle time.Duration) (bool, string, error) {
	if cfg.Intercept.Transparent() {
		return false, "transparent mode: once the redirect is removed, sessions reach " + cfg.Intercept.Host + " directly", nil
	}
	if !homeIsThisUsers() {
		return false, notThisMac, nil
	}
	if pid := runningPassthrough(); pid > 0 {
		return false, fmt.Sprintf("one is already waiting (pid %d)", pid), nil
	}
	self, err := os.Executable()
	if err != nil {
		return false, "", err
	}
	dir, err := config.ConfigDir()
	if err != nil {
		return false, "", err
	}
	logf, err := os.OpenFile(filepath.Join(dir, "passthrough.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return false, "", err
	}
	defer logf.Close()
	cmd := exec.Command(self, "passthrough", "--listen", listen, "--target", target, "--idle", idle.String())
	cmd.Stdout, cmd.Stderr = logf, logf
	// Its own session: no SIGHUP when the terminal or Revert window closes,
	// and outside the gateway's process group, which launchd kills on bootout.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return false, "", err
	}
	_ = cmd.Process.Release()
	return true, "", nil
}

// runningPassthrough returns the pid of a live pass-through, or 0.
func runningPassthrough() int {
	p, err := passthroughPidPath()
	if err != nil {
		return 0
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0
	}
	// A pid file outlives a crash, and the pid can be reused: only a process
	// that is still this binary's pass-through counts.
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil || !strings.Contains(string(out), "passthrough") {
		return 0
	}
	return pid
}

// stopPassthrough asks a running pass-through to give the port back and waits
// up to 5s for it to close its listener. Requests it is still streaming
// finish; it exits after them.
func stopPassthrough(logger *log.Logger) {
	pid := runningPassthrough()
	if pid == 0 {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	p, _ := passthroughPidPath()
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(p + ".listening"); errors.Is(err, os.ErrNotExist) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	logger.Printf("stopped the pass-through (pid %d): Claude Burst is back in front", pid)
}

func runPassthrough(listen, target string, idle time.Duration) error {
	logger := log.New(os.Stdout, "passthrough ", log.LstdFlags)
	u, err := url.Parse(target)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("pass-through target %q is not a URL", target)
	}
	pidPath, err := passthroughPidPath()
	if err != nil {
		return err
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return err
	}
	defer os.Remove(pidPath)

	// Stopping while still waiting for the port is a plain exit; once
	// serving, the listener closes first so a starting gateway can bind.
	var lnp atomic.Pointer[net.Listener]
	signal.Notify(sigterm, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigterm
		gotSignal.Store(true)
		if l := lnp.Load(); l != nil {
			logger.Print("asked to stop: giving the port back")
			_ = (*l).Close()
			os.Remove(pidPath + ".listening")
		}
	}()

	ln, err := bindWhenFree(listen, passthroughBindWait, func() bool { return gotSignal.Load() })
	if err != nil {
		logger.Printf("gave up: %v", err)
		return err
	}
	lnp.Store(&ln)
	if gotSignal.Load() {
		ln.Close()
		return nil
	}
	_ = os.WriteFile(pidPath+".listening", nil, 0o600)
	defer os.Remove(pidPath + ".listening")
	logger.Printf("serving %s -> %s; exits after %s with no requests", listen, target, idle)

	var inflight atomic.Int64
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(u)
			r.Out.Host = u.Host
		},
		// Streams each server-sent event as it arrives.
		FlushInterval: -1,
		ErrorLog:      logger,
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			io.WriteString(w, `{"passthrough":true,"target":`+strconv.Quote(target)+"}\n")
			return
		}
		inflight.Add(1)
		last.Store(time.Now().UnixNano())
		defer func() {
			inflight.Add(-1)
			last.Store(time.Now().UnixNano())
		}()
		proxy.ServeHTTP(w, r)
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second}

	go func() {
		for range time.Tick(passthroughTick) {
			quiet := time.Since(time.Unix(0, last.Load()))
			if gotSignal.Load() || (inflight.Load() == 0 && quiet >= idle) {
				if !gotSignal.Load() {
					logger.Printf("no requests for %s: exiting", quiet.Round(time.Second))
				}
				// Closes the listener now, so a gateway starting can bind,
				// and lets requests still streaming finish.
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				_ = server.Shutdown(ctx)
				cancel()
				return
			}
		}
	}()
	err = server.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) || gotSignal.Load() {
		// Serve returns as soon as the listener closes; wait for streams.
		for inflight.Load() > 0 {
			time.Sleep(time.Second)
		}
		return nil
	}
	return err
}

var gotSignal atomic.Bool
var sigterm = make(chan os.Signal, 1)

// bindWhenFree binds listen, retrying while the gateway still holds it.
func bindWhenFree(listen string, wait time.Duration, stop func() bool) (net.Listener, error) {
	deadline := time.Now().Add(wait)
	for {
		ln, err := net.Listen("tcp", listen)
		if err == nil {
			return ln, nil
		}
		if stop() || time.Now().After(deadline) {
			return nil, fmt.Errorf("%s still in use after %s: %w", listen, wait, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

const gatewayLabel = "ninja.andrewbaker.claude-burst"

func rolledBackMarker() string {
	dir, _ := config.ConfigDir()
	return filepath.Join(dir, "rolled-back")
}

// handOverToPassthrough is disable's second half in base-url mode: start the
// pass-through, then stop the gateway so it can take the port. The marker
// keeps the self-heal watchdog from starting the gateway again (as after
// rollback.sh); enable clears it.
func handOverToPassthrough(cfg config.Config) {
	if !homeIsThisUsers() {
		fmt.Println("pass-through:", notThisMac)
		return
	}
	started, why, err := startPassthrough(cfg, cfg.Listen, passthroughTarget(cfg), passthroughIdle)
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not start the pass-through (%v): sessions already open need restarting (claude --resume keeps their history)\n", err)
	} else if !started {
		fmt.Println("pass-through:", why)
	}
	_ = os.WriteFile(rolledBackMarker(), []byte(time.Now().Format("2006-01-02 15:04:05")+" disabled by claude-burst disable\n"), 0o600)
	fmt.Println("stopping the gateway (replies in progress finish first, up to 50s)...")
	_ = exec.Command("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), gatewayLabel)).Run()
	if !started {
		return
	}
	c := http.Client{Timeout: 2 * time.Second}
	for i := 0; i < 20; i++ {
		if resp, err := c.Get("http://" + cfg.Listen + "/healthz"); err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if strings.Contains(string(b), `"passthrough":true`) {
				fmt.Printf("sessions already open keep working: %s now forwards straight to %s, and stops after %s unused\n",
					cfg.Listen, passthroughTarget(cfg), passthroughIdle)
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	p, _ := passthroughPidPath()
	fmt.Fprintf(os.Stderr, "the pass-through is not answering on %s yet: see %s. Sessions already open may need restarting (claude --resume keeps their history)\n",
		cfg.Listen, filepath.Join(filepath.Dir(p), "passthrough.log"))
}

// startGatewayAgent loads the gateway LaunchAgent if it is not running (after
// disable or rollback.sh), and clears the marker that kept the watchdog off.
func startGatewayAgent() {
	if !homeIsThisUsers() {
		return
	}
	m := rolledBackMarker()
	os.Remove(m)
	os.Remove(m + ".noted")
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	if exec.Command("launchctl", "print", domain+"/"+gatewayLabel).Run() == nil {
		return
	}
	home, _ := os.UserHomeDir()
	plist := filepath.Join(home, "Library", "LaunchAgents", gatewayLabel+".plist")
	if _, err := os.Stat(plist); err != nil {
		fmt.Println("no gateway LaunchAgent installed: run ./install.sh, or start one with: claude-burst serve")
		return
	}
	if out, err := exec.Command("launchctl", "bootstrap", domain, plist).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "could not start the gateway: %v %s\n", err, strings.TrimSpace(string(out)))
		return
	}
	fmt.Println("started the gateway")
}

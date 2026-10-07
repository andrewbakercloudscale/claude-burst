package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/admin"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/logline"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// consoleCmd runs the support console: `claude-burst console`. It has its
// own LaunchAgent and loads nothing the gateway needs, so a gateway that
// will not start (a broken config, a crash) still leaves a page that shows
// why and can fix it. A config that does not load falls back to defaults.
// restoreConfig is the way out of a config.json the gateway cannot read.
func restoreConfig(args []string) {
	rejectArgs("restore-config", args)
	r, err := config.RestoreLastGood()
	if errors.Is(err, config.ErrConfigLoads) {
		fmt.Println(err)
		return
	}
	if err != nil {
		fatal(err)
	}
	fmt.Printf("restored config.json from %s\nthe file that would not load is kept as %s\nthe gateway starts by itself within seconds; if not: claude-burst open\n", r.From, r.Kept)
}

func consoleCmd(args []string) {
	rejectArgs("console", args)
	cfg, err := config.Load()
	if err != nil {
		cfg = config.Default()
	}
	addr := cfg.ConsoleListen()
	if addr == "" {
		fmt.Println("console: off (console_listen is \"off\")")
		return
	}
	logger := log.New(&logline.Writer{W: os.Stderr}, "console: ", 0)
	if dir, err := config.ConfigDir(); err == nil {
		// launchd appends this process's output to these two for ever.
		for _, name := range []string{"console.err.log", "console.out.log"} {
			_, _ = trimStderrLog(filepath.Join(dir, name), stderrLogMax, stderrLogKeep)
		}
		notice.SetDefault(notice.New(notice.Path(dir), logger))
		// Burst's alerts over the Codex window too: the usage panel only
		// draws over Ghostty.
		if ca := admin.NewCodexAlerts(dir); ca != nil {
			go ca.Run(context.Background())
		}
	}
	ln, err := listenRetry(addr, logger)
	if err != nil {
		fatal(err)
	}
	logger.Printf("support console on http://%s/", addr)
	srv := &http.Server{Handler: admin.NewConsole(version, rootHelperPath()).Handler(), ReadHeaderTimeout: 10 * time.Second}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal(err)
	}
}

// listenRetry binds addr, waiting out an older console still letting go of
// it after a deploy. launchd restarts the console if this gives up.
func listenRetry(addr string, logger *log.Logger) (net.Listener, error) {
	var err error
	for i := 0; i < 20; i++ {
		var ln net.Listener
		if ln, err = net.Listen("tcp", addr); err == nil {
			return ln, nil
		}
		if i == 0 {
			logger.Printf("waiting for %s: %v", addr, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil, fmt.Errorf("console: bind %s: %w", addr, err)
}

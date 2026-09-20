package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/claudesettings"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/keychain"
	"github.com/andrewbakercloudscale/claude-burst/internal/shunt"
)

func shuntUsage() {
	fmt.Print(`claude-burst shunt - keep bulk reading and boilerplate out of the frontier model's context

  shunt enable [--read] [--write]   turn parts on (default: both); installs the skill, and the guard hook when read is on
  shunt disable [--read] [--write]  turn parts off (default: both); removes the hook when read is off, the skill when none remain
  shunt status                      what is on, whether the worker is ready, and what it has saved
  shunt doctor [--quick]            check the worker sees a whole prompt (catches silent truncation)

Used by Claude, not by you:
  shunt guard                       PreToolUse hook: refuses whole-file reads above the threshold
  shunt read --question Q FILE...   answer Q from the files with path:line citations
  shunt write --spec S --ref F --out PATH   generate a file straight to disk

Options for enable:
  --min-lines N     delegate reads at or above N lines (default 350)
  --chunk-lines N   lines per worker call (default 6000)
  --model M         worker model (default: the secondary's model)

The worker is your configured openai-compatible secondary. File contents are
sent to that provider; credential-looking files never are.
`)
}

func shuntCmd(args []string) {
	if len(args) == 0 {
		shuntUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "enable":
		shuntEnable(args[1:])
	case "disable":
		shuntDisable(args[1:])
	case "status":
		shuntStatus()
	case "doctor":
		shuntDoctor(args[1:])
	case "guard":
		shuntGuard()
	case "read":
		shuntRead(args[1:])
	case "write":
		shuntWrite(args[1:])
	case "help", "--help", "-h":
		shuntUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown shunt command %q\n\n", args[0])
		shuntUsage()
		os.Exit(2)
	}
}

func shuntEnable(args []string) {
	fs := flag.NewFlagSet("shunt enable", flag.ExitOnError)
	read := fs.Bool("read", false, "enable bulk-read delegation")
	write := fs.Bool("write", false, "enable code-write delegation")
	minLines := fs.Int("min-lines", 0, "delegate whole-file reads at or above this many lines")
	chunk := fs.Int("chunk-lines", 0, "lines per worker call")
	model := fs.String("model", "", "worker model (default: the secondary's model)")
	_ = fs.Parse(args)
	if !*read && !*write {
		*read, *write = true, true
	}

	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	// Refuse up front. Turning the guard on with no usable worker would block
	// whole-file reads and offer nothing to redirect them to.
	if *model != "" {
		cfg.Shunt.Model = *model
	}
	if err := shunt.Readiness(cfg, keychain.Describe); err != nil {
		fatal(fmt.Errorf("cannot enable the shunt: %w", err))
	}
	if *read {
		cfg.Shunt.Read = true
	}
	if *write {
		cfg.Shunt.Write = true
	}
	if *minLines > 0 {
		cfg.Shunt.MinLines = *minLines
	}
	if *chunk > 0 {
		cfg.Shunt.ChunkLines = *chunk
	}
	if err := config.Save(cfg); err != nil {
		fatal(err)
	}
	applyShunt(cfg)
	fmt.Printf("shunt on: read=%v write=%v (threshold %d lines, worker %s via %s)\n",
		cfg.Shunt.Read, cfg.Shunt.Write, cfg.Shunt.MinLinesOrDefault(), workerModel(cfg), cfg.Secondary.BaseURL)
	fmt.Println("File contents go to that provider when delegated; credential-looking files never do.")
	fmt.Println("Restart Claude Code for the hook and skill to load. Verify with: claude-burst shunt doctor")
}

func shuntDisable(args []string) {
	fs := flag.NewFlagSet("shunt disable", flag.ExitOnError)
	read := fs.Bool("read", false, "disable bulk-read delegation")
	write := fs.Bool("write", false, "disable code-write delegation")
	_ = fs.Parse(args)
	if !*read && !*write {
		*read, *write = true, true
	}
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	if *read {
		cfg.Shunt.Read = false
	}
	if *write {
		cfg.Shunt.Write = false
	}
	if err := config.Save(cfg); err != nil {
		fatal(err)
	}
	applyShunt(cfg)
	fmt.Printf("shunt: read=%v write=%v\n", cfg.Shunt.Read, cfg.Shunt.Write)
	if !cfg.Shunt.Enabled() {
		fmt.Println("hook and skill removed. Restart Claude Code.")
	} else if !cfg.Shunt.Read {
		fmt.Println("guard hook removed (only write is on). Restart Claude Code.")
	}
}

// applyShunt makes settings.json and the skill agree with config.
func applyShunt(cfg config.Config) {
	if err := shunt.Apply(cfg, shunt.SelfPath()); err != nil {
		fatal(err)
	}
}

func workerModel(cfg config.Config) string { return shunt.ModelOf(cfg) }

func shuntStatus() {
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	fmt.Print(shuntStatusText(cfg))
}

// shuntStatusText is shared by `shunt status` and `status`.
func shuntStatusText(cfg config.Config) string {
	var sb strings.Builder
	s := cfg.Shunt
	fmt.Fprintf(&sb, "shunt: read=%v write=%v threshold=%d lines chunk=%d lines\n", s.Read, s.Write, s.MinLinesOrDefault(), s.ChunkLinesOrDefault())
	if p, err := claudesettings.Path(); err == nil {
		if root, err := claudesettings.Read(p); err == nil {
			fmt.Fprintf(&sb, "shunt hook: %s\n", onOff(shunt.HookInstalled(root), "installed", "not installed"))
			if s.Read && !shunt.HookInstalled(root) {
				sb.WriteString("shunt WARNING: read is on but the guard hook is missing, so nothing is enforcing it. Run: claude-burst shunt enable\n")
			}
		}
	}
	fmt.Fprintf(&sb, "shunt skill: %s\n", onOff(shunt.SkillInstalled(), "installed", "not installed"))
	if s.Enabled() {
		if err := shunt.Readiness(cfg, keychain.Describe); err != nil {
			fmt.Fprintf(&sb, "shunt worker: NOT READY -- %v\n", err)
		} else {
			fmt.Fprintf(&sb, "shunt worker: ready (%s via %s)\n", workerModel(cfg), cfg.Secondary.BaseURL)
		}
	}
	if lp, err := config.ShuntLogPath(); err == nil {
		if sum, err := shunt.SummarizeLog(lp, time.Now().Add(-30*24*time.Hour)); err == nil {
			fmt.Fprintf(&sb, "shunt (30d): %s\n", sum)
		}
	}
	return sb.String()
}

func onOff(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

func shuntDoctor(args []string) {
	fs := flag.NewFlagSet("shunt doctor", flag.ExitOnError)
	quick := fs.Bool("quick", false, "small prompt (cheap) instead of a full chunk")
	_ = fs.Parse(args)
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	w, err := shunt.NewWorker(cfg)
	if err != nil {
		fatal(err)
	}
	lines := cfg.Shunt.ChunkLinesOrDefault()
	if *quick {
		lines = 300
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	start := time.Now()
	marker, ok, res, err := w.Doctor(ctx, lines)
	if err != nil {
		fatal(err)
	}
	usd, known := w.Cost(res.InputTokens, res.OutputTokens)
	cost := "cost unknown (no pricing entry)"
	if known {
		cost = fmt.Sprintf("$%.4f", usd)
	}
	fmt.Printf("worker %s via %s: sent %d lines (%d tokens) in %.1fs, %s\n", w.Model, w.Label, lines, res.InputTokens, time.Since(start).Seconds(), cost)
	if !ok {
		fmt.Printf("FAIL: marker %s did not come back (got %q). The endpoint is truncating long prompts, so a delegated read would answer from part of the file. Raise its context window or lower --chunk-lines.\n", marker, strings.TrimSpace(res.Text))
		os.Exit(1)
	}
	fmt.Println("OK: the worker saw the whole prompt.")
}

// shuntGuard is the PreToolUse hook. Exit 2 with a message on stderr blocks the
// tool call and hands the message to the model; any other outcome allows it.
// Every internal error therefore ends in exit 0: see shunt.Decide.
func shuntGuard() {
	in, err := shunt.ParseHookInput(os.Stdin)
	if err != nil {
		return
	}
	cfg, err := config.Load()
	if err != nil {
		return
	}
	d := shunt.Decide(in, shunt.GuardOptions{Read: cfg.Shunt.Read, MinLines: cfg.Shunt.MinLinesOrDefault(), Bin: shunt.SelfPath()})
	if !d.Deny {
		return
	}
	if lp, err := config.ShuntLogPath(); err == nil {
		_ = shunt.Append(lp, shunt.Event{Kind: shunt.KindDeny, OK: true, Files: 1, BytesIn: d.Bytes, Note: in.ToolName, Cwd: in.Cwd})
	}
	fmt.Fprintln(os.Stderr, d.Reason)
	os.Exit(2)
}

func shuntRead(args []string) {
	fs := flag.NewFlagSet("shunt read", flag.ExitOnError)
	question := fs.String("question", "", "what you need to know (required)")
	fs.StringVar(question, "q", "", "shorthand for --question")
	chunk := fs.Int("chunk-lines", 0, "override lines per worker call")
	_ = fs.Parse(args)

	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	if !cfg.Shunt.Read {
		fatal(fmt.Errorf("bulk-read delegation is off. Read the file with offset and limit windows, or run: claude-burst shunt enable --read"))
	}
	w, err := shunt.NewWorker(cfg)
	if err != nil {
		fatal(fmt.Errorf("%w. Read the file with offset and limit windows instead", err))
	}
	cl := cfg.Shunt.ChunkLinesOrDefault()
	if *chunk > 0 {
		cl = *chunk
	}
	cwd, _ := os.Getwd()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	start := time.Now()
	res, err := w.BulkRead(ctx, shunt.ReadRequest{Question: *question, Paths: fs.Args(), Cwd: cwd, ChunkLines: cl})
	logShunt(shunt.Event{Kind: shunt.KindRead, OK: err == nil, Files: res.Files, Calls: res.Calls, BytesIn: res.BytesIn,
		BytesOut: int64(len(res.Text)), InputTokens: res.InputTokens, OutputTokens: res.OutputTokens, Model: w.Model, Destination: w.Endpoint(),
		USD: res.USD, PricingUnknown: res.Unpriced, DurationMS: time.Since(start).Milliseconds(), Note: errNote(err)})
	if err != nil {
		fatal(err)
	}
	fmt.Println(res.Text)
	fmt.Println()
	fmt.Println(res.Footer(w.Model, time.Since(start)))
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func shuntWrite(args []string) {
	fs := flag.NewFlagSet("shunt write", flag.ExitOnError)
	spec := fs.String("spec", "", "what the file must do (or - to read stdin)")
	specFile := fs.String("spec-file", "", "read the specification from a file")
	out := fs.String("out", "", "path to write (required)")
	var refs multiFlag
	fs.Var(&refs, "ref", "reference file whose conventions to follow (repeatable)")
	_ = fs.Parse(args)

	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	if !cfg.Shunt.Write {
		fatal(fmt.Errorf("code-write delegation is off. Write the file yourself, or run: claude-burst shunt enable --write"))
	}
	specText := *spec
	switch {
	case *specFile != "":
		b, err := os.ReadFile(*specFile)
		if err != nil {
			fatal(err)
		}
		specText = string(b)
	case specText == "-":
		b, err := readAllStdin()
		if err != nil {
			fatal(err)
		}
		specText = string(b)
	}
	w, err := shunt.NewWorker(cfg)
	if err != nil {
		fatal(fmt.Errorf("%w. Write the file yourself instead", err))
	}
	cwd, _ := os.Getwd()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	start := time.Now()
	res, err := w.CodeWrite(ctx, shunt.WriteRequest{Spec: specText, Refs: refs, Out: *out, Cwd: cwd})
	logShunt(shunt.Event{Kind: shunt.KindWrite, OK: err == nil, Files: 1, Calls: 1, BytesOut: int64(res.Bytes),
		InputTokens: res.InputTokens, OutputTokens: res.OutputTokens, Model: w.Model, Destination: w.Endpoint(), USD: res.USD,
		PricingUnknown: res.Unpriced, DurationMS: time.Since(start).Milliseconds(), Note: errNote(err)})
	if err != nil {
		fatal(err)
	}
	fmt.Printf("wrote %s (%d lines, %d bytes) via %s in %.1fs.\n", res.Path, res.Lines, res.Bytes, w.Model, time.Since(start).Seconds())
	if res.Backup != "" {
		fmt.Printf("previous version kept at %s\n", res.Backup)
	}
	fmt.Println("The file was NOT shown to you. Review it (git diff, or Read a window) and run the tests or linter before relying on it.")
}

func logShunt(e shunt.Event) {
	if lp, err := config.ShuntLogPath(); err == nil {
		_ = config.EnsureDir()
		_ = shunt.Append(lp, e)
	}
}

// errNote keeps a failure's reason in the log without the message body, which
// can echo provider output.
func errNote(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

func readAllStdin() ([]byte, error) {
	var sb strings.Builder
	buf := make([]byte, 32*1024)
	for {
		n, err := os.Stdin.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return []byte(sb.String()), nil
}

package main

import (
	"context"
	"encoding/json"
	"errors"
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
  shunt log [-n N] [--problems] [--session ID] [--json]
                                    what happened, in plain text: refusals, delegations, failures,
                                    with project and session (--problems: only what needs a look)

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
	case "log":
		shuntLog(args[1:])
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
		// Anything that needs a look, newest last, so a looping session or a
		// failing worker is on the status page rather than buried in a log.
		since := time.Now().Add(-24 * time.Hour)
		// Folded before filtering, for the same reason `shunt log` does: a block
		// that a delegated read answered is not a problem, and only the folded
		// row knows that.
		var probs []shunt.Activity
		if evs, err := shunt.Recent(lp, 400, func(e shunt.Event) bool { return e.Time.After(since) }); err == nil {
			for _, a := range shunt.Fold(evs, time.Now()) {
				if a.IsProblem() {
					probs = append(probs, a)
					if len(probs) == 5 {
						break
					}
				}
			}
		}
		if len(probs) > 0 {
			fmt.Fprintf(&sb, "shunt PROBLEMS (last 24h, newest last; full list: claude-burst shunt log --problems):\n")
			for i := len(probs) - 1; i >= 0; i-- {
				fmt.Fprintf(&sb, "  %s\n", shuntLogLine(probs[i]))
			}
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

// guardError records that the guard could not decide and let the call through.
// It used to return silently, which made a broken guard indistinguishable from
// one that had nothing to block.
func guardError(stage, note string, in shunt.HookInput) {
	logShunt(shunt.Event{Kind: shunt.KindGuardError, OK: false, Stage: stage, Note: note, Session: in.SessionID, Cwd: in.Cwd})
}

// shuntGuard is the PreToolUse hook. Exit 2 with a message on stderr blocks the
// tool call and hands the message to the model; any other outcome allows it.
// Every internal error therefore ends in exit 0 -- see shunt.Decide -- but never
// silently: each one is logged as a guard_error so a broken guard shows up.
func shuntGuard() {
	var in shunt.HookInput
	defer func() {
		if r := recover(); r != nil {
			guardError("panic", fmt.Sprint(r), in)
		}
	}()
	in, err := shunt.ParseHookInput(os.Stdin)
	if err != nil {
		guardError(shunt.StageInput, "hook payload is not valid JSON: "+errNote(err), in)
		return
	}
	cfg, err := config.Load()
	if err != nil {
		guardError(shunt.StageConfig, errNote(err), in)
		return
	}
	threshold := cfg.Shunt.MinLinesOrDefault()
	d := shunt.Decide(in, shunt.GuardOptions{Read: cfg.Shunt.Read, MinLines: threshold, Bin: shunt.SelfPath()})
	if !d.Deny {
		return
	}

	ev := shunt.Event{Kind: shunt.KindDeny, OK: true, Session: in.SessionID, Cwd: in.Cwd,
		Tool: d.Tool, Path: d.Path, Lines: d.Lines, Threshold: threshold, BytesIn: d.Bytes, Files: 1}
	msg := d.Reason
	if lp, err := config.ShuntLogPath(); err == nil {
		// Counted BEFORE this refusal is recorded, so it means "refused this
		// many times already". A session that follows the redirect resets it.
		ev.Repeat = shunt.RepeatCount(lp, ev, time.Now())
		_ = config.EnsureDir()
		_ = shunt.Append(lp, ev)
	}
	if ev.Repeat >= 1 {
		msg = fmt.Sprintf("REFUSAL #%d of this file in this session with no answer in between. Retrying cannot succeed -- run the shunt read command below now.\n\n", ev.Repeat+1) + msg
	}
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(2)
}

// run carries what every shunt read/write log line needs, so that EVERY exit
// path records itself. Before this, the feature-off, no-worker and bad-argument
// failures called fatal() ahead of the log write and left no trace at all.
type run struct {
	ev    shunt.Event
	start time.Time
}

func newRun(kind string) *run {
	cwd, _ := os.Getwd()
	return &run{start: time.Now(), ev: shunt.Event{Kind: kind, Session: os.Getenv("CLAUDE_CODE_SESSION_ID"), Cwd: cwd}}
}

// fail logs the failure with its stage and exits with the message.
func (r *run) fail(stage string, err error) {
	r.ev.OK = false
	r.ev.Stage = stage
	if se := shunt.StageOf(err); se != "error" && stage == "" {
		r.ev.Stage = se
	}
	r.ev.Note = errNote(err)
	r.ev.DurationMS = time.Since(r.start).Milliseconds()
	logShunt(r.ev)
	fatal(err)
}

func shuntRead(args []string) {
	r := newRun(shunt.KindRead)
	fs := flag.NewFlagSet("shunt read", flag.ContinueOnError)
	question := fs.String("question", "", "what you need to know (required)")
	fs.StringVar(question, "q", "", "shorthand for --question")
	chunk := fs.Int("chunk-lines", 0, "override lines per worker call")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		r.fail(shunt.StageArgs, err)
	}
	r.ev.Paths = fs.Args()

	cfg, err := config.Load()
	if err != nil {
		r.fail(shunt.StageConfig, err)
	}
	if !cfg.Shunt.Read {
		r.fail(shunt.StageDisabled, fmt.Errorf("bulk-read delegation is off. Read the file with offset and limit windows, or run: claude-burst shunt enable --read"))
	}
	if strings.TrimSpace(*question) == "" || len(fs.Args()) == 0 {
		r.fail(shunt.StageArgs, fmt.Errorf("--question and at least one file are required: claude-burst shunt read --question \"...\" FILE..."))
	}
	w, err := shunt.NewWorker(cfg)
	if err != nil {
		r.fail(shunt.StageWorkerInit, fmt.Errorf("%w. Read the file with offset and limit windows instead", err))
	}
	r.ev.Model, r.ev.Destination = w.Model, w.Endpoint()
	cl := cfg.Shunt.ChunkLinesOrDefault()
	if *chunk > 0 {
		cl = *chunk
	}
	cwd, _ := os.Getwd()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	res, err := w.BulkRead(ctx, shunt.ReadRequest{Question: *question, Paths: fs.Args(), Cwd: cwd, ChunkLines: cl})
	r.ev.Files, r.ev.Calls, r.ev.BytesIn, r.ev.BytesOut = res.Files, res.Calls, res.BytesIn, int64(len(res.Text))
	r.ev.InputTokens, r.ev.OutputTokens, r.ev.USD, r.ev.PricingUnknown = res.InputTokens, res.OutputTokens, res.USD, res.Unpriced
	if err != nil {
		r.fail(shunt.StageOf(err), err)
	}
	r.ev.OK = true
	r.ev.DurationMS = time.Since(r.start).Milliseconds()
	logShunt(r.ev)
	fmt.Println(res.Text)
	fmt.Println()
	fmt.Println(res.Footer(w.Model, time.Since(r.start)))
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func shuntWrite(args []string) {
	r := newRun(shunt.KindWrite)
	fs := flag.NewFlagSet("shunt write", flag.ContinueOnError)
	spec := fs.String("spec", "", "what the file must do (or - to read stdin)")
	specFile := fs.String("spec-file", "", "read the specification from a file")
	out := fs.String("out", "", "path to write (required)")
	var refs multiFlag
	fs.Var(&refs, "ref", "reference file whose conventions to follow (repeatable)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		r.fail(shunt.StageArgs, err)
	}
	r.ev.Path, r.ev.Paths = *out, refs

	cfg, err := config.Load()
	if err != nil {
		r.fail(shunt.StageConfig, err)
	}
	if !cfg.Shunt.Write {
		r.fail(shunt.StageDisabled, fmt.Errorf("code-write delegation is off. Write the file yourself, or run: claude-burst shunt enable --write"))
	}
	specText := *spec
	switch {
	case *specFile != "":
		b, err := os.ReadFile(*specFile)
		if err != nil {
			r.fail(shunt.StageArgs, err)
		}
		specText = string(b)
	case specText == "-":
		b, err := readAllStdin()
		if err != nil {
			r.fail(shunt.StageArgs, err)
		}
		specText = string(b)
	}
	if strings.TrimSpace(specText) == "" || *out == "" {
		r.fail(shunt.StageArgs, fmt.Errorf("--spec and --out are required: claude-burst shunt write --spec \"...\" --ref EXAMPLE --out PATH"))
	}
	w, err := shunt.NewWorker(cfg)
	if err != nil {
		r.fail(shunt.StageWorkerInit, fmt.Errorf("%w. Write the file yourself instead", err))
	}
	r.ev.Model, r.ev.Destination = w.Model, w.Endpoint()
	cwd, _ := os.Getwd()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	res, err := w.CodeWrite(ctx, shunt.WriteRequest{Spec: specText, Refs: refs, Out: *out, Cwd: cwd})
	r.ev.Files, r.ev.Calls, r.ev.BytesOut = 1, 1, int64(res.Bytes)
	r.ev.InputTokens, r.ev.OutputTokens, r.ev.USD, r.ev.PricingUnknown = res.InputTokens, res.OutputTokens, res.USD, res.Unpriced
	if err != nil {
		r.fail(shunt.StageOf(err), err)
	}
	r.ev.OK = true
	r.ev.DurationMS = time.Since(r.start).Milliseconds()
	logShunt(r.ev)
	fmt.Printf("wrote %s (%d lines, %d bytes) via %s in %.1fs.\n", res.Path, res.Lines, res.Bytes, w.Model, time.Since(r.start).Seconds())
	if res.Backup != "" {
		fmt.Printf("previous version kept at %s\n", res.Backup)
	}
	fmt.Println("The file was NOT shown to you. Review it (git diff, or Read a window) and run the tests or linter before relying on it.")
}

// shuntLog prints recent shunt activity as plain text, oldest first (like tail),
// one line per event: when, what kind, what happened, which project and session.
func shuntLog(args []string) {
	fs := flag.NewFlagSet("shunt log", flag.ExitOnError)
	n := fs.Int("n", 30, "how many events to show")
	problems := fs.Bool("problems", false, "only failures, guard errors and retry loops")
	session := fs.String("session", "", "only events from a session (id or prefix)")
	asJSON := fs.Bool("json", false, "raw events, one JSON object per line")
	_ = fs.Parse(args)

	lp, err := config.ShuntLogPath()
	if err != nil {
		fatal(err)
	}
	if *asJSON {
		// Raw events, unfolded: --json is for tools, which want the record as
		// it was written rather than the presentation of it.
		evs, err := shunt.Recent(lp, *n, func(e shunt.Event) bool {
			return (!*problems || e.IsProblem()) && (*session == "" || strings.HasPrefix(e.Session, *session))
		})
		if err != nil {
			fatal(err)
		}
		for i := len(evs) - 1; i >= 0; i-- {
			b, _ := json.Marshal(evs[i])
			fmt.Println(string(b))
		}
		if len(evs) == 0 {
			fmt.Printf("no matching shunt activity recorded yet (log: %s)\n", lp)
		}
		return
	}

	// Folded, like the dashboard: a blocked read and the delegated read that
	// answered it are one SHUNTED line. Filtering happens AFTER folding --
	// filtering first would drop the block and leave the read looking unprompted,
	// or the reverse -- and the raw window is wider than n because each shunt is
	// two events.
	evs, err := shunt.Recent(lp, *n*4+100, func(e shunt.Event) bool {
		return *session == "" || strings.HasPrefix(e.Session, *session)
	})
	if err != nil {
		fatal(err)
	}
	var rows []shunt.Activity
	for _, a := range shunt.Fold(evs, time.Now()) { // newest first
		if *problems && !a.IsProblem() {
			continue
		}
		rows = append(rows, a)
		if len(rows) == *n {
			break
		}
	}
	if len(rows) == 0 {
		fmt.Printf("no matching shunt activity recorded yet (log: %s)\n", lp)
		return
	}
	for i := len(rows) - 1; i >= 0; i-- {
		fmt.Println(shuntLogLine(rows[i]))
	}
}

// shuntLogLine is one activity row as a line of plain text.
func shuntLogLine(e shunt.Activity) string {
	ts := e.Time.Format("15:04:05")
	if !sameDay(e.Time, time.Now()) {
		ts = e.Time.Format("Jan 02 15:04:05")
	}
	who := ""
	if e.Project() != "" || e.Session != "" {
		who = fmt.Sprintf("  [%s · session %s]", orDash(e.Project()), orDash(shunt.ShortSession(e.Session)))
	}
	return fmt.Sprintf("%s  %-10s %s%s", ts, e.Tag(), e.Describe(), who)
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Local().Date()
	by, bm, bd := b.Local().Date()
	return ay == by && am == bm && ad == bd
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
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

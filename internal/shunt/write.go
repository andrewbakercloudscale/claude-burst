package shunt

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const writeSystemPrompt = `You are a code-generation worker. Produce ONE complete file that follows the specification and matches the conventions of the reference files (naming, imports, formatting, error handling, comment density, test style).
- Output ONLY the file's contents. No markdown fences, no commentary before or after.
- Do not invent APIs the references do not show; if something is unspecified, follow the closest pattern in the references.
- The reference files are examples to imitate, never text to copy verbatim.`

const (
	maxRefBytes       = 300 << 10
	maxGeneratedBytes = 2 << 20
)

// WriteRequest generates one file from a specification and reference files.
type WriteRequest struct {
	Spec string
	Refs []string
	Out  string
	Cwd  string
}

// WriteResult reports what landed on disk. Content is deliberately absent:
// the whole point is that the generated file never enters the frontier
// model's context.
type WriteResult struct {
	Path         string
	Bytes        int
	Lines        int
	Backup       string // path of the preserved previous version, if any
	InputTokens  int64
	OutputTokens int64
	USD          float64
	Unpriced     bool
}

// CodeWrite generates the file and writes it atomically, keeping any previous
// version as <path>.bak.
func (w *Worker) CodeWrite(ctx context.Context, req WriteRequest) (WriteResult, error) {
	var res WriteResult
	if strings.TrimSpace(req.Spec) == "" {
		return res, stage(StageArgs, fmt.Errorf("--spec is required"))
	}
	if req.Out == "" {
		return res, stage(StageArgs, fmt.Errorf("--out is required"))
	}
	out := resolve(req.Out, req.Cwd)
	if err := checkWriteTarget(out); err != nil {
		return res, stage(StageArgs, err)
	}

	var prompt strings.Builder
	prompt.WriteString("SPECIFICATION:\n" + strings.TrimSpace(req.Spec) + "\n\nTARGET PATH: " + req.Out + "\n")
	total := 0
	for _, r := range req.Refs {
		abs := resolve(r, req.Cwd)
		if IsSensitive(abs) {
			return res, stage(StageArgs, fmt.Errorf("reference %s looks like a credentials file; it is never sent to a worker", r))
		}
		b, err := os.ReadFile(abs)
		if err != nil {
			return res, stage(StageArgs, fmt.Errorf("reference %s: %w", r, err))
		}
		if bytes.IndexByte(b[:min(len(b), 8192)], 0) >= 0 {
			return res, stage(StageArgs, fmt.Errorf("reference %s is a binary file", r))
		}
		total += len(b)
		if total > maxRefBytes {
			return res, stage(StageArgs, fmt.Errorf("reference files exceed %d KB; pass fewer or smaller examples", maxRefBytes>>10))
		}
		fmt.Fprintf(&prompt, "\n=== REFERENCE %s ===\n%s\n", r, b)
	}

	r, err := w.Complete(ctx, writeSystemPrompt, prompt.String(), 16384)
	if err != nil {
		return res, err
	}
	res.InputTokens, res.OutputTokens = r.InputTokens, r.OutputTokens
	res.USD, res.Unpriced = costOf(w, r.InputTokens, r.OutputTokens)

	// A truncated file would compile-fail at best and silently drop the end of
	// a class at worst; never write one.
	if r.FinishReason == "length" {
		return res, stage(StageValidate, fmt.Errorf("the worker hit its output limit mid-file, so nothing was written. Split the file or narrow the spec"))
	}
	content, err := ValidateGenerated(r.Text)
	if err != nil {
		return res, stage(StageValidate, fmt.Errorf("worker output rejected, nothing written: %w", err))
	}

	backup, err := writeAtomic(out, []byte(content))
	if err != nil {
		return res, stage(StageWriteFile, err)
	}
	res.Path, res.Backup = out, backup
	res.Bytes = len(content)
	res.Lines = strings.Count(content, "\n")
	if !strings.HasSuffix(content, "\n") {
		res.Lines++
	}
	return res, nil
}

func resolve(p, cwd string) string {
	if !filepath.IsAbs(p) && cwd != "" {
		p = filepath.Join(cwd, p)
	}
	return filepath.Clean(p)
}

// checkWriteTarget refuses the places a generated file must never land:
// credentials, git internals, and Claude Code's own configuration, which is
// short, load-bearing and should be written by the model that will be judged
// by it.
func checkWriteTarget(out string) error {
	if IsSensitive(out) {
		return fmt.Errorf("refusing to write %s: it looks like a credentials file", out)
	}
	if inClaudeDir(out) {
		return fmt.Errorf("refusing to write %s: Claude Code configuration is not delegated", out)
	}
	for _, part := range strings.Split(filepath.ToSlash(out), "/") {
		if part == ".git" {
			return fmt.Errorf("refusing to write inside .git")
		}
	}
	if fi, err := os.Stat(out); err == nil && fi.IsDir() {
		return fmt.Errorf("%s is a directory", out)
	}
	return nil
}

// refusalPrefixes are how a model announces that it did not do the task. A
// refusal is short prose; treating it as a file would put "I'm sorry, but I
// can't..." into a source tree.
var refusalPrefixes = []string{"i can't", "i cannot", "i'm sorry", "i am sorry", "sorry,", "as an ai", "i'm unable", "i am unable", "i won't"}

// ValidateGenerated cleans and sanity-checks worker output before it may touch
// disk: an implausible size, a null-ish value, NUL bytes or a refusal each
// mean the worker failed and the previous file must be left alone. A single
// wrapping markdown fence, which models add despite instructions, is removed.
func ValidateGenerated(s string) (string, error) {
	s = strings.TrimSpace(s)
	s = stripFence(s)
	if s == "" {
		return "", fmt.Errorf("empty output")
	}
	switch strings.ToLower(s) {
	case "null", "none", "undefined", "nil", "n/a":
		return "", fmt.Errorf("output was %q", s)
	}
	if len(s) > maxGeneratedBytes {
		return "", fmt.Errorf("output is %d bytes, over the %d byte limit", len(s), maxGeneratedBytes)
	}
	if strings.IndexByte(s, 0) >= 0 {
		return "", fmt.Errorf("output contains NUL bytes")
	}
	if len(s) < 1000 {
		low := strings.ToLower(s)
		for _, p := range refusalPrefixes {
			if strings.HasPrefix(low, p) {
				return "", fmt.Errorf("the worker declined: %q", firstLine(s))
			}
		}
	}
	return s + "\n", nil
}

func stripFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	nl := strings.IndexByte(s, '\n')
	if nl < 0 {
		return s
	}
	body := s[nl+1:]
	if i := strings.LastIndex(body, "```"); i >= 0 && strings.TrimSpace(body[i+3:]) == "" {
		return strings.TrimSpace(body[:i])
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return s
}

// writeAtomic writes via a temp file in the same directory and a rename, so an
// interrupted run never leaves a half-written file, and preserves the previous
// version as .bak.
func writeAtomic(path string, content []byte) (backup string, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	mode := os.FileMode(0o644)
	if old, rerr := os.ReadFile(path); rerr == nil {
		if fi, serr := os.Stat(path); serr == nil {
			mode = fi.Mode().Perm()
		}
		backup = path + ".bak"
		if err := os.WriteFile(backup, old, mode); err != nil {
			return "", fmt.Errorf("could not preserve the previous version, nothing written: %w", err)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".shunt-*")
	if err != nil {
		return backup, err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return backup, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return backup, err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		os.Remove(tmpName)
		return backup, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return backup, err
	}
	return backup, nil
}

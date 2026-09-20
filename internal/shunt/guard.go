package shunt

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// HookInput is the part of Claude Code's PreToolUse payload the guard reads.
type HookInput struct {
	ToolName  string `json:"tool_name"`
	Cwd       string `json:"cwd"`
	ToolInput struct {
		FilePath string   `json:"file_path"`
		Offset   *float64 `json:"offset"`
		Limit    *float64 `json:"limit"`
		Command  string   `json:"command"`
	} `json:"tool_input"`
}

// Decision is the guard's verdict. Deny carries the message Claude Code hands
// back to the model.
type Decision struct {
	Deny   bool
	Reason string
	Path   string // the file that tripped the threshold
	Lines  int    // its line count (at least MinLines when denied)
	Bytes  int64
}

// GuardOptions are the knobs the guard needs, decoupled from config so the
// guard is a pure function of its input and the filesystem.
type GuardOptions struct {
	Read     bool
	MinLines int
	Bin      string // command the denial message tells the model to run; default "claude-burst"
}

// Decide never returns an error: a guard that cannot decide must allow. The
// opposite failure -- refusing every read because the checker broke -- would
// leave Claude unable to look at any file at all, and unlike the build gates
// in the WordPress repos there is a safe direction to fail here, because a
// wrongly allowed read merely costs tokens.
func Decide(in HookInput, opt GuardOptions) Decision {
	if !opt.Read || opt.MinLines <= 0 {
		return Decision{}
	}
	switch in.ToolName {
	case "Read":
		// A windowed read is already cheap, and it is the escape hatch the
		// denial message points at, so it must always pass.
		if in.ToolInput.Offset != nil || in.ToolInput.Limit != nil {
			return Decision{}
		}
		return checkPath(in.ToolInput.FilePath, in.Cwd, opt)
	case "Bash":
		for _, p := range bashReadTargets(in.ToolInput.Command, opt.MinLines) {
			if d := checkPath(p, in.Cwd, opt); d.Deny {
				return d
			}
		}
	}
	return Decision{}
}

func checkPath(path, cwd string, opt GuardOptions) Decision {
	minLines := opt.MinLines
	if path == "" {
		return Decision{}
	}
	if !filepath.IsAbs(path) {
		if cwd == "" {
			return Decision{}
		}
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)

	// Claude Code's own configuration is exempt (the article's rule too):
	// agents, skills and settings are short, load-bearing and read for exact
	// wording, which is the opposite of what a summary is good for.
	if inClaudeDir(path) || IsSensitive(path) {
		return Decision{}
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return Decision{}
	}
	// A file with N lines has at least N-1 newlines, so anything smaller than
	// this cannot reach the threshold and needs no scan at all.
	if fi.Size() < int64(minLines-1) {
		return Decision{}
	}
	lines, text := countLines(path, minLines)
	if !text || lines < minLines {
		return Decision{}
	}
	d := Decision{Deny: true, Path: path, Lines: lines, Bytes: fi.Size()}
	d.Reason = denyMessage(opt.Bin, path, lines, minLines)
	return d
}

func inClaudeDir(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".claude" {
			return true
		}
	}
	return false
}

// countLines counts newline-terminated lines up to limit, stopping as soon as
// the threshold is reached so a 2 GB file costs the same as a 400-line one.
// text is false when the file looks binary (a NUL in the first 8 KiB), which
// the guard leaves alone: images and PDFs go through Claude Code's own reader.
func countLines(path string, limit int) (lines int, text bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64*1024)
	head, _ := r.Peek(8192)
	if bytes.IndexByte(head, 0) >= 0 {
		return 0, false
	}
	buf := make([]byte, 64*1024)
	sawData := false
	last := byte('\n')
	for {
		n, err := r.Read(buf)
		if n > 0 {
			sawData = true
			lines += bytes.Count(buf[:n], []byte{'\n'})
			last = buf[n-1]
			if lines >= limit {
				return lines, true
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, false
		}
	}
	if sawData && last != '\n' {
		lines++ // an unterminated final line still counts
	}
	return lines, true
}

func denyMessage(bin, path string, lines, minLines int) string {
	if bin == "" {
		bin = "claude-burst"
	}
	return fmt.Sprintf(`claude-burst shunt: %s has %d+ lines (threshold %d). Do NOT retry this read -- it will be refused every time.

Get what you need from a cheaper worker instead. You will get a short answer with path:line citations:

  %s shunt read --question "<exactly what you need to know>" %s [more files...]

Need exact lines to edit or quote? Read only that range: Read with offset and limit, or sed -n 'START,ENDp' %s. Windowed reads are never blocked.`,
		path, lines, minLines, shellQuote(bin), shellQuote(path), shellQuote(path))
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r == '/' || r == '.' || r == '_' || r == '-' || r == '+' || r == ':' || r == '@' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// bashReadTargets returns the files a plain cat/head/tail/less/more command
// would dump into context. It is deliberately conservative: anything with a
// pipe, redirect, chain, substitution or glob returns nothing, because a
// misparse here blocks a legitimate command and a missed parse merely costs
// tokens. `cat f | grep x` is left alone for the same reason -- the model only
// sees the filtered output.
func bashReadTargets(command string, minLines int) []string {
	command = strings.TrimSpace(command)
	if command == "" || strings.ContainsAny(command, "|;&<>`$(){}*?[]!\n\\") {
		return nil
	}
	words, ok := shellWords(command)
	if !ok || len(words) < 2 {
		return nil
	}
	prog := filepath.Base(words[0])
	args := words[1:]

	var files []string
	switch prog {
	case "cat", "less", "more":
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				files = append(files, a)
			}
		}
	case "head", "tail":
		count, explicit, follow := headTailCount(args)
		if follow || !explicit || count < minLines {
			// Default is 10 lines and a small -n is a deliberate window; both
			// are already cheap. -f/-F streams a growing file and is not a read.
			return nil
		}
		for i := 0; i < len(args); i++ {
			a := args[i]
			if a == "-n" || a == "-c" || a == "--lines" || a == "--bytes" {
				i++
				continue
			}
			if !strings.HasPrefix(a, "-") {
				files = append(files, a)
			}
		}
	default:
		return nil
	}
	return files
}

// headTailCount extracts the -n N / -nN / -N / --lines=N count. explicit is
// false when none was given.
func headTailCount(args []string) (count int, explicit, follow bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-f" || a == "-F" || a == "--follow":
			follow = true
		case a == "-n" || a == "--lines":
			if i+1 < len(args) {
				count, explicit = atoiSigned(args[i+1]), true
				i++
			}
		case strings.HasPrefix(a, "--lines="):
			count, explicit = atoiSigned(strings.TrimPrefix(a, "--lines=")), true
		case strings.HasPrefix(a, "-n") && len(a) > 2:
			count, explicit = atoiSigned(a[2:]), true
		case len(a) > 1 && a[0] == '-' && a[1] >= '0' && a[1] <= '9':
			count, explicit = atoiSigned(a[1:]), true
		case a == "-c" || a == "--bytes":
			// A byte count is not a line count; treat it as a deliberate window.
			return 0, true, follow
		}
	}
	return count, explicit, follow
}

// atoiSigned reads "+N"/"-N"/"N". tail -n +K means "from line K", which can be
// huge, so it is deliberately treated as unbounded rather than as a window.
func atoiSigned(s string) int {
	if strings.HasPrefix(s, "+") {
		return 1 << 30
	}
	n, err := strconv.Atoi(strings.TrimPrefix(s, "-"))
	if err != nil {
		return 0
	}
	return n
}

// shellWords splits a command on whitespace, honouring simple single and
// double quotes. It reports false on an unterminated quote.
func shellWords(s string) ([]string, bool) {
	var words []string
	var cur strings.Builder
	var quote rune
	inWord := false
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, false
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, true
}

// ParseHookInput decodes the hook payload from stdin.
func ParseHookInput(r io.Reader) (HookInput, error) {
	var in HookInput
	err := json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(&in)
	return in, err
}

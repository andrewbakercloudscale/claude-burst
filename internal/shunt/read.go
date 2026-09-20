package shunt

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const readSystemPrompt = `You are a code-reading worker for a senior engineer. Answer the question using ONLY the file text provided.
- Every line of the files is prefixed with its line number. Cite every claim as path:line (or path:start-end).
- Be terse: short bullets, no preamble, no restating the question. Quote code only when the exact wording matters.
- If the provided text does not contain the answer, say so plainly. Never guess or fill in from general knowledge.
- If NONE of the provided text is relevant to the question, reply with exactly: NOT IN THIS PART`

const notInThisPart = "NOT IN THIS PART"

const maxFileBytes = 32 << 20

// ReadRequest asks a question over one or more files.
type ReadRequest struct {
	Question   string
	Paths      []string
	Cwd        string
	ChunkLines int
}

// ReadResult is the answer plus the accounting for it.
type ReadResult struct {
	Text         string
	Files        int
	Calls        int
	BytesIn      int64
	InputTokens  int64
	OutputTokens int64
	USD          float64
	Unpriced     bool
	Skipped      []string // paths not delegated, with the reason
}

// unit is a contiguous run of one file's lines, already numbered.
type unit struct {
	path       string
	start, end int
	text       string
	lines      int
	bytes      int64
}

// BulkRead answers req.Question from the files, chunking anything larger than
// ChunkLines and running the chunks concurrently.
func (w *Worker) BulkRead(ctx context.Context, req ReadRequest) (ReadResult, error) {
	var res ReadResult
	if strings.TrimSpace(req.Question) == "" {
		return res, fmt.Errorf("--question is required")
	}
	if len(req.Paths) == 0 {
		return res, fmt.Errorf("at least one file path is required")
	}
	chunk := req.ChunkLines
	if chunk <= 0 {
		chunk = 6000
	}

	var units []unit
	for _, p := range req.Paths {
		abs := p
		if !filepath.IsAbs(abs) && req.Cwd != "" {
			abs = filepath.Join(req.Cwd, abs)
		}
		abs = filepath.Clean(abs)
		if IsSensitive(abs) {
			res.Skipped = append(res.Skipped, p+" (looks like a credentials file; never sent to a worker -- read it directly if you must)")
			continue
		}
		us, err := loadUnits(abs, p, chunk)
		if err != nil {
			res.Skipped = append(res.Skipped, p+" ("+err.Error()+")")
			continue
		}
		units = append(units, us...)
		res.Files++
	}
	if len(units) == 0 {
		return res, fmt.Errorf("nothing to read: %s", strings.Join(res.Skipped, "; "))
	}

	groups := pack(units, chunk)
	answers := make([]string, len(groups))
	errs := make([]error, len(groups))
	toks := make([][2]int64, len(groups))

	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, g := range groups {
		wg.Add(1)
		go func(i int, g []unit) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r, err := w.Complete(ctx, readSystemPrompt, renderGroup(req.Question, g), 2048)
			if err != nil {
				errs[i] = err
				return
			}
			answers[i] = strings.TrimSpace(r.Text)
			toks[i] = [2]int64{r.InputTokens, r.OutputTokens}
			if r.FinishReason == "length" {
				answers[i] += "\n[worker answer was cut off at its length limit -- ask a narrower question]"
			}
		}(i, g)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			return res, fmt.Errorf("%w (call %d of %d). Fall back to reading with offset and limit windows", err, i+1, len(groups))
		}
	}
	for _, u := range units {
		res.BytesIn += u.bytes
	}
	res.Calls = len(groups)
	for _, t := range toks {
		res.InputTokens += t[0]
		res.OutputTokens += t[1]
	}
	res.USD, res.Unpriced = costOf(w, res.InputTokens, res.OutputTokens)
	res.Text = combine(groups, answers)
	return res, nil
}

func costOf(w *Worker, in, out int64) (float64, bool) {
	usd, known := w.Cost(in, out)
	return usd, !known && (in > 0 || out > 0)
}

func loadUnits(abs, shown string, chunk int) ([]unit, error) {
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("cannot stat: %v", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	if fi.Size() > maxFileBytes {
		return nil, fmt.Errorf("larger than %d MB", maxFileBytes>>20)
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	if bytes.IndexByte(b[:min(len(b), 8192)], 0) >= 0 {
		return nil, fmt.Errorf("binary file")
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(b) == 0 {
		lines = nil
	}

	var out []unit
	for start := 0; start < len(lines) || (start == 0 && len(lines) == 0); start += chunk {
		end := min(start+chunk, len(lines))
		var sb strings.Builder
		for i := start; i < end; i++ {
			fmt.Fprintf(&sb, "%6d\t%s\n", i+1, lines[i])
		}
		out = append(out, unit{path: shown, start: start + 1, end: end, text: sb.String(), lines: end - start, bytes: int64(sb.Len())})
		if len(lines) == 0 {
			break
		}
	}
	return out, nil
}

// pack merges consecutive small units into one call so several short files
// cost one round trip rather than one each.
func pack(units []unit, chunk int) [][]unit {
	var groups [][]unit
	var cur []unit
	total := 0
	for _, u := range units {
		if len(cur) > 0 && total+u.lines > chunk {
			groups = append(groups, cur)
			cur, total = nil, 0
		}
		cur = append(cur, u)
		total += u.lines
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups
}

func renderGroup(question string, g []unit) string {
	var sb strings.Builder
	sb.WriteString("QUESTION: " + strings.TrimSpace(question) + "\n\n")
	for _, u := range g {
		fmt.Fprintf(&sb, "=== FILE %s (lines %d-%d) ===\n%s\n", u.path, u.start, u.end, u.text)
	}
	return sb.String()
}

func combine(groups [][]unit, answers []string) string {
	if len(groups) == 1 {
		return strings.TrimSpace(strings.TrimPrefix(answers[0], notInThisPart+"\n"))
	}
	var parts []string
	for i, a := range answers {
		if strings.TrimSpace(a) == notInThisPart {
			continue
		}
		parts = append(parts, fmt.Sprintf("--- %s ---\n%s", describeGroup(groups[i]), a))
	}
	if len(parts) == 0 {
		return "The worker found nothing relevant to the question in any of the files."
	}
	return strings.Join(parts, "\n\n")
}

func describeGroup(g []unit) string {
	var parts []string
	for _, u := range g {
		parts = append(parts, fmt.Sprintf("%s:%d-%d", u.path, u.start, u.end))
	}
	return strings.Join(parts, ", ")
}

// Footer is the one line that says what the delegation did.
func (r ReadResult) Footer(model string, d time.Duration) string {
	kept := EstimateTokens(r.BytesIn)
	s := fmt.Sprintf("[shunt: %d file(s), %d call(s), ~%d tokens of file text kept out of context; worker %s, %.1fs", r.Files, r.Calls, kept, model, d.Seconds())
	if r.Unpriced {
		s += "; cost unknown (no pricing entry)"
	} else {
		s += fmt.Sprintf("; $%.4f", r.USD)
	}
	s += "]"
	for _, sk := range r.Skipped {
		s += "\n[shunt: skipped " + sk + "]"
	}
	return s
}

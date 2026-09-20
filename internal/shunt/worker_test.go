package shunt

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// fake serves chat completions, recording each user prompt.
type fake struct {
	mu      sync.Mutex
	prompts []string
	reply   func(user string) (string, string)
	status  int
	auth    string
}

func (f *fake) server(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string              `json:"model"`
			Messages []map[string]string `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		user := body.Messages[1]["content"]
		f.mu.Lock()
		f.prompts = append(f.prompts, user)
		f.auth = r.Header.Get("Authorization")
		f.mu.Unlock()
		if f.status != 0 {
			http.Error(w, "boom", f.status)
			return
		}
		text, finish := f.reply(user)
		if finish == "" {
			finish = "stop"
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": text}, "finish_reason": finish}},
			"usage":   map[string]int{"prompt_tokens": 1000, "completion_tokens": 50},
		})
	}))
	t.Cleanup(s.Close)
	return s
}

func numbered(n int) string {
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&sb, "func f%d() {}\n", i)
	}
	return sb.String()
}

func TestBulkReadCitesAndCounts(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "svc.go")
	os.WriteFile(p, []byte(numbered(50)), 0o644)
	f := &fake{reply: func(string) (string, string) { return "retries are in svc.go:12", "" }}
	w := NewTestWorker(f.server(t).URL, "glm")
	w.Pricing = map[string]config.ModelPrice{"glm": {InputPerMTok: 1, OutputPerMTok: 4}}

	res, err := w.BulkRead(context.Background(), ReadRequest{Question: "where are retries?", Paths: []string{p}, ChunkLines: 6000})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "retries are in svc.go:12" || res.Calls != 1 || res.Files != 1 {
		t.Errorf("unexpected result %+v", res)
	}
	if f.auth != "Bearer test-key" {
		t.Errorf("worker must authenticate with its own key, got %q", f.auth)
	}
	p0 := f.prompts[0]
	if !strings.Contains(p0, "QUESTION: where are retries?") || !strings.Contains(p0, "    12\tfunc f12() {}") {
		t.Errorf("prompt must carry the question and numbered lines:\n%s", p0[:200])
	}
	want := 1000.0/1e6*1 + 50.0/1e6*4
	if res.USD < want*0.999 || res.USD > want*1.001 || res.Unpriced {
		t.Errorf("cost %v want %v unpriced=%v", res.USD, want, res.Unpriced)
	}
}

func TestBulkReadChunksALargeFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.go")
	os.WriteFile(p, []byte(numbered(2500)), 0o644)
	f := &fake{reply: func(user string) (string, string) {
		if strings.Contains(user, "func f2100()") {
			return "found at big.go:2100", ""
		}
		return notInThisPart, ""
	}}
	w := NewTestWorker(f.server(t).URL, "glm")
	res, err := w.BulkRead(context.Background(), ReadRequest{Question: "q", Paths: []string{p}, ChunkLines: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if res.Calls != 3 {
		t.Fatalf("2500 lines at 1000/chunk is 3 calls, got %d", res.Calls)
	}
	if !strings.Contains(res.Text, "found at big.go:2100") || strings.Contains(res.Text, notInThisPart) {
		t.Errorf("irrelevant parts must be dropped and the hit kept:\n%s", res.Text)
	}
	// the third chunk must be numbered from 2001, or citations are wrong
	joined := strings.Join(f.prompts, "\n")
	if !strings.Contains(joined, "  2001\tfunc f2001() {}") || !strings.Contains(joined, "lines 2001-2500") {
		t.Errorf("chunks must keep absolute line numbers")
	}
}

func TestBulkReadNothingRelevantAnywhere(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.go")
	os.WriteFile(p, []byte(numbered(30)), 0o644)
	f := &fake{reply: func(string) (string, string) { return notInThisPart, "" }}
	w := NewTestWorker(f.server(t).URL, "m")
	res, err := w.BulkRead(context.Background(), ReadRequest{Question: "q", Paths: []string{p}, ChunkLines: 10})
	if err != nil || !strings.Contains(res.Text, "nothing relevant") {
		t.Errorf("want an explicit nothing-found, got %q err=%v", res.Text, err)
	}
}

func TestBulkReadPacksSmallFilesIntoOneCall(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for i := 0; i < 4; i++ {
		p := filepath.Join(dir, fmt.Sprintf("f%d.go", i))
		os.WriteFile(p, []byte(numbered(100)), 0o644)
		paths = append(paths, p)
	}
	f := &fake{reply: func(string) (string, string) { return "ok", "" }}
	w := NewTestWorker(f.server(t).URL, "m")
	res, err := w.BulkRead(context.Background(), ReadRequest{Question: "q", Paths: paths, ChunkLines: 6000})
	if err != nil || res.Calls != 1 || res.Files != 4 {
		t.Errorf("4 small files should be one call: %+v err=%v", res, err)
	}
}

func TestBulkReadNeverSendsSecrets(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	os.WriteFile(env, []byte("API_KEY=sk-secret\n"), 0o600)
	ok := filepath.Join(dir, "a.go")
	os.WriteFile(ok, []byte(numbered(10)), 0o644)
	f := &fake{reply: func(string) (string, string) { return "fine", "" }}
	w := NewTestWorker(f.server(t).URL, "m")
	res, err := w.BulkRead(context.Background(), ReadRequest{Question: "q", Paths: []string{env, ok}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(f.prompts, ""), "sk-secret") {
		t.Fatalf("a credentials file reached the worker")
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], ".env") {
		t.Errorf("the skip must be reported: %v", res.Skipped)
	}
	if _, err := w.BulkRead(context.Background(), ReadRequest{Question: "q", Paths: []string{env}}); err == nil {
		t.Errorf("only-secrets must be an error, not an empty success")
	}
}

func TestBulkReadFailureTellsTheModelHowToProceed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.go")
	os.WriteFile(p, []byte(numbered(10)), 0o644)
	f := &fake{status: 500}
	w := NewTestWorker(f.server(t).URL, "m")
	_, err := w.BulkRead(context.Background(), ReadRequest{Question: "q", Paths: []string{p}})
	if err == nil || !strings.Contains(err.Error(), "offset and limit") || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("failure must name the status and the fallback: %v", err)
	}
}

func TestUnknownPricingIsFlaggedNotFree(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.go")
	os.WriteFile(p, []byte(numbered(10)), 0o644)
	f := &fake{reply: func(string) (string, string) { return "ok", "" }}
	w := NewTestWorker(f.server(t).URL, "unpriced-model")
	res, _ := w.BulkRead(context.Background(), ReadRequest{Question: "q", Paths: []string{p}})
	if !res.Unpriced || res.USD != 0 || !strings.Contains(res.Footer("m", time.Second), "cost unknown") {
		t.Errorf("an unpriced model must read as unknown, not $0: %+v", res)
	}
}

func TestCodeWriteWritesBacksUpAndNeverShowsContent(t *testing.T) {
	dir := t.TempDir()
	ref := filepath.Join(dir, "OrderTest.java")
	os.WriteFile(ref, []byte("class OrderTest {}\n"), 0o644)
	out := filepath.Join(dir, "sub", "UserTest.java")
	f := &fake{reply: func(string) (string, string) { return "```java\nclass UserTest {}\n```", "" }}
	w := NewTestWorker(f.server(t).URL, "m")

	res, err := w.CodeWrite(context.Background(), WriteRequest{Spec: "test the user", Refs: []string{ref}, Out: out})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if string(got) != "class UserTest {}\n" {
		t.Errorf("fence must be stripped, got %q", got)
	}
	if res.Backup != "" {
		t.Errorf("no previous file, no backup")
	}
	if !strings.Contains(f.prompts[0], "=== REFERENCE "+ref) || !strings.Contains(f.prompts[0], "class OrderTest") {
		t.Errorf("reference must reach the worker")
	}

	// overwrite: previous version preserved
	f.reply = func(string) (string, string) { return "class UserTest2 {}", "" }
	res, err = w.CodeWrite(context.Background(), WriteRequest{Spec: "again", Out: out})
	if err != nil || res.Backup != out+".bak" {
		t.Fatalf("want a .bak, got %+v err=%v", res, err)
	}
	if b, _ := os.ReadFile(out + ".bak"); string(b) != "class UserTest {}\n" {
		t.Errorf("backup must hold the previous version, got %q", b)
	}
}

func TestCodeWriteLeavesTheFileAloneWhenTheWorkerFails(t *testing.T) {
	out := filepath.Join(t.TempDir(), "keep.go")
	os.WriteFile(out, []byte("package keep\n"), 0o644)
	cases := map[string]func(string) (string, string){
		"truncated": func(string) (string, string) { return "package x\nfunc a() {", "length" },
		"refusal":   func(string) (string, string) { return "I'm sorry, but I can't help with that.", "" },
		"empty":     func(string) (string, string) { return "   ", "" },
		"null":      func(string) (string, string) { return "null", "" },
	}
	for name, reply := range cases {
		f := &fake{reply: reply}
		w := NewTestWorker(f.server(t).URL, "m")
		if _, err := w.CodeWrite(context.Background(), WriteRequest{Spec: "s", Out: out}); err == nil {
			t.Errorf("%s: must be an error", name)
		}
		if b, _ := os.ReadFile(out); string(b) != "package keep\n" {
			t.Errorf("%s: existing file was disturbed: %q", name, b)
		}
		if _, err := os.Stat(out + ".bak"); err == nil {
			t.Errorf("%s: a failed run must not leave a .bak", name)
		}
	}
}

func TestCodeWriteRefusesDangerousTargets(t *testing.T) {
	dir := t.TempDir()
	f := &fake{reply: func(string) (string, string) { return "x", "" }}
	w := NewTestWorker(f.server(t).URL, "m")
	for _, out := range []string{
		filepath.Join(dir, ".env"),
		filepath.Join(dir, ".git", "hooks", "pre-push"),
		filepath.Join(dir, ".claude", "settings.json"),
		dir,
	} {
		if _, err := w.CodeWrite(context.Background(), WriteRequest{Spec: "s", Out: out}); err == nil {
			t.Errorf("must refuse %s", out)
		}
	}
	if len(f.prompts) != 0 {
		t.Errorf("a refused target must not spend a worker call")
	}
	ref := filepath.Join(dir, "id_rsa")
	os.WriteFile(ref, []byte("PRIVATE"), 0o600)
	if _, err := w.CodeWrite(context.Background(), WriteRequest{Spec: "s", Refs: []string{ref}, Out: filepath.Join(dir, "a.go")}); err == nil {
		t.Errorf("a credentials file must never be a reference")
	}
}

func TestDoctorDetectsTruncation(t *testing.T) {
	// echoes the marker only when it can see the start of the prompt
	f := &fake{reply: func(user string) (string, string) {
		return strings.SplitN(strings.TrimPrefix(user, "MARKER: "), "\n", 2)[0], ""
	}}
	w := NewTestWorker(f.server(t).URL, "m")
	if _, ok, _, err := w.Doctor(context.Background(), 50); err != nil || !ok {
		t.Errorf("healthy worker should pass: ok=%v err=%v", ok, err)
	}
	f.reply = func(string) (string, string) { return "I cannot see any marker", "" }
	if _, ok, _, _ := w.Doctor(context.Background(), 50); ok {
		t.Errorf("a worker that lost the start of the prompt must fail the doctor")
	}
}

func TestSummaryAndUnpricedHonesty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shunt.jsonl")
	Append(path, Event{Kind: KindRead, OK: true, BytesIn: 400_000, BytesOut: 2_000, InputTokens: 100_000, OutputTokens: 500, USD: 0.02})
	Append(path, Event{Kind: KindWrite, OK: true, BytesOut: 8_000, PricingUnknown: true})
	Append(path, Event{Kind: KindRead, OK: false})
	Append(path, Event{Kind: KindDeny, OK: true, BytesIn: 50_000})
	s, err := SummarizeLog(path, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Reads != 2 || s.Writes != 1 || s.Denials != 1 || s.Failures != 1 {
		t.Errorf("counts wrong: %+v", s)
	}
	if want := EstimateTokens(398_000) + EstimateTokens(8_000); s.KeptOutTokens() != want {
		t.Errorf("kept-out %d want %d", s.KeptOutTokens(), want)
	}
	if !strings.Contains(s.String(), "INCOMPLETE") {
		t.Errorf("an unpriced call must mark the cost incomplete: %s", s)
	}
}

func TestSkillDescribesOnlyWhatIsOn(t *testing.T) {
	readOnly := SkillContent(config.ShuntConfig{Read: true}, "/bin/claude-burst")
	if !strings.Contains(readOnly, "shunt read") || strings.Contains(readOnly, "shunt write") {
		t.Errorf("read-only skill must not advertise write")
	}
	writeOnly := SkillContent(config.ShuntConfig{Write: true}, "/bin/claude-burst")
	if !strings.Contains(writeOnly, "shunt write") || strings.Contains(writeOnly, "shunt read --question") {
		t.Errorf("write-only skill must not advertise read")
	}
	if !strings.Contains(readOnly, "Do NOT delegate") || !strings.HasPrefix(readOnly, "---\nname: "+SkillName) {
		t.Errorf("skill needs frontmatter and its non-candidates")
	}
}

func TestRecentIsNewestFirstAndFilters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shunt.jsonl")
	for i, k := range []string{KindRead, KindDeny, KindWrite, KindDeny, KindRead} {
		Append(path, Event{Time: time.Unix(int64(1000+i), 0), Kind: k, OK: true, Files: i})
	}
	all, _ := Recent(path, 3, nil)
	if len(all) != 3 || all[0].Files != 4 || all[2].Files != 2 {
		t.Errorf("want the last three, newest first: %+v", all)
	}
	calls, _ := Recent(path, 10, func(e Event) bool { return e.Kind == KindRead || e.Kind == KindWrite })
	if len(calls) != 3 || calls[0].Files != 4 {
		t.Errorf("filter: %+v", calls)
	}
	if none, err := Recent(filepath.Join(t.TempDir(), "absent"), 5, nil); err != nil || len(none) != 0 {
		t.Errorf("a missing log is empty, not an error: %v %v", none, err)
	}
}

func TestEventKeptOutTokens(t *testing.T) {
	cases := []struct {
		e    Event
		want int64
	}{
		{Event{Kind: KindRead, OK: true, BytesIn: 4000, BytesOut: 400}, 900},
		{Event{Kind: KindRead, OK: true, BytesIn: 100, BytesOut: 5000}, 0}, // an answer longer than the file kept nothing out
		{Event{Kind: KindWrite, OK: true, BytesOut: 8000}, 2000},
		{Event{Kind: KindRead, OK: false, BytesIn: 4000}, 0},
		{Event{Kind: KindDeny, OK: true, BytesIn: 90000}, 0},
	}
	for _, c := range cases {
		if got := c.e.KeptOutTokens(); got != c.want {
			t.Errorf("%+v: got %d want %d", c.e, got, c.want)
		}
	}
}

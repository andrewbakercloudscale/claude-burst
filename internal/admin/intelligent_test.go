package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/autocompact"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/repo"
)

// seedLearned gives the server a repository the learner has a threshold
// for, already stepped today so a save does not move it.
func seedLearned(t *testing.T, s *Server) (root string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "src", "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The real lookup takes every temp folder for scratch space, which is
	// where a test's repository has to live.
	s.repos = &repoResolver{repo.NewWith(t.TempDir(), nil)}
	s.learnMu.Lock()
	s.learned = autocompact.State{Repos: map[string]*autocompact.Repo{root: {
		Root: root, Name: "my-project", Threshold: 160_000, Target: 150_000, Previous: 175_000, Compactions: 5,
		Failures:  autocompact.Failures{Unpaid: 1, Attempts: 5, Rate: 0.2, LostUSD: 0.31},
		Reason:    "a compaction leaves 70k",
		SteppedOn: time.Now().Local().Format("2006-01-02"),
	}}}
	s.learnMu.Unlock()
	return root
}

func threshold(t *testing.T, s *Server, query string) (thresholdAnswer, int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7788/api/GetAutoCompactionThreshold?"+query, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	var a thresholdAnswer
	_ = json.Unmarshal(w.Body.Bytes(), &a)
	return a, w.Code
}

func TestGetAutoCompactionThreshold(t *testing.T) {
	s := newTestServer(t)
	writeConfig(t, os.Getenv("HOME"))
	root := seedLearned(t, s)

	if _, code := threshold(t, s, ""); code != http.StatusBadRequest {
		t.Fatalf("no folder: status %d, want 400", code)
	}
	// Compaction off: nothing compacts, whatever was learned.
	a, code := threshold(t, s, "folder=my-project")
	if code != http.StatusOK || a.Threshold != 0 || a.Source != "off" || a.Enabled {
		t.Fatalf("compaction off: %d %+v", code, a)
	}
	// Fixed mode: the one Compact at, with what the mode would use beside it.
	if rr := mutate(t, s, "/api/compaction", `{"enabled":true,"compact_at_tokens":300000}`); rr.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rr.Code, rr.Body.String())
	}
	a, _ = threshold(t, s, "folder=my-project")
	if a.Threshold != 300_000 || a.Source != "fixed" || a.Intelligent || a.Target != 150_000 || a.Root != root {
		t.Fatalf("fixed mode: %+v", a)
	}
	// Intelligent mode: the learned one, by name, by path anywhere inside
	// the repository, and whatever the case of the name.
	rr := mutate(t, s, "/api/compaction", `{"enabled":true,"compact_at_tokens":300000,"mode":"intelligent","floor_tokens":120000,"window_minutes":45}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Intelligent Compaction Mode on: 1 repositories") {
		t.Fatalf("save: %d %s", rr.Code, rr.Body.String())
	}
	for _, q := range []string{"folder=my-project", "folder=MY-PROJECT", "folder=" + url.QueryEscape(filepath.Join(root, "src", "deep"))} {
		a, code = threshold(t, s, q)
		if code != http.StatusOK || a.Threshold != 160_000 || a.Source != "learned" || !a.Intelligent || a.Repo != "my-project" {
			t.Fatalf("%s: %d %+v", q, code, a)
		}
	}
	if a.Fixed != 300_000 || a.Floor != 120_000 || a.DelayMinutes != 45 || a.Previous != 175_000 || a.Failures.Unpaid != 1 || a.Failures.LostUSD != 0.31 {
		t.Fatalf("the rest of the answer: %+v", a)
	}
	// The running gateway has it too.
	if cfg, err := config.Load(); err != nil || !cfg.PrimaryCompaction.Intelligent() || cfg.PrimaryCompaction.FloorTokens != 120_000 {
		t.Fatalf("config.json: %+v %v", cfg.PrimaryCompaction, err)
	}
	// A folder nothing is known about is on the fixed Compact at.
	a, _ = threshold(t, s, "folder=never-seen")
	if a.Threshold != 300_000 || a.Source != "fixed" || !strings.Contains(a.Reason, "nothing learned") {
		t.Fatalf("unknown folder: %+v", a)
	}
	// The user's own override for the repository wins over the learned one.
	if rr := mutate(t, s, "/api/compaction/repo", `{"repo":"`+root+`","compact_at_tokens":500000}`); rr.Code != http.StatusOK {
		t.Fatalf("override: %d %s", rr.Code, rr.Body.String())
	}
	a, _ = threshold(t, s, "folder=my-project")
	if a.Threshold != 500_000 || a.Source != "override" {
		t.Fatalf("override: %+v", a)
	}
}

func TestIntelligentCompactionSettingsAreValidatedAndListed(t *testing.T) {
	s := newTestServer(t)
	writeConfig(t, os.Getenv("HOME"))
	seedLearned(t, s)
	for _, body := range []string{
		`{"enabled":true,"mode":"clever"}`,
		`{"enabled":true,"mode":"intelligent","floor_tokens":10000}`,                             // under the least Compact at
		`{"enabled":true,"mode":"intelligent","compact_at_tokens":200000,"floor_tokens":250000}`, // over Compact at
	} {
		if rr := mutate(t, s, "/api/compaction", body); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400", body, rr.Code)
		}
	}
	if rr := mutate(t, s, "/api/compaction", `{"enabled":true,"mode":"intelligent"}`); rr.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rr.Code, rr.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7788/api/intelligent-compaction", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	var v learnedView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %v %s", w.Code, err, w.Body.String())
	}
	if v.Mode != "intelligent" || !v.Enabled || v.Fixed != 300_000 || v.Floor != 100_000 || v.Delay != 30 || v.Days != 14 {
		t.Fatalf("view %+v", v)
	}
	if len(v.Repos) != 1 || v.Repos[0].InForce != 160_000 || v.Repos[0].Source != "learned" || v.Repos[0].Name != "my-project" {
		t.Fatalf("rows %+v", v.Repos)
	}
	// What was learned is kept in the test's own folder, never the real one.
	if p := s.learnedPath(); !strings.HasPrefix(p, os.TempDir()) && !strings.Contains(p, "/T/") {
		t.Fatalf("learner's file at %q", p)
	}
	if st := autocompact.Load(s.learnedPath()); st.Repos == nil || len(st.Repos) != 1 {
		t.Fatalf("saved state %+v", st)
	}
}

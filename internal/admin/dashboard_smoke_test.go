package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// The dashboard's JavaScript, run in a real browser against this package's
// real handlers. check-admin-js.sh only proves the script parses; a field
// renamed on one side, or a render that throws on an empty list, parses
// fine and leaves a blank section. This loads the page, lets every first
// fetch and render run, and fails on any uncaught page error or 5xx.
//
// Needs node and Playwright with Chromium (CI installs both; see
// .github/workflows/test.yml). Locally it runs when node already resolves
// playwright (a global install) or CLAUDE_BURST_PLAYWRIGHT is a directory
// holding node_modules/playwright, and skips otherwise.
func TestDashboardRunsInABrowser(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	env := os.Environ()
	if dir := os.Getenv("CLAUDE_BURST_PLAYWRIGHT"); dir != "" {
		env = append(env, "NODE_PATH="+filepath.Join(dir, "node_modules"))
	}
	probe := exec.Command(node, "-e", `require.resolve("playwright")`)
	probe.Env = env
	if probe.Run() != nil {
		t.Skip("playwright not resolvable from node; set CLAUDE_BURST_PLAYWRIGHT to a directory with node_modules/playwright")
	}

	s := newTestServer(t)
	stubMacForSettings(t)
	stubPermissions(t, "Identifier="+signingIdentifier+"\nAuthority="+signingName+"\n", true, nil)
	repo := filepath.Join(t.TempDir(), "smoke-repo")
	cfg := config.Default()
	cfg.PrimaryCompaction.RepoOverrides = []config.RepoCompaction{{Repo: repo, CompactAtTokens: 250_000}}
	dir := filepath.Join(os.Getenv("HOME"), ".config", "claude-burst")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	// Loading the page must only read. A POST here would be the page
	// changing settings on its own.
	var mu sync.Mutex
	var mutations []string
	h := s.Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			mu.Lock()
			mutations = append(mutations, r.Method+" "+r.URL.Path)
			mu.Unlock()
			http.Error(w, "the smoke test allows no mutations", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	cmd := exec.Command(node, filepath.Join("..", "..", "scripts", "ci", "admin-smoke.js"), srv.URL+"/", "smoke-repo")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("dashboard smoke failed: %v", err)
	}
	if !strings.Contains(string(out), "250k") {
		t.Fatal("the repository override did not render with its limit")
	}
	if !strings.Contains(string(out), "signed by "+signingName) || !strings.Contains(string(out), "Downloads") {
		t.Fatal("the Permissions section did not render signing and folders")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(mutations) > 0 {
		t.Fatalf("loading the page sent mutations: %v", mutations)
	}
}

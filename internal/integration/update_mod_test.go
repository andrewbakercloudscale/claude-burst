package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A fake claude: a version, a plugin list read from $FAKE/installed (the
// install path, or nothing), a marketplace remembered by path in
// $FAKE/marketplace, and install copying the mod from that marketplace, as
// the real one copies a local marketplace's plugin into its cache. Every call is
// logged to $FAKE/calls.
const fakeClaude = `#!/bin/zsh
echo "$*" >> "$FAKE/calls"
case "$1 $2" in
  "--version ") echo "${FAKE_VERSION:-2.1.288} (Claude Code)" ;;
  "plugin list")
    if [[ -s "$FAKE/installed" ]]; then
      printf '[{"id":"burst-band@burst","installPath":"%s"}]\n' "$(cat "$FAKE/installed")"
    else
      echo '[]'
    fi ;;
  "plugin install")
    rm -rf "$FAKE/cache"; mkdir -p "$FAKE/cache"
    m="$(cat "$FAKE/marketplace")/mods/burst-band"
    cp -R "$m/.claude-plugin" "$m/hooks" "$FAKE/cache/"
    echo "$FAKE/cache" > "$FAKE/installed" ;;
  "plugin uninstall") : > "$FAKE/installed" ;;
  "plugin marketplace")
    if [[ "$3" == list ]]; then
      if [[ -s "$FAKE/marketplace" ]]; then
        printf '[{"name":"burst","source":"directory","path":"%s"}]\n' "$(cat "$FAKE/marketplace")"
      else
        echo '[]'
      fi
    fi
    [[ "$3" == add ]] && echo "$4" > "$FAKE/marketplace"
    [[ "$3" == remove ]] && : > "$FAKE/marketplace" ;;
esac
exit 0
`

type modRig struct {
	t         *testing.T
	fake, src string
	env       []string
}

func newModRig(t *testing.T) *modRig {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("macOS scripts")
	}
	r := &modRig{t: t, fake: t.TempDir()}
	// A copy of the mod, so the test can change it without touching the repo.
	root := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, "scripts"), 0o755))
	script, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "update-mod.sh"))
	must(t, err)
	must(t, os.WriteFile(filepath.Join(root, "scripts", "update-mod.sh"), script, 0o755))
	must(t, os.MkdirAll(filepath.Join(root, ".claude-plugin"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, ".claude-plugin", "marketplace.json"), []byte(`{"name":"burst","plugins":[{"name":"burst-band","source":"./mods/burst-band"}]}`), 0o644))
	r.src = filepath.Join(root, "mods", "burst-band")
	must(t, os.MkdirAll(filepath.Join(r.src, ".claude-plugin"), 0o755))
	must(t, os.MkdirAll(filepath.Join(r.src, "hooks"), 0o755))
	must(t, os.WriteFile(filepath.Join(r.src, ".claude-plugin", "plugin.json"), []byte(`{"name":"burst-band","version":"0.1.0"}`), 0o644))
	must(t, os.WriteFile(filepath.Join(r.src, "hooks", "register.js"), []byte("export function register(on) {}\n"), 0o644))
	claude := filepath.Join(r.fake, "claude")
	must(t, os.WriteFile(claude, []byte(fakeClaude), 0o755))
	r.env = []string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin", "FAKE=" + r.fake, "MOD_SRC=" + r.src, "CLAUDE_BIN=" + claude}
	return r
}

func (r *modRig) run(extraEnv []string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("zsh", append([]string{filepath.Join(filepath.Dir(filepath.Dir(r.src)), "scripts", "update-mod.sh")}, args...)...)
	cmd.Env = append(append([]string{}, r.env...), extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("update-mod.sh %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func (r *modRig) calls() string {
	b, _ := os.ReadFile(filepath.Join(r.fake, "calls"))
	return string(b)
}

func TestUpdateModInstallsUpdatesAndRemoves(t *testing.T) {
	r := newModRig(t)

	if out := r.run(nil); !strings.Contains(out, "burst-band mod: installed") {
		t.Fatalf("first run: %s\ncalls:\n%s", out, r.calls())
	}
	if !strings.Contains(r.calls(), "plugin marketplace add") {
		t.Errorf("the marketplace must be added first:\n%s", r.calls())
	}

	if out := r.run(nil); !strings.Contains(out, "up to date") {
		t.Fatalf("unchanged: %s", out)
	}

	// An edit with the same version still reaches the installed copy.
	must(t, os.WriteFile(filepath.Join(r.src, "hooks", "register.js"), []byte("export function register(on) { /* new */ }\n"), 0o644))
	if out := r.run(nil); !strings.Contains(out, "burst-band mod: updated") {
		t.Fatalf("edited: %s\ncalls:\n%s", out, r.calls())
	}
	b, err := os.ReadFile(filepath.Join(r.fake, "cache", "hooks", "register.js"))
	must(t, err)
	if !strings.Contains(string(b), "new") {
		t.Fatalf("installed copy not updated: %s", b)
	}

	_ = os.Remove(filepath.Join(r.fake, "calls"))
	r.run(nil, "uninstall")
	if c := r.calls(); !strings.Contains(c, "plugin uninstall burst-band@burst") || !strings.Contains(c, "plugin marketplace remove burst") {
		t.Fatalf("uninstall calls:\n%s", c)
	}
}

func TestUpdateModSkipsWhereModsCannotLoad(t *testing.T) {
	r := newModRig(t)
	if out := r.run([]string{"FAKE_VERSION=2.1.200"}); !strings.Contains(out, "older than 2.1.287") {
		t.Fatalf("old Claude Code: %s", out)
	}
	if out := r.run([]string{"CLAUDE_BURST_MOD=no"}); !strings.Contains(out, "skipped") {
		t.Fatalf("opted out: %s", out)
	}
	if strings.Contains(r.calls(), "plugin install") {
		t.Fatalf("nothing may be installed:\n%s", r.calls())
	}
	r.env = append(r.env, "CLAUDE_BIN=/nonexistent/claude")
	if out := r.run(nil); !strings.Contains(out, "not found") {
		t.Fatalf("no Claude Code: %s", out)
	}
}

// Seen on a Mac running Burst 0.19.0 with mod 0.3.0: the marketplace was the
// checkout that first installed the mod, and an update run from anywhere
// else reinstalled that checkout's old copy.
func TestUpdateModReplacesAMarketplaceLeftAtAnOldCheckout(t *testing.T) {
	r := newModRig(t)
	old := filepath.Join(t.TempDir(), "claude-burst-repo")
	must(t, os.MkdirAll(filepath.Join(old, "mods", "burst-band", ".claude-plugin"), 0o755))
	must(t, os.MkdirAll(filepath.Join(old, "mods", "burst-band", "hooks"), 0o755))
	must(t, os.WriteFile(filepath.Join(old, "mods", "burst-band", ".claude-plugin", "plugin.json"), []byte(`{"name":"burst-band","version":"0.0.1"}`), 0o644))
	must(t, os.WriteFile(filepath.Join(old, "mods", "burst-band", "hooks", "register.js"), []byte("// old\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(r.fake, "marketplace"), []byte(old+"\n"), 0o644))
	must(t, os.MkdirAll(filepath.Join(r.fake, "cache"), 0o755))
	must(t, os.WriteFile(filepath.Join(r.fake, "installed"), []byte(filepath.Join(r.fake, "cache")+"\n"), 0o644))

	if out := r.run(nil); !strings.Contains(out, "burst-band mod: updated") {
		t.Fatalf("want the new mod installed over the old checkout's: %s\ncalls:\n%s", out, r.calls())
	}
	b, err := os.ReadFile(filepath.Join(r.fake, "cache", ".claude-plugin", "plugin.json"))
	must(t, err)
	if !strings.Contains(string(b), "0.1.0") {
		t.Fatalf("the old checkout's mod is still installed: %s", b)
	}
	if m, _ := os.ReadFile(filepath.Join(r.fake, "marketplace")); !strings.Contains(string(m), ".local/share/claude-burst/marketplace") {
		t.Fatalf("the marketplace must move to the copy that stays put: %s", m)
	}
}

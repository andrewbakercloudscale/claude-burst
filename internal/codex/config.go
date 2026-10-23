package codex

import (
	"errors"
	"fmt"
	"github.com/BurntSushi/toml"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Codex is pointed at the gateway by a block Burst owns at the TOP of
// ~/.codex/config.toml, between these markers: top-level keys have to come
// before the first [table], and everything between the markers is Burst's to
// rewrite or remove. Nothing outside them is ever touched.
const (
	beginMark = "# BEGIN claude-burst"
	endMark   = "# END claude-burst"
	// ProviderID is the model provider's name in Codex's config.
	ProviderID = "claude-burst"
)

// ConfigPath is Codex's config file: $CODEX_HOME/config.toml, else
// ~/.codex/config.toml, the same lookup Codex makes.
func ConfigPath() (string, error) {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return filepath.Join(h, "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
}

// Status is whether Codex is routed through the gateway.
type Status struct {
	Path string `json:"path"`
	// Installed: Codex's home folder exists, so Codex has run on this Mac.
	Installed bool `json:"installed"`
	// Enabled: Burst's block is in config.toml.
	Enabled bool   `json:"enabled"`
	BaseURL string `json:"base_url,omitempty"`
	// Conflict names a model_provider the user set themselves, which the
	// block would collide with (TOML refuses a key set twice).
	Conflict string `json:"conflict,omitempty"`
	// Invalid is why config.toml does not parse: Codex cannot start either.
	Invalid string `json:"invalid,omitempty"`
}

// ReadStatus reports Codex's routing from its config file.
func ReadStatus(path string) Status {
	st := Status{Path: path}
	if fi, err := os.Stat(filepath.Dir(path)); err == nil && fi.IsDir() {
		st.Installed = true
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	ours, rest := split(string(b))
	st.Enabled = ours != ""
	doc, err := parse(string(b))
	if err != nil {
		st.Invalid = err.Error()
	} else if !st.Enabled && unmarked(doc) {
		st.Enabled, rest = true, ""
	}
	if st.Enabled && doc != nil {
		m, _ := doc["model_providers"].(map[string]any)
		p, _ := m[ProviderID].(map[string]any)
		st.BaseURL, _ = p["base_url"].(string)
	}
	if doc, err := parse(rest); err == nil {
		st.Conflict = topLevelProvider(doc)
	}
	return st
}

// unmarked reports whether the document holds Burst's provider without the
// markers around it. The Codex app rewrites config.toml when a setting
// changes in it, and that drops comments and turns the inline table into a
// [model_providers.claude-burst] table: Codex still went through Burst, while
// the dashboard said a provider of the user's own kept it away (23 Oct 2026).
// Burst's is the one it names "Claude Burst" on this Mac's loopback.
func unmarked(doc map[string]any) bool {
	if topLevelProvider(doc) != ProviderID {
		return false
	}
	m, _ := doc["model_providers"].(map[string]any)
	p, _ := m[ProviderID].(map[string]any)
	name, _ := p["name"].(string)
	url, _ := p["base_url"].(string)
	return name == "Claude Burst" && strings.HasPrefix(url, "http://127.0.0.1:")
}

var (
	providerLine = regexp.MustCompile(`^\s*model_provider\s*=\s*["']` + ProviderID + `["']\s*(#.*)?$`)
	inlineLine   = regexp.MustCompile(`^\s*model_providers\s*\.\s*("|')?` + ProviderID + `("|')?\s*=`)
	tableLine    = regexp.MustCompile(`^\s*\[\s*model_providers\s*\.\s*("|')?` + ProviderID + `("|')?\s*\]`)
)

// withoutUnmarked is the file with Burst's unmarked provider taken out: its
// model_provider line before the first table, and its table or inline table.
// ok is false when what is left still names it, so nothing is written.
func withoutUnmarked(s string) (string, bool) {
	var out []string
	top, inTable := true, false
	for _, l := range strings.SplitAfter(s, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") {
			top = false
			inTable = tableLine.MatchString(l)
			if inTable {
				continue
			}
		}
		if inTable || (top && (providerLine.MatchString(l) || inlineLine.MatchString(l))) {
			continue
		}
		out = append(out, l)
	}
	rest := strings.Join(out, "")
	doc, err := parse(rest)
	return rest, err == nil && topLevelProvider(doc) == "" && !hasOwnProvider(doc)
}

// split separates Burst's block from the rest of the file.
func split(s string) (ours, rest string) {
	i := strings.Index(s, beginMark)
	if i < 0 {
		return "", s
	}
	j := strings.Index(s[i:], endMark)
	if j < 0 {
		// A begin with no end: treat the file as Burst-free rather than
		// guess where the block stops and delete someone's settings.
		return "", s
	}
	j += i + len(endMark)
	if j < len(s) && s[j] == '\n' {
		j++
	}
	return s[i:j], s[:i] + s[j:]
}

// parse reads a config.toml. What Codex itself would refuse is refused
// here too, before anything is written.
func parse(s string) (map[string]any, error) {
	var doc map[string]any
	if _, err := toml.Decode(s, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// topLevelProvider is the document's own model_provider, "" when unset.
func topLevelProvider(doc map[string]any) string {
	if v, ok := doc["model_provider"]; ok {
		if p := fmt.Sprint(v); p != "" {
			return p
		}
		return "(empty)"
	}
	return ""
}

// hasOwnProvider reports whether the document defines model_providers.claude-burst.
func hasOwnProvider(doc map[string]any) bool {
	m, _ := doc["model_providers"].(map[string]any)
	_, ok := m[ProviderID]
	return ok
}

// Block is what Enable writes for a gateway listening on listen.
func Block(listen string) string {
	return beginMark + " (Codex through Claude Burst: delete down to END to undo, or run claude-burst codex disable)\n" +
		`model_provider = "` + ProviderID + `"` + "\n" +
		`model_providers.` + ProviderID + ` = { name = "Claude Burst", base_url = "http://` + listen +
		`/backend-api/codex", wire_api = "responses", requires_openai_auth = true }` + "\n" +
		endMark + "\n"
}

// ErrConflict is returned when config.toml already chooses a provider.
var ErrConflict = errors.New("codex config already sets model_provider")

// Enable routes Codex through the gateway. backupDir receives a copy of the
// file as it was. Codex reads its config when a session starts, so sessions
// already open keep going straight to ChatGPT until restarted.
func Enable(path, listen, backupDir string) error {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	_, rest := split(string(b))
	doc, err := parse(rest)
	if err != nil {
		return fmt.Errorf("%s is not valid TOML, so Codex cannot read it either; fix it first: %v", path, err)
	}
	// Burst's own provider without its markers is put back inside them.
	if unmarked(doc) {
		if r, ok := withoutUnmarked(rest); ok {
			rest = r
			doc, _ = parse(rest)
		}
	}
	if p := topLevelProvider(doc); p != "" {
		return fmt.Errorf("%w (model_provider = %q in %s): Codex is set to use that provider instead of Burst's; delete that line to route Codex through Burst", ErrConflict, p, path)
	}
	if hasOwnProvider(doc) {
		return fmt.Errorf("%w (%s already defines a %q provider of its own)", ErrConflict, path, ProviderID)
	}
	out := Block(listen) + rest
	// The result must parse too: nothing Codex would refuse is ever written.
	if _, err := parse(out); err != nil {
		return fmt.Errorf("routing Codex through Burst would leave %s invalid (%v); nothing was changed", path, err)
	}
	if len(b) > 0 {
		if err := backup(path, b, backupDir); err != nil {
			return err
		}
	}
	return write(path, out)
}

// Disable removes Burst's block, leaving the rest of the file as it is.
func Disable(path, backupDir string) error {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	ours, rest := split(string(b))
	if ours == "" {
		doc, err := parse(rest)
		if err != nil || !unmarked(doc) {
			return nil
		}
		var ok bool
		if rest, ok = withoutUnmarked(rest); !ok {
			return fmt.Errorf("%s names Burst's provider without Burst's markers, in a form Burst cannot take out safely; delete model_provider and [model_providers.%s] by hand", path, ProviderID)
		}
	}
	if err := backup(path, b, backupDir); err != nil {
		return err
	}
	return write(path, rest)
}

func backup(path string, b []byte, dir string) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	name := "codex-config.toml." + time.Now().Format("20060102-150405")
	return os.WriteFile(filepath.Join(dir, name), b, 0o600)
}

// write replaces the file atomically, keeping its permissions. A symlinked
// config.toml (a dotfiles repo) is written through: the link stays, and the
// file it points at gets the change.
func write(path, s string) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".claude-burst.tmp"
	if err := os.WriteFile(tmp, []byte(s), mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

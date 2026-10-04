package codex

import (
	"bufio"
	"errors"
	"fmt"
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
}

var (
	// model_provider as a bare or quoted key, with a basic or literal
	// string value: TOML allows all four, and missing one writes the key a
	// second time, which Codex refuses to start with.
	providerLine = regexp.MustCompile(`^\s*(?:model_provider|"model_provider"|'model_provider')\s*=\s*(?:"([^"]*)"|'([^']*)'|(\S+))`)
	baseURLRe    = regexp.MustCompile(`base_url\s*=\s*"([^"]*)"`)
	tableLine    = regexp.MustCompile(`^\s*\[`)
	ownTable     = regexp.MustCompile(`(?m)^\s*\[?\s*model_providers\.(?:claude-burst|"claude-burst"|'claude-burst')\s*(?:\]|=|\.|$)`)
)

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
	if ours != "" {
		st.Enabled = true
		if m := baseURLRe.FindStringSubmatch(ours); m != nil {
			st.BaseURL = m[1]
		}
	}
	st.Conflict = topLevelProvider(rest)
	return st
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

// topLevelProvider is a model_provider set before the first [table].
// Lines inside a multi-line string are skipped: a "[x]" or a
// "model_provider =" in one is text, not TOML.
func topLevelProvider(s string) string {
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	inMulti := ""
	for sc.Scan() {
		line := sc.Text()
		if inMulti != "" {
			if strings.Count(line, inMulti)%2 == 1 {
				inMulti = ""
			}
			continue
		}
		if tableLine.MatchString(line) {
			return ""
		}
		if m := providerLine.FindStringSubmatch(line); m != nil {
			for _, v := range m[1:] {
				if v != "" {
					return v
				}
			}
			return "(empty)"
		}
		for _, q := range []string{`"""`, "'''"} {
			if strings.Count(line, q)%2 == 1 {
				inMulti = q
				break
			}
		}
	}
	return ""
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
	if p := topLevelProvider(rest); p != "" {
		return fmt.Errorf("%w (model_provider = %q in %s): remove that line to route Codex through Burst", ErrConflict, p, path)
	}
	// The block defines model_providers.claude-burst; the same table defined
	// elsewhere would be a second definition, which TOML refuses.
	if ownTable.MatchString(rest) {
		return fmt.Errorf("%w (%s already defines a %q provider of its own)", ErrConflict, path, ProviderID)
	}
	if len(b) > 0 {
		if err := backup(path, b, backupDir); err != nil {
			return err
		}
	}
	return write(path, Block(listen)+rest)
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
		return nil
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

// write replaces the file atomically, keeping its permissions.
func write(path, s string) error {
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

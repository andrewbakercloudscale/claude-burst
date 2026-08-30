// Package claudesettings reads and edits ~/.claude/settings.json.
//
// Shared by the CLI and the admin server so the two can never disagree about
// what "enabled" means or how to undo it. Every edit is surgical: this file
// belongs to the user and holds their hooks, statusLine, model choice and
// permissions, so nothing here rewrites more than the one key it owns.
package claudesettings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const BaseURLKey = "ANTHROPIC_BASE_URL"

func Path() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".claude", "settings.json"), nil
}

// Read parses settings.json into a generic map so unknown keys survive a round
// trip. Note that Go marshals map keys in sorted order, so rewriting the file
// reorders it; harmless, but it is why the file looks churned after an edit.
func Read(p string) (map[string]any, error) {
	root := map[string]any{}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return root, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &root); err != nil {
			return nil, fmt.Errorf("refusing to edit invalid %s: %w", p, err)
		}
	}
	return root, nil
}

func Write(p string, root map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(b, '\n'), 0600)
}

// BaseURL returns the currently configured ANTHROPIC_BASE_URL, if any.
func BaseURL(root map[string]any) string {
	env, _ := root["env"].(map[string]any)
	if env == nil {
		return ""
	}
	v, _ := env[BaseURLKey].(string)
	return v
}

// ClearBaseURL removes ANTHROPIC_BASE_URL if it points at our own gateway,
// leaving a value the user set deliberately alone. Matching the configured
// listen address as well as the loopback prefix matters: with a non-loopback
// --listen a prefix-only check left the key stranded, silently keeping Claude
// Code pointed at a gateway that had just been disabled.
func ClearBaseURL(root map[string]any, listen string) bool {
	env, _ := root["env"].(map[string]any)
	if env == nil {
		return false
	}
	v, ok := env[BaseURLKey].(string)
	if !ok {
		return false
	}
	if v != "http://"+listen && v != "https://"+listen && !strings.HasPrefix(v, "http://127.0.0.1:") {
		return false
	}
	delete(env, BaseURLKey)
	if len(env) == 0 {
		delete(root, "env")
	} else {
		root["env"] = env
	}
	return true
}

// SetBaseURL points Claude Code at the gateway.
//
// It deliberately sets no credential: in oauth-passthrough mode that preserves
// the saved subscription OAuth, and in anthropic-api-key mode Claude Code's own
// key is forwarded unchanged.
func SetBaseURL(root map[string]any, url string) {
	env, _ := root["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	env[BaseURLKey] = url
	root["env"] = env
}

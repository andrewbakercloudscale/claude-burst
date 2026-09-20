// Package shunt hands bulk file reading and boilerplate generation to the
// secondary provider so the frontier model does not carry them in its context.
//
// The idea (Spotify's, rebuilt without their plugin) is that most of what a
// coding agent does is I/O rather than judgment: reading 25,000 tokens to
// produce 300 tokens of understanding. Three parts, all in this package:
//
//   - a PreToolUse guard that REFUSES whole-file reads above a threshold --
//     instructions alone do not hold once the agent is under pressure;
//   - a worker that answers a question over those files, or generates a file
//     straight to disk, using the configured openai-compatible secondary;
//   - a skill that tells Claude when to reach for the worker and, as
//     importantly, when not to.
package shunt

import (
	"path/filepath"
	"strings"
)

// IsSensitive reports whether a path looks like it holds credentials. Such
// files are never sent to the worker: it is a third-party endpoint, and a
// delegated read ships the whole file there. The guard lets a direct read
// through instead, which is the behaviour the user had before this existed.
//
// This is a name-based heuristic and errs toward refusing. It is a safety net,
// not a data-loss-prevention control.
func IsSensitive(path string) bool {
	clean := filepath.ToSlash(filepath.Clean(path))
	for _, part := range strings.Split(clean, "/") {
		switch part {
		case ".ssh", ".aws", ".gnupg", ".kube", ".docker":
			return true
		}
	}
	base := strings.ToLower(filepath.Base(clean))

	if base == ".env" || (strings.HasPrefix(base, ".env.") && !isEnvTemplate(base)) {
		return true
	}
	switch base {
	case ".netrc", ".npmrc", ".pypirc", ".pgpass", ".htpasswd", "credentials", "credentials.json",
		"secrets.json", "secrets.yaml", "secrets.yml", "secret.json", "service-account.json":
		return true
	}
	for _, prefix := range []string{"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519"} {
		if strings.HasPrefix(base, prefix) {
			return true
		}
	}
	for _, suffix := range []string{".pem", ".key", ".p12", ".pfx", ".keystore", ".jks", ".tfstate", ".tfvars", ".kdbx"} {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return false
}

func isEnvTemplate(base string) bool {
	for _, s := range []string{".example", ".sample", ".template", ".dist"} {
		if strings.HasSuffix(base, s) {
			return true
		}
	}
	return false
}

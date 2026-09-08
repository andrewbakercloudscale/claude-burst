package keychain

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strings"
)

func account() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if v := os.Getenv("USER"); v != "" {
		return v
	}
	return "claude-burst"
}

func Store(service, value string) error {
	if value == "" {
		return fmt.Errorf("empty key")
	}
	cmd := exec.Command("/usr/bin/security", "add-generic-password", "-U", "-a", account(), "-s", service, "-w", value)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("security add-generic-password: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Load returns the secret for service, checking envVar first and falling
// back to macOS Keychain. envVar is caller-specified (rather than hardcoded
// here) because different providers use different env vars for the same
// override convenience -- e.g. AWS_BEARER_TOKEN_BEDROCK for the "bedrock"
// service, TOGETHER_API_KEY for a "claude-burst-together" service. A single
// hardcoded env var here would silently hand one provider's secret to
// another provider's Load call.
func Load(service, envVar string) (string, error) {
	if v := os.Getenv(envVar); v != "" {
		return v, nil
	}
	cmd := exec.Command("/usr/bin/security", "find-generic-password", "-a", account(), "-s", service, "-w")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("key not found in %s or macOS Keychain (service %q)", envVar, service)
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "", fmt.Errorf("key is empty (service %q)", service)
	}
	return v, nil
}

func Delete(service string) error {
	cmd := exec.Command("/usr/bin/security", "delete-generic-password", "-a", account(), "-s", service)
	_ = cmd.Run()
	return nil
}

// Has reports whether a secret is available for service, without reading it.
//
// Deliberately drops Load's -w flag: the admin UI needs to answer "is a key
// stored for this provider?" and nothing more, and a value that is never
// fetched cannot end up in a log line, an error string, or a JSON response
// by accident. It checks envVar the same way Load does, so its answer is
// what Load would actually find rather than a Keychain-only view -- a key
// supplied purely through the environment must not read as missing.
func Has(service, envVar string) bool {
	if os.Getenv(envVar) != "" {
		return true
	}
	cmd := exec.Command("/usr/bin/security", "find-generic-password", "-a", account(), "-s", service)
	return cmd.Run() == nil
}

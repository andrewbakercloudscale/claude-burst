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
		return fmt.Errorf("empty Bedrock key")
	}
	cmd := exec.Command("/usr/bin/security", "add-generic-password", "-U", "-a", account(), "-s", service, "-w", value)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("security add-generic-password: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func Load(service string) (string, error) {
	if v := os.Getenv("AWS_BEARER_TOKEN_BEDROCK"); v != "" {
		return v, nil
	}
	cmd := exec.Command("/usr/bin/security", "find-generic-password", "-a", account(), "-s", service, "-w")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("Bedrock key not found in env or macOS Keychain")
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "", fmt.Errorf("Bedrock key is empty")
	}
	return v, nil
}

func Delete(service string) error {
	cmd := exec.Command("/usr/bin/security", "delete-generic-password", "-a", account(), "-s", service)
	_ = cmd.Run()
	return nil
}

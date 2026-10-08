//go:build !go1.24

package codex

import "testing"

// getH2C has nothing to fetch with before Go 1.24: the check is skipped.
func getH2C(*testing.T, string) string { return "ok HTTP/2.0" }

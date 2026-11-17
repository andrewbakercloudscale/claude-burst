package admin

import (
	"os"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/desktop"
)

// TestMain points Claude Desktop's lookup at an empty Applications folder:
// tests give themselves a temporary HOME, which does not hide the real
// /Applications/Claude.app of the Mac running them (17 Nov 2026).
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "burst-admin-apps-")
	if err != nil {
		panic(err)
	}
	os.Setenv(desktop.ApplicationsDirEnv, dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

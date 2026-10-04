package main

import (
	"path/filepath"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// claude-burst notice writes notices.json itself, gateway or not, under the
// same ext- kind /api/alert uses.
func TestNoticeCmdWritesWithoutTheGateway(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	noticeCmd([]string{"--kind", "pf-heal", "--severity", "error", "--title", "Burst redirect broken: repair 1 of 3 failed", "--detail", "d"})
	evs, err := notice.Read(filepath.Join(home, ".config", "claude-burst", "notices.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Kind != "ext-pf-heal" || evs[0].Severity != "error" || evs[0].Title != "Burst redirect broken: repair 1 of 3 failed" {
		t.Fatalf("events = %+v", evs)
	}
}

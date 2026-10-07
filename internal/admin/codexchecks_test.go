package admin

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func runCodexChecks(t *testing.T, c, s map[string]any) ([]pageCheck, string) {
	t.Helper()
	cj, _ := json.Marshal(c)
	sj, _ := json.Marshal(s)
	var got struct {
		Checks []pageCheck `json:"checks"`
		State  string      `json:"state"`
	}
	runPageJS(t, []string{"codexChecks", "checksState"}, fmt.Sprintf(`
const checks = codexChecks(%s, %s, Date.parse(%q));
out({checks: checks || [], state: checks ? checksState(checks) : "none"});`, cj, sj, jsTime(time.Date(2026, 10, 4, 18, 0, 0, 0, time.UTC))), &got)
	return got.Checks, got.State
}

func codexKeys(cs []pageCheck) string {
	var k []string
	for _, c := range cs {
		k = append(k, c.Key)
		if !c.OK {
			k[len(k)-1] += "!"
		}
	}
	return strings.Join(k, ",")
}

func TestCodexChecks(t *testing.T) {
	state := map[string]any{"intercept": map[string]any{"self_heal": map[string]any{"running": true}}}
	healthy := map[string]any{
		"status":    map[string]any{"installed": true, "enabled": true, "base_url": "http://127.0.0.1:7779", "path": "~/.codex/config.toml"},
		"listen":    "127.0.0.1:7779",
		"listening": true,
		"recent":    []any{map[string]any{"time": "2026-10-04T17:58:00Z", "http_status": 200}},
		"limits": map[string]any{"headers": map[string]any{
			"x-codex-primary-used-percent": "40", "x-codex-primary-window-minutes": "300"}},
		"history": map[string]any{"window": map[string]any{"Requests": 100}, "days": []any{map[string]any{"errors": 1}}},
	}
	cs, st := runCodexChecks(t, healthy, state)
	if got, want := codexKeys(cs), "cxroute,cxport,cxchatgpt,cxlimits,cxwatchdog,cxerrors"; got != want || st != "ok" {
		t.Fatalf("healthy: %s (%s), want %s ok", got, st, want)
	}

	// The newest real answer was a 401 four minutes ago: red. A 499 after it
	// is Codex hanging up and does not hide it.
	broken := map[string]any{}
	for k, v := range healthy {
		broken[k] = v
	}
	broken["recent"] = []any{
		map[string]any{"time": "2026-10-04T17:57:00Z", "http_status": 499},
		map[string]any{"time": "2026-10-04T17:56:00Z", "http_status": 401},
	}
	broken["limits"] = map[string]any{"headers": map[string]any{
		"x-codex-secondary-used-percent": "95", "x-codex-secondary-window-minutes": "10080"}}
	cs, st = runCodexChecks(t, broken, state)
	if got := codexKeys(cs); got != "cxroute,cxport,cxchatgpt!,cxlimits!,cxwatchdog,cxerrors" || st != "bad" {
		t.Fatalf("401 and 95%% weekly: %s (%s)", got, st)
	}
	for _, c := range cs {
		if c.Key == "cxchatgpt" && !strings.Contains(c.Detail, "sign-in was refused") {
			t.Errorf("401 detail: %q", c.Detail)
		}
	}

	// A turn that never left this Mac: Network down, and nothing about ChatGPT.
	broken["recent"] = []any{map[string]any{"time": "2026-10-04T17:58:00Z", "http_status": 502,
		"note": "upstream unreachable: dial tcp 104.18.32.47:443: connect: network is unreachable"}}
	cs, _ = runCodexChecks(t, broken, state)
	if got := codexKeys(cs); !strings.Contains(got, "network!") || strings.Contains(got, "cxchatgpt") {
		t.Fatalf("offline: %s", got)
	}
	// The gateway saying so for Claude Code counts for a bare 502 too.
	broken["recent"] = []any{map[string]any{"time": "2026-10-04T17:58:00Z", "http_status": 502}}
	off := map[string]any{"intercept": state["intercept"], "primary_health": map[string]any{"network_down": true, "last_failure": "2026-10-04T17:59:00Z"}}
	if cs, _ = runCodexChecks(t, broken, off); !strings.Contains(codexKeys(cs), "network!") {
		t.Fatalf("offline by the gateway's word: %s", codexKeys(cs))
	}

	// An old failure no longer counts.
	broken["recent"] = []any{map[string]any{"time": "2026-10-04T17:00:00Z", "http_status": 502}}
	if cs, _ = runCodexChecks(t, broken, state); strings.Contains(codexKeys(cs), "cxchatgpt!") {
		t.Errorf("an hour-old 502 still fails: %s", codexKeys(cs))
	}

	// Routed to a port nobody answers on: red.
	dead := map[string]any{"status": healthy["status"], "listen": "127.0.0.1:7779", "listening": false, "listen_error": "address in use"}
	if cs, st = runCodexChecks(t, dead, nil); codexKeys(cs) != "cxroute,cxport!,cxchatgpt" || st != "bad" {
		t.Errorf("dead port: %s (%s)", codexKeys(cs), st)
	}

	// Going straight to ChatGPT is a choice: amber, not red.
	direct := map[string]any{"status": map[string]any{"installed": true, "path": "x"}, "listen": "127.0.0.1:7779", "listening": true}
	if cs, st = runCodexChecks(t, direct, nil); codexKeys(cs) != "cxroute!,cxport,cxchatgpt" || st != "warn" {
		t.Errorf("not routed: %s (%s)", codexKeys(cs), st)
	}

	// Codex never ran here: no card.
	if _, st = runCodexChecks(t, map[string]any{"status": map[string]any{"installed": false}}, nil); st != "none" {
		t.Errorf("no Codex: %s", st)
	}
}

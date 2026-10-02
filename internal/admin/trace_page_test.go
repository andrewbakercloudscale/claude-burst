package admin

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// The trace as the page draws it: verdict first, one dotted row per hop in
// order, the state in words as well as colour, mtr hops in a table under
// the upstream hop, and every server string escaped.
func TestTraceHTML(t *testing.T) {
	r := traceResult{
		State: hopBad, Verdict: "Broken at TLS, as Claude Code's Node sees it: Node REFUSES <the> certificate.",
		Mode: "transparent", Target: "https://api.anthropic.com",
		Hops: []traceHop{
			{Key: "dns", Name: "DNS on this Mac", State: hopOK, DurationMS: 2, Summary: "resolves to 127.0.0.1"},
			{Key: "tls", Name: "TLS, as Claude Code's Node sees it", State: hopBad, Summary: "Node REFUSES <script>x</script>", Detail: "Trust used: Node"},
			{Key: "upstream", Name: "Upstream answer", State: hopWarn, DurationMS: 812, Summary: "HTTP 200",
				Legs: []traceLeg{
					{Name: "this Mac to the gateway", Note: "loopback, one local hop", Hops: []netHop{{Host: "127.0.0.1", Avg: 1, Worst: 1}}},
					{Name: "gateway to api.anthropic.com (160.79.104.10)", Hops: []netHop{{Host: "10.0.0.1", Loss: 33.3, Avg: 4.25, Worst: 9}}},
				}},
			{Key: "failover", Name: "Failover", State: hopSkip, Summary: "skipped (optional): no secondary is set up"},
		},
	}
	js, _ := json.Marshal(r)
	var got string
	runPageJS(t, []string{"traceHTML"}, fmt.Sprintf(`out(traceHTML(%s));`, js), &got)

	if !strings.HasPrefix(got, `<div class="trace"><div class="tr-verdict bad">Broken at TLS`) {
		t.Fatalf("the verdict must come first, coloured by the overall state: %s", got[:120])
	}
	rows := regexp.MustCompile(`<li class="tr-hop (\w+)">`).FindAllStringSubmatch(got, -1)
	var states []string
	for _, m := range rows {
		states = append(states, m[1])
	}
	if strings.Join(states, ",") != "ok,bad,warn,skip" {
		t.Fatalf("one row per hop, in order, with its state: %v", states)
	}
	for _, want := range []string{
		`<span class="tr-tag">failed</span>`, `<span class="tr-tag">skipped</span>`, `<span class="tr-tag">check, 812 ms</span>`,
		"<td>10.0.0.1</td><td>33.3%</td><td>4.3 ms</td><td>9.0 ms</td>",
		"loopback, one local hop", "transparent mode, sent to https://api.anthropic.com",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	if strings.Contains(got, "<script>") || !strings.Contains(got, "&lt;script&gt;") {
		t.Error("server text must be escaped")
	}
	if !strings.Contains(string(indexHTML), `<button class="success" id="setupTrace">&#x21CC; Send test message</button>`) ||
		!strings.Contains(string(indexHTML), `post("/api/trace")`) {
		t.Error("the Send test message button must sit in Setup and POST /api/trace")
	}
}

package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The dashboard's badge row and "Anthropic answering" check, run under node.
// Each case here is a moment on 2026-09-30 when the page looked healthy
// while traffic was failing or going to the secondary.

// pageFunc returns the source of a top-level `function name(` in the page,
// up to the first line that is exactly "}", or the whole of a one-line one.
func pageFunc(t *testing.T, name string) string {
	t.Helper()
	src := string(indexHTML)
	start := strings.Index(src, "\nfunction "+name+"(")
	if start < 0 {
		t.Fatalf("function %s not found in admin.html", name)
	}
	// A function written on one line ends where that line does.
	if nl := strings.Index(src[start+1:], "\n"); nl >= 0 && strings.HasSuffix(src[start+1:start+1+nl], "}") {
		return src[start : start+2+nl]
	}
	end := strings.Index(src[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("function %s has no closing brace at column 0", name)
	}
	return src[start : start+end+3]
}

func pageConst(t *testing.T, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^const ` + name + ` = .*$`).FindString(string(indexHTML))
	if m == "" {
		t.Fatalf("const %s not found in admin.html", name)
	}
	return m
}

// runPageJS evaluates the named page functions plus body under node and
// decodes what body prints with out(...).
func runPageJS(t *testing.T, funcs []string, body string, into any) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		// In CI a skip is a pass nobody sees: the dashboard's logic would
		// go untested for as long as the runner had no Node.
		if os.Getenv("CI") != "" {
			t.Fatal("node is not available in CI, so the dashboard's JavaScript is not being tested")
		}
		t.Skip("node not available")
	}
	var b strings.Builder
	b.WriteString(pageConst(t, "esc") + "\n")
	for _, f := range funcs {
		b.WriteString(pageFunc(t, f) + "\n")
	}
	b.WriteString("const out = v => process.stdout.write(JSON.stringify(v));\n")
	b.WriteString(body)
	cmd := exec.Command(node, "-e", b.String())
	cmd.Env = append(cmd.Environ(), "TZ=UTC")
	got, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("node: %v\n%s", err, ee.Stderr)
		}
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, into); err != nil {
		t.Fatalf("decode %q: %v", got, err)
	}
}

func jsTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// Per-model windows put a model on the secondary without the account-wide
// overflow flag. The badge used to read only the flag and said PRIMARY while
// Opus went to GLM.
func TestRouteBadgeShowsPerModelSecondary(t *testing.T) {
	now := time.Now()
	state := map[string]any{
		"overflow": false,
		"downgrade": map[string]any{"rejected": []map[string]any{
			{"model": "claude-opus-5", "until": jsTime(now.Add(5 * time.Minute))},
			{"model": "claude-haiku-4-5", "until": jsTime(now.Add(-time.Minute))}, // expired
		}},
	}
	js, _ := json.Marshal(state)
	var got map[string]string
	runPageJS(t, []string{"secondaryModels", "routeBadgeHTML"}, fmt.Sprintf(`
const s = %s, now = %d;
out({
  active: routeBadgeHTML(s, {ok: true}, now),
  unverified: routeBadgeHTML(s, {ok: false}, now),
  overflow: routeBadgeHTML({...s, overflow: true}, {ok: true}, now),
  clear: routeBadgeHTML({overflow: false}, {ok: true}, now),
});`, js, now.UnixMilli()), &got)

	if !strings.Contains(got["active"], "SECONDARY") || !strings.Contains(got["active"], "claude-opus-5") {
		t.Errorf("per-model window must show SECONDARY naming the model, got %q", got["active"])
	}
	if strings.Contains(got["active"], "claude-haiku-4-5") {
		t.Errorf("an expired window must not be listed, got %q", got["active"])
	}
	if got["unverified"] != "" {
		t.Errorf("no routing claim before the traffic path is confirmed, got %q", got["unverified"])
	}
	if !strings.Contains(got["overflow"], "OVERFLOW") {
		t.Errorf("account-wide overflow badge, got %q", got["overflow"])
	}
	if !strings.Contains(got["clear"], "PRIMARY") || strings.Contains(got["clear"], "SECONDARY") {
		t.Errorf("nothing on the secondary must read PRIMARY, got %q", got["clear"])
	}
}

// Force secondary in the header is greyed out with the reason while there is
// no secondary or everything is on it already, and otherwise says what it
// will send where and for how long.
func TestForceTopSaysWhatItWillDo(t *testing.T) {
	type st struct {
		Disabled bool   `json:"disabled"`
		Title    string `json:"title"`
	}
	var got map[string]st
	runPageJS(t, []string{"forceTopState"}, `
out({
  none: forceTopState({secondary_ready: false}, "15 minutes", ""),
  ready: forceTopState({secondary_ready: true}, "15 minutes", ""),
  already: forceTopState({secondary_ready: true, overflow: true}, "15 minutes", ""),
  one: forceTopState({secondary_ready: true, overflow: true}, "1 hour", "claude-sonnet-5"),
});`, &got)
	if !got["none"].Disabled || !strings.Contains(got["none"].Title, "No secondary") {
		t.Errorf("no secondary: %+v", got["none"])
	}
	if got["ready"].Disabled || !strings.Contains(got["ready"].Title, "every model to the secondary for 15 minutes") {
		t.Errorf("ready: %+v", got["ready"])
	}
	if !got["already"].Disabled || !strings.Contains(got["already"].Title, "already") {
		t.Errorf("everything on the secondary: %+v", got["already"])
	}
	if got["one"].Disabled || !strings.Contains(got["one"].Title, "claude-sonnet-5 to the secondary for 1 hour") {
		t.Errorf("one model: %+v", got["one"])
	}
}

// Force primary is always in the header; it is enabled exactly while
// something goes to the secondary, and its tooltip says what it will do.
func TestResetTopEnabledOnlyWhileOnSecondary(t *testing.T) {
	now := time.Now()
	type st struct {
		Disabled bool   `json:"disabled"`
		Title    string `json:"title"`
	}
	var got map[string]st
	runPageJS(t, []string{"secondaryModels", "resetTopState"}, fmt.Sprintf(`
const now = %d;
out({
  none: resetTopState({overflow: false}, now),
  expired: resetTopState({downgrade: {rejected: [{model: "m", until: %q}]}}, now),
  model: resetTopState({downgrade: {rejected: [{model: "claude-opus-5", until: %q}]}}, now),
  overflow: resetTopState({overflow: true}, now),
});`, now.UnixMilli(), jsTime(now.Add(-time.Minute)), jsTime(now.Add(time.Minute))), &got)

	if !got["none"].Disabled || !strings.Contains(got["none"].Title, "Already on primary") {
		t.Errorf("nothing on secondary: %+v", got["none"])
	}
	if !got["expired"].Disabled {
		t.Errorf("an expired window must not enable the button: %+v", got["expired"])
	}
	if got["model"].Disabled || !strings.Contains(got["model"].Title, "claude-opus-5") {
		t.Errorf("per-model window: %+v", got["model"])
	}
	if got["overflow"].Disabled || !strings.Contains(got["overflow"].Title, "every model") {
		t.Errorf("account-wide overflow: %+v", got["overflow"])
	}
}

// The button sits in the header beside the route badge and is never hidden:
// on 2026-09-30 it was four cards down while Opus went to GLM, and a hidden
// header copy could not be found when it was needed.
func TestResetTopIsInTheHeaderBesideTheRouteBadge(t *testing.T) {
	src := string(indexHTML)
	h0, h1 := strings.Index(src, "<header>"), strings.Index(src, "</header>")
	if h0 < 0 || h1 < h0 {
		t.Fatal("no <header> in admin.html")
	}
	header := src[h0:h1]
	badge := strings.Index(header, `id="routeBadge"`)
	btn := regexp.MustCompile(`<button[^>]*id="resetTop"[^>]*>`).FindStringIndex(header)
	if badge < 0 || btn == nil {
		t.Fatal("routeBadge and resetTop must both be in the header")
	}
	if between := header[badge:btn[0]]; strings.Contains(between, "<button") {
		t.Errorf("resetTop must come straight after the route badge, found another button between: %q", between)
	}
	if tag := header[btn[0]:btn[1]]; regexp.MustCompile(`\shidden[\s>=]`).MatchString(tag) {
		t.Errorf("resetTop must not be hidden: %s", tag)
	}
	if strings.Contains(src, `$("resetTop").hidden`) {
		t.Error("script hides resetTop again; it is disabled, not hidden, when not needed")
	}
}

// "Anthropic answering": failing while every request fails, clear once one
// answers, and not stuck red for ever after an old failure.
func TestPrimaryHealthCheck(t *testing.T) {
	now := time.Now()
	type check struct {
		OK       bool   `json:"ok"`
		Critical bool   `json:"critical"`
		Detail   string `json:"detail"`
		Label    string `json:"label"`
	}
	var got map[string]*check
	runPageJS(t, []string{"primaryHealthCheck"}, fmt.Sprintf(`
const now = %d, zero = "0001-01-01T00:00:00Z";
out({
  missing: primaryHealthCheck(undefined, now),
  fresh: primaryHealthCheck({failures: 0, last_answer: zero, last_failure: zero, failing_since: zero}, now),
  failing: primaryHealthCheck({failures: 7, last_answer: zero, last_failure: %q, failing_since: %q, last_error: "resolve api.anthropic.com over DoH"}, now),
  offline: primaryHealthCheck({failures: 113, network_down: true, last_answer: zero, last_failure: %q, failing_since: %q, last_error: "dial tcp 160.79.104.10:443: connect: network is unreachable"}, now),
  stale: primaryHealthCheck({failures: 2, last_answer: zero, last_failure: %q, failing_since: %q, last_error: "x"}, now),
  answering: primaryHealthCheck({failures: 0, last_answer: %q, last_failure: %q, failing_since: zero}, now),
});`, now.UnixMilli(),
		jsTime(now.Add(-30*time.Second)), jsTime(now.Add(-5*time.Minute)),
		jsTime(now.Add(-30*time.Second)), jsTime(now.Add(-5*time.Minute)),
		jsTime(now.Add(-11*time.Minute)), jsTime(now.Add(-12*time.Minute)),
		jsTime(now.Add(-time.Minute)), jsTime(now.Add(-time.Hour))), &got)

	if got["missing"] != nil {
		t.Errorf("no primary_health in the state: no check, got %+v", got["missing"])
	}
	if c := got["fresh"]; c == nil || !c.OK || !strings.Contains(c.Detail, "no request yet") {
		t.Errorf("fresh gateway (Go zero times): %+v", c)
	}
	if c := got["failing"]; c == nil || c.OK || !c.Critical ||
		!strings.Contains(c.Detail, "7 requests failed") ||
		!strings.Contains(c.Detail, "no answer since the gateway started") ||
		!strings.Contains(c.Detail, "DoH") {
		t.Errorf("every request failing must be a critical red check naming the error: %+v", c)
	}
	// Offline is said as that alone: no error text, nothing about Anthropic.
	if c := got["offline"]; c == nil || c.OK || c.Label != "Network down" ||
		strings.Contains(c.Detail, "dial") || strings.Contains(c.Detail, "113") {
		t.Errorf("offline: %+v", c)
	}
	if c := got["stale"]; c == nil || !c.OK {
		t.Errorf("a failure over 10 minutes old must not hold the check red: %+v", c)
	}
	if c := got["answering"]; c == nil || !c.OK || !strings.Contains(c.Detail, "last answer") {
		t.Errorf("answering: %+v", c)
	}
}

// /api/state carries primary_health with exactly the fields
// primaryHealthCheck reads. A renamed Go field would leave the check
// silently absent (it returns null for a missing object) or always green.
func TestStateCarriesPrimaryHealthForTheCheck(t *testing.T) {
	s := newTestServer(t)
	req := localRequest(http.MethodGet, "http://x/api/state", nil)
	req.Host = "127.0.0.1:7788"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		PrimaryHealth map[string]any `json:"primary_health"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.PrimaryHealth == nil {
		t.Fatal("/api/state has no primary_health; the Anthropic answering check would never render")
	}
	fn := pageFunc(t, "primaryHealthCheck")
	for _, k := range []string{"failures", "last_answer", "last_failure", "failing_since"} {
		if _, ok := body.PrimaryHealth[k]; !ok {
			t.Errorf("primary_health has no %q (got %v)", k, body.PrimaryHealth)
		}
		if !strings.Contains(fn, "ph."+k) {
			t.Errorf("primaryHealthCheck no longer reads ph.%s; update this test with it", k)
		}
	}
}

// The savings chart's series: up is the context compacted, down is what the
// summaries and cache rewrites cost, and net is kept from the server.
func TestSavingsSeries(t *testing.T) {
	type d struct {
		Date string  `json:"date"`
		Up   float64 `json:"up"`
		Down float64 `json:"down"`
		Net  float64 `json:"net"`
	}
	var got []d
	runPageJS(t, []string{"savingsSeries"}, `
out(savingsSeries([
  {date: "2026-09-29", saved_usd: 10, summary_usd: 1.5, rewrite_usd: 0.5, net_usd: 8, compactions: 2, requests: 30},
  {date: "2026-09-30"},
]));`, &got)
	if len(got) != 2 || got[0].Up != 10 || got[0].Down != 2 || got[0].Net != 8 {
		t.Fatalf("got %+v", got)
	}
	if got[1].Up != 0 || got[1].Down != 0 || got[1].Net != 0 {
		t.Fatalf("an empty day must be zeros, got %+v", got[1])
	}
}

// The chart is on the page in its own section, hidden until there is
// something to draw, with a legend and a table view.
func TestSavingsChartIsOnThePage(t *testing.T) {
	src := string(indexHTML)
	for _, want := range []string{`id="sec-savings" style="scroll-margin-top:70px" hidden`, `data-target="sec-savings"`, "Savings per day", "Savings from Pauseless Compaction", "Cost of Pauseless Compaction", `id="pcChartTable"`, "drawSavingsChart();"} {
		if !strings.Contains(src, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// The savings tiles give the share of the context compacted, and the
// money after costs with the costs as a share of the saving.
func TestSavingsTiles(t *testing.T) {
	type tile struct {
		Value string `json:"value"`
		Text  string `json:"text"`
	}
	var got map[string]map[string]tile
	runPageJS(t, []string{"savingsTiles", "signedUSD", "netUSD", "pct", "num"}, `
out({
  some: savingsTiles({compacted_requests: 931, tokens_not_resent: 317.2e6, twin_tokens: 446e6,
    saved_usd: 63.44, summary_usd: 6.84, rewrite_usd: 1.74, net_usd: 54.86, twin_usd: 89.2}),
  none: savingsTiles({}),
});`, &got)
	s := got["some"]
	if !strings.Contains(s["tokens"].Value, "317.2M") || !strings.Contains(s["tokens"].Value, "(71%)") ||
		!strings.Contains(s["tokens"].Text, "71% of the 446.0M that would have been sent without Burst, over 931 requests") {
		t.Errorf("tokens: %+v", s["tokens"])
	}
	m := s["money"]
	if !strings.HasPrefix(m.Value, "$54.86") || !strings.Contains(m.Value, "(62%)") || !strings.HasPrefix(m.Text, "async context savings, after costs") ||
		!strings.Contains(m.Text, "$63.44 saved, $8.58 costs (14% of the saving)") || !strings.Contains(m.Text, "62% off what these requests' context would have cost") {
		t.Errorf("money: %+v", m)
	}
	if n := got["none"]; n["tokens"].Value != "-" || n["money"].Value != "-" || strings.Contains(n["money"].Text, "NaN") {
		t.Errorf("none: %+v", n)
	}
}

// The share tables are built as HTML strings. A repository or model name
// comes from outside (a folder name, a provider's model id), so each table
// must show it as text: no tag or attribute of its own.
func TestShareTablesEscapeWhatComesFromOutside(t *testing.T) {
	const evil = `<img src=x onerror=alert(1)>"'><script>x</script>`
	h := map[string]any{"repos": []map[string]any{
		{"repo": evil, "path": evil, "sessions": 1, "requests": 2, "usd": 3.5, "compacted": true, "saved_usd": 1.25},
	}}
	js, _ := json.Marshal(h)
	var got map[string]string
	runPageJS(t, []string{"money", "num", "signedUSD", "netUSD", "sharePct", "shareTable", "renderRepoSplit"}, fmt.Sprintf(`
const els = {};
const $ = id => (els[id] = els[id] || {innerHTML: ""});
renderRepoSplit(%s);
out({
  repos: els.repoSplit.innerHTML,
  raw: shareTable("<b>", "<i>", [{h: "<u>", title: '"><s>'}],
    [{name: "ok", title: '"><script>', share: 0.5, color: '"><script>', cells: ["1"], cls: ['"><script>']}],
    {name: "Total", cells: ["1"], cls: ['"><script>']}),
});`, js), &got)
	for name, html := range got {
		if html == "" {
			t.Fatalf("%s: nothing was drawn", name)
		}
		for _, bad := range []string{"<img", "<script", "<b>", "<i>", "<u>", "<s>", `background:"`} {
			if strings.Contains(html, bad) {
				t.Errorf("%s: %q reached the page unescaped:\n%s", name, bad, html)
			}
		}
	}
	if !strings.Contains(got["repos"], "&lt;img src=x") {
		t.Errorf("the repository's name must still be shown, as text:\n%s", got["repos"])
	}
}

// The inspector's tables sort by the header clicked: size, age (the order
// the items were sent in) or name, either way round.
func TestInspectorOrder(t *testing.T) {
	var got map[string][]string
	runPageJS(t, []string{"inOrder"}, `
const rows = [{name: "b", tokens: 5}, {name: "c", tokens: 900}, {name: "a", tokens: 40}, {name: "d", tokens: 40}].map((it, i) => [it, i]);
const names = (k, desc) => inOrder(rows, k, desc).map(r => r[0].name);
out({big: names("size", true), small: names("size", false), old: names("age", false), fresh: names("age", true), name: names("name", false)});`, &got)
	want := map[string]string{"big": "cadb", "small": "badc", "old": "bcad", "fresh": "dacb", "name": "abcd"}
	for k, w := range want {
		if g := strings.Join(got[k], ""); g != w {
			t.Errorf("%s: got %s, want %s", k, g, w)
		}
	}
}

// The Money saved tile sums compaction's net saving over the days the other
// tiles cover, and has nothing to show before the savings have loaded.
func TestSavedOnTheTilesDays(t *testing.T) {
	var got []*float64
	runPageJS(t, []string{"savedOn"}, `
const daily = [{date: "2026-10-01", net_usd: 5}, {date: "2026-10-02", net_usd: -1.5}, {date: "2026-10-03", net_usd: 7}, {date: "2026-10-04"}];
out([savedOn(daily, ["2026-10-02", "2026-10-03", "2026-10-04", "2026-10-05"]), savedOn(null, ["2026-10-02"]), savedOn([], [])]);`, &got)
	if len(got) != 3 || got[0] == nil || *got[0] != 5.5 || got[1] != nil || got[2] == nil || *got[2] != 0 {
		t.Fatalf("got %v", got)
	}
}

// The address names the tab and the section as the menu does, and reads
// back to them: a name two tabs share goes by the tab, a section brings its
// own tab otherwise, and an address that names nothing known is left alone.
func TestTheAddressNamesTheTabAndSection(t *testing.T) {
	var got []any
	runPageJS(t, []string{"slugOf", "hashOf", "placeOf"}, `
const menu = [["claude", "Context inspector", "sec-inspect"], ["codex", "Context inspector", "sec-codex-inspect"], ["claude", "Context & cache", "sec-context"]]
  .map(([tab, label, id]) => ({tab, slug: slugOf(label), id}));
const p = h => { const at = placeOf(h, menu); return at ? at.tab + ":" + at.id : null; };
out([hashOf("claude", "sec-inspect", menu), hashOf("codex", "sec-codex-inspect", menu), hashOf("claude", "sec-context", menu), hashOf("codex", "~home", menu),
  p("#claude/context-inspector"), p("#codex/context-inspector"), p("#general/context-and-cache"), p("#claude/sec-inspect"), p("#general"), p("#codex/gone"), p("#nothing"), p("")]);`, &got)
	want := []any{"#claude/context-inspector", "#codex/context-inspector", "#claude/context-and-cache", "#codex",
		"claude:sec-inspect", "codex:sec-codex-inspect", "claude:sec-context", "claude:sec-inspect", "general:", "codex:", nil, nil}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

// The best size in hindsight: named for the view shown, with what Burst
// cost over it, and each repository on its own only beside all of them.
func TestStrategyBestNamesTheCheapestReplayedSize(t *testing.T) {
	var got map[string]*struct {
		At      int64   `json:"at"`
		USD     float64 `json:"usd"`
		Over    float64 `json:"over"`
		Share   float64 `json:"share"`
		PerRepo float64 `json:"perRepo"`
	}
	runPageJS(t, []string{"strategyBest"}, `
const repo = {repo: "a", actual: {usd: 90}, cheapest: {at: 150000, usd: 100}};
const st = {actual: {usd: 600}, cheapest: {at: 200000, usd: 480}, per_repo_usd: 450, repos: [repo]};
out({all: strategyBest(st, st), one: strategyBest(st, repo), old: strategyBest({actual: {usd: 5}}, {actual: {usd: 5}})});`, &got)
	if a := got["all"]; a == nil || a.At != 200_000 || a.USD != 480 || a.Over != 120 || a.Share != 0.25 || a.PerRepo != 450 {
		t.Errorf("all repositories: %+v", a)
	}
	if o := got["one"]; o == nil || o.At != 150_000 || o.Over != -10 || o.PerRepo != 0 {
		t.Errorf("one repository: %+v", o)
	}
	if got["old"] != nil {
		t.Errorf("no sweep in the data, want nothing: %+v", got["old"])
	}
}

func TestStrategyTrackSetsTheReplayBesideWhatHappened(t *testing.T) {
	var got map[string]*struct {
		Planned float64 `json:"planned"`
		Actual  float64 `json:"actual"`
		Off     float64 `json:"off"`
		Miss    float64 `json:"miss"`
		Gain    float64 `json:"gain"`
	}
	runPageJS(t, []string{"strategyTrack"}, `
const track = {since: "2026-10-08T12:00:00Z", requests: 400, planned: {usd: 80}, actual: {usd: 100}, fixed: {usd: 130}};
out({some: strategyTrack({track}), none: strategyTrack({actual: {usd: 5}}), empty: strategyTrack({track: {requests: 0, planned: {usd: 0}}}), few: strategyTrack({track: {...track, requests: 199}})});`, &got)
	if a := got["some"]; a == nil || a.Planned != 80 || a.Actual != 100 || a.Off != 20 || a.Miss != 0.25 || a.Gain != 30 {
		t.Errorf("a track: %+v", a)
	}
	if got["none"] != nil || got["empty"] != nil || got["few"] != nil {
		t.Errorf("nothing on record or too few requests, want nothing: %+v %+v %+v", got["none"], got["empty"], got["few"])
	}
}

package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/codex"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
)

// The Codex tab's two tests, drawn by the same traceHTML as Claude Code's
// Send test message:
//
//   - Trace (/api/codex/trace) walks Codex's path hop by hop: its config,
//     the gateway's port, its ChatGPT login, DNS, chatgpt.com directly, an
//     authenticated call through Burst, and the network path. It asks only
//     for the model list, so it costs no plan usage.
//   - Test turn (/api/codex/test-turn) runs the real Codex CLI once, forced
//     through Burst, and checks the turn was recorded. A few thousand
//     tokens of the plan.
var codexTraceMu sync.Mutex

// codexCLIPaths is where Codex's CLI is looked for after PATH: inside the
// ChatGPT app, then Homebrew.
var codexCLIPaths = []string{
	"/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex",
	"/Applications/Codex.app/Contents/Resources/codex",
	"/opt/homebrew/bin/codex",
	"/usr/local/bin/codex",
}

func codexHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

// codexLogin is Codex's ChatGPT sign-in from auth.json. The token is used
// for the one test request and never returned or logged.
type codexLogin struct {
	token, account string
	expires        time.Time
}

func readCodexLogin() (codexLogin, error) {
	b, err := os.ReadFile(filepath.Join(codexHome(), "auth.json"))
	if err != nil {
		return codexLogin{}, err
	}
	var a struct {
		AuthMode string `json:"auth_mode"`
		Tokens   struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return codexLogin{}, fmt.Errorf("auth.json is not readable JSON: %v", err)
	}
	if a.Tokens.AccessToken == "" {
		if a.AuthMode != "" && a.AuthMode != "chatgpt" {
			return codexLogin{}, fmt.Errorf("Codex signs in with %q, not ChatGPT", a.AuthMode)
		}
		return codexLogin{}, fmt.Errorf("no ChatGPT sign-in in auth.json")
	}
	l := codexLogin{token: a.Tokens.AccessToken, account: a.Tokens.AccountID}
	if parts := strings.Split(l.token, "."); len(parts) == 3 {
		if p, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
			var c struct {
				Exp int64 `json:"exp"`
			}
			if json.Unmarshal(p, &c) == nil && c.Exp > 0 {
				l.expires = time.Unix(c.Exp, 0)
			}
		}
	}
	return l, nil
}

// codexClientVersion is the version Codex last fetched its model list
// with; ChatGPT refuses the list without one.
func codexClientVersion() string {
	b, err := os.ReadFile(filepath.Join(codexHome(), "models_cache.json"))
	if err == nil {
		var c struct {
			ClientVersion string `json:"client_version"`
		}
		if json.Unmarshal(b, &c) == nil && c.ClientVersion != "" {
			return c.ClientVersion
		}
	}
	return "0.159.2"
}

func (s *Server) handleCodexTrace(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !codexTraceMu.TryLock() {
		http.Error(w, "a Codex test is already running; wait for it", http.StatusConflict)
		return
	}
	defer codexTraceMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
	defer cancel()
	writeJSON(w, s.runCodexTrace(ctx, cfg))
}

func (s *Server) runCodexTrace(ctx context.Context, cfg config.Config) traceResult {
	d := s.traceDeps()
	listen, upstream := cfg.CodexListen(), cfg.CodexUpstream()
	res := traceResult{Mode: "codex", Target: upstream + "/backend-api/codex"}
	add := func(h traceHop) { res.Hops = append(res.Hops, h) }

	// 1. Is Codex sent here at all?
	path, _ := codex.ConfigPath()
	st := codex.ReadStatus(path)
	cfgHop := traceHop{Key: "config", Name: "Codex's config"}
	switch {
	case st.Enabled:
		cfgHop.State, cfgHop.Summary = hopOK, "routes Codex through Burst: "+st.BaseURL
	case st.Conflict != "":
		cfgHop.State, cfgHop.Summary = hopWarn, fmt.Sprintf("Codex uses its own provider %q, so its turns do not come here", st.Conflict)
	case !st.Installed:
		cfgHop.State, cfgHop.Summary = hopWarn, "Codex has not run on this Mac"
	default:
		cfgHop.State, cfgHop.Summary = hopWarn, "Codex goes straight to ChatGPT; the hops below test Burst's path anyway"
	}
	cfgHop.Detail = path
	add(cfgHop)

	// 2. The gateway's port.
	gwHop := traceHop{Key: "gateway", Name: "Burst's Codex port"}
	if listen == "" {
		gwHop.State, gwHop.Summary = hopBad, `off: codex.listen is "off" in config.json`
		add(gwHop)
		return finishCodexTrace(res)
	}
	t0 := time.Now()
	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	conn, err := d.dial(dctx, "tcp", listen)
	cancel()
	gwHop.DurationMS = ms(time.Since(t0))
	if err != nil {
		gwHop.State, gwHop.Summary = hopBad, fmt.Sprintf("nothing answers on %s: %v", listen, err)
		if s.codexErr != "" {
			gwHop.Detail = "The gateway could not start it: " + s.codexErr
		}
		add(gwHop)
		return finishCodexTrace(res)
	}
	conn.Close()
	gwHop.State, gwHop.Summary = hopOK, "listening on "+listen
	add(gwHop)

	// 3. The ChatGPT sign-in Codex sends with every request.
	login, lerr := readCodexLogin()
	loginHop := traceHop{Key: "login", Name: "Codex's ChatGPT sign-in"}
	switch {
	case lerr != nil:
		loginHop.State, loginHop.Summary = hopBad, lerr.Error()
		loginHop.Detail = "Sign in again in Codex. The authenticated hop below is skipped."
	case !login.expires.IsZero() && time.Now().After(login.expires):
		loginHop.State = hopWarn
		loginHop.Summary = "the saved token expired " + login.expires.Local().Format("15:04 Mon 2 Jan") + "; Codex refreshes it when it next runs"
	default:
		loginHop.State, loginHop.Summary = hopOK, "found in auth.json"
		if !login.expires.IsZero() {
			loginHop.Summary += ", valid until " + login.expires.Local().Format("15:04 Mon 2 Jan")
		}
	}
	add(loginHop)

	// 4. DNS for the upstream.
	u, _ := url.Parse(upstream)
	host := u.Hostname()
	dnsHop := traceHop{Key: "dns", Name: "DNS for " + host}
	t0 = time.Now()
	lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	addrs, err := d.lookupHost(lctx, host)
	cancel()
	dnsHop.DurationMS = ms(time.Since(t0))
	if err != nil || len(addrs) == 0 {
		dnsHop.State, dnsHop.Summary = hopBad, fmt.Sprintf("could not resolve %s: %v", host, err)
		add(dnsHop)
		return finishCodexTrace(res)
	}
	dnsHop.State, dnsHop.Summary = hopOK, strings.Join(addrs, ", ")
	if anyLoopback(addrs) {
		dnsHop.State, dnsHop.Summary = hopBad, host+" resolves to this Mac ("+strings.Join(addrs, ", ")+"): something redirects it, so the gateway would call itself"
	}
	add(dnsHop)

	// 5. chatgpt.com directly, no credential: any HTTP answer proves the
	// network and TLS; 401 or 403 is the expected one.
	direct := traceHop{Key: "direct", Name: host + " directly"}
	modelsPath := "/backend-api/codex/models?client_version=" + url.QueryEscape(codexClientVersion())
	code, dur, _, err := codexGet(ctx, upstream+modelsPath, nil)
	direct.DurationMS = ms(dur)
	if err != nil {
		direct.State, direct.Summary = hopBad, "no answer: "+err.Error()
	} else {
		direct.State, direct.Summary = hopOK, fmt.Sprintf("HTTPS answers (HTTP %d without a sign-in, as expected)", code)
	}
	add(direct)

	// 6. Through Burst, signed in: what Codex itself does at session start.
	thru := traceHop{Key: "through", Name: "Through Burst, signed in"}
	if lerr != nil {
		thru.State, thru.Summary = hopSkip, "no sign-in to send"
	} else {
		h := http.Header{"Authorization": {"Bearer " + login.token}}
		if login.account != "" {
			h.Set("ChatGPT-Account-Id", login.account)
		}
		code, dur, body, err := codexGet(ctx, "http://"+listen+modelsPath, h)
		thru.DurationMS = ms(dur)
		switch {
		case err != nil:
			thru.State, thru.Summary = hopBad, "no answer from the gateway: "+err.Error()
		case code == http.StatusOK:
			n := len(codex.ParseWindows(body))
			thru.State, thru.Summary = hopOK, fmt.Sprintf("ChatGPT answered through Burst: %d models listed", n)
		case code == http.StatusUnauthorized || code == http.StatusForbidden:
			thru.State, thru.Summary = hopBad, fmt.Sprintf("ChatGPT refused the sign-in (HTTP %d): open Codex so it refreshes it, or sign in again", code)
		case code == http.StatusBadGateway:
			thru.State, thru.Summary = hopBad, "the gateway could not reach "+host+": "+firstLine(string(body), 200)
		default:
			thru.State, thru.Summary = hopBad, fmt.Sprintf("HTTP %d: %s", code, firstLine(string(body), 200))
		}
	}
	add(thru)

	// 7. The network path, as for Claude Code.
	path7 := traceHop{Key: "network", Name: "Network path", State: hopSkip}
	ip := addrs[0]
	if !anyLoopback(addrs) {
		leg := traceLeg{Name: fmt.Sprintf("this Mac to %s (%s)", host, ip)}
		mtrLeg(ctx, d, &leg, ip)
		path7.Legs = []traceLeg{leg}
		path7.Summary = "not measured: see below"
		if len(leg.Hops) > 0 {
			path7.State = hopOK
			last := leg.Hops[len(leg.Hops)-1]
			path7.Summary = fmt.Sprintf("%d hops, last %s at %.0f ms, %.0f%% loss", len(leg.Hops), last.Host, last.Avg, last.Loss)
			if last.Loss >= 50 {
				path7.State = hopWarn
			}
		}
	}
	add(path7)
	return finishCodexTrace(res)
}

func finishCodexTrace(res traceResult) traceResult {
	res.State = hopOK
	for _, h := range res.Hops {
		if h.State == hopBad {
			res.State = hopBad
			res.Verdict = h.Name + ": " + h.Summary
			return res
		}
		if h.State == hopWarn && res.State == hopOK {
			res.State, res.Verdict = hopWarn, h.Name+": "+h.Summary
		}
	}
	if res.State == hopOK {
		res.Verdict = "Codex's path through Burst works: ChatGPT answered signed in, through this Mac's gateway."
	}
	return res
}

// codexGet is one GET with the body read (up to 4MB) and timed.
func codexGet(ctx context.Context, rawURL string, h http.Header) (int, time.Duration, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, 0, nil, err
	}
	for k, v := range h {
		req.Header[k] = v
	}
	t0 := time.Now()
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext}}).Do(req)
	if err != nil {
		return 0, time.Since(t0), nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, time.Since(t0), b, nil
}

// codexTurnResult is what the test turn reports.
type codexTurnResult struct {
	OK         bool   `json:"ok"`
	Detail     string `json:"detail"`
	Reply      string `json:"reply,omitempty"`
	Model      string `json:"model,omitempty"`
	Tokens     int64  `json:"tokens,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Output     string `json:"output,omitempty"`
}

func (s *Server) handleCodexTestTurn(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !codexTraceMu.TryLock() {
		http.Error(w, "a Codex test is already running; wait for it", http.StatusConflict)
		return
	}
	defer codexTraceMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 100*time.Second)
	defer cancel()
	writeJSON(w, s.runCodexTestTurn(ctx, cfg))
}

// runCodexTestTurn runs `codex exec` once with Burst's provider given on
// the command line, so it tests Burst's path whether or not config.toml
// routes Codex here, and ignores the user's config (hooks, plugins, MCP) so
// it costs as little as a turn can. It then looks for the turn in Codex's
// request log: a reply that did not pass through Burst is a failure.
func (s *Server) runCodexTestTurn(ctx context.Context, cfg config.Config) codexTurnResult {
	listen := cfg.CodexListen()
	if listen == "" {
		return codexTurnResult{Detail: `The Codex gateway is off (codex.listen is "off" in config.json).`}
	}
	d := s.traceDeps()
	bin := d.findCodex()
	if bin == "" {
		return codexTurnResult{Detail: "Codex's command-line tool was not found (looked on PATH and in " + strings.Join(codexCLIPaths, ", ") + ")."}
	}
	tmp, err := os.MkdirTemp("", "burst-codex-test-")
	if err != nil {
		return codexTurnResult{Detail: err.Error()}
	}
	defer os.RemoveAll(tmp)
	last := filepath.Join(tmp, "last.txt")
	provider := `model_providers.claude-burst={name="Claude Burst",base_url="http://` + listen +
		`/backend-api/codex",wire_api="responses",requires_openai_auth=true}`
	start := time.Now()
	out, errOut, rerr := d.run(ctx, nil, bin, "exec", "--skip-git-repo-check", "--ephemeral", "--ignore-user-config", "--ignore-rules",
		"-s", "read-only", "-C", tmp, "-o", last,
		"-c", `model_provider="claude-burst"`, "-c", provider,
		"Reply with just the word pong.")
	dur := time.Since(start)
	reply, _ := os.ReadFile(last)
	res := codexTurnResult{Reply: strings.TrimSpace(string(reply)), DurationMS: ms(dur)}
	tail := strings.TrimSpace(string(errOut) + "\n" + string(out))
	if len(tail) > 1500 {
		tail = "..." + tail[len(tail)-1500:]
	}

	// The turn, as the gateway recorded it: this run's own, by the session
	// id Codex prints, so a real session busy at the same time is never
	// taken for it.
	sid := ""
	if m := codexSessionLine.FindStringSubmatch(string(errOut) + "\n" + string(out)); m != nil {
		sid = m[1]
	}
	var turn *metrics.Event
	if mp, err := config.CodexMetricsPath(); err == nil {
		if evs, err := metrics.Recent(mp, 20); err == nil {
			for i := range evs {
				if sid != "" && evs[i].SessionID == sid || sid == "" && !evs[i].Time.Before(start.Add(-time.Second)) {
					turn = &evs[i]
					break
				}
			}
		}
	}
	switch {
	case ctx.Err() != nil:
		res.Detail, res.Output = "Codex did not finish within 100s.", tail
	case rerr != nil:
		res.Detail, res.Output = "Codex failed: "+firstLine(lastLine(strings.Split(tail, "\n")), 300), tail
	case turn == nil:
		res.Detail, res.Output = "Codex answered, but no turn reached Burst's gateway: the reply did not come through it.", tail
	case turn.HTTPStatus >= 400:
		res.Detail, res.Output = fmt.Sprintf("The turn went through Burst and ChatGPT answered HTTP %d: %s", turn.HTTPStatus, turn.Note), tail
	default:
		res.OK = true
		res.Model = turn.Model
		res.Tokens = turn.InputTokens + turn.CacheReadTokens + turn.OutputTokens
		res.Detail = fmt.Sprintf("Codex answered %q through Burst in %.1fs: %s, %s tokens.", res.Reply, dur.Seconds(), turn.Model, fmtThousands(res.Tokens))
	}
	return res
}

func fmtThousands(n int64) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

var codexSessionLine = regexp.MustCompile(`(?m)^session id:\s*(\S+)`)

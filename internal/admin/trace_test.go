package admin

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/router"
	"github.com/andrewbakercloudscale/claude-burst/internal/tlsca"
)

// traceRig is a transparent-mode gateway on a real TLS listener, a fake
// Anthropic behind it, and trace dependencies that stand in for /etc/hosts
// (lookupHost) and the pf rule (dial sends :443 to the listener). Nothing
// leaves the machine and nothing touches the real Keychain.
type traceRig struct {
	s        *Server
	cfg      config.Config
	upHits   atomic.Int64
	upAuth   atomic.Value
	listener string
}

type rigOpts struct {
	bundleFromOtherCA bool // the 2026-10-02 rotation: the bundle holds a CA the gateway no longer serves
	upstreamStatus    int
}

func newTraceRig(t *testing.T, o rigOpts) *traceRig {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	rig := &traceRig{}
	if o.upstreamStatus == 0 {
		o.upstreamStatus = 200
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rig.upHits.Add(1)
		rig.upAuth.Store(r.Header.Get("Authorization"))
		if r.Header.Get(router.TraceHeader) != "" {
			t.Errorf("the trace header reached the provider")
		}
		w.Header().Set("content-type", "application/json")
		w.Header().Set("request-id", "req_trace123")
		w.WriteHeader(o.upstreamStatus)
		if o.upstreamStatus >= 400 {
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-haiku-4-5-20251001","content":[{"type":"text","text":"pong"}],"usage":{"input_tokens":20,"output_tokens":2}}`))
	}))
	t.Cleanup(up.Close)

	dir := t.TempDir()
	cfg := config.Default()
	cfg.AnthropicBaseURL = up.URL
	cfg.Intercept.Mode = config.InterceptTransparent
	cfg.Intercept.Host = "api.anthropic.com"
	cfg.Intercept.UpstreamAddr = "192.0.2.10" // pinned: no DoH lookups from a test
	cfg.Intercept.CADir = filepath.Join(dir, "ca")
	cfg.Intercept.CABundle = filepath.Join(dir, "bundle.pem")
	leaf, caPEM, err := tlsca.LoadOrCreate(cfg.Intercept.CADir, cfg.Intercept.Host)
	if err != nil {
		t.Fatal(err)
	}
	// Serve the chain with the CA in it, as a rotated CA's bridge does: an
	// untrusted self-signed certificate in the served chain is what OpenSSL
	// reports as SELF_SIGNED_CERT_IN_CHAIN.
	served := *leaf
	blk, _ := pem.Decode(caPEM)
	served.Certificate = append(append([][]byte{}, leaf.Certificate...), blk.Bytes)
	bundle := caPEM
	if o.bundleFromOtherCA {
		_, other, err := tlsca.LoadOrCreate(filepath.Join(dir, "other-ca"), cfg.Intercept.Host)
		if err != nil {
			t.Fatal(err)
		}
		bundle = other
	}
	if err := os.WriteFile(cfg.Intercept.CABundle, bundle, 0600); err != nil {
		t.Fatal(err)
	}

	gw, err := router.New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(gw)
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{served}}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	rig.listener = ts.Listener.Addr().String()
	_, lport, _ := net.SplitHostPort(rig.listener)

	s := New(gw, filepath.Join(dir, "metrics.jsonl"), "test", "", "/x/transparent-root.sh")
	s.trace = traceDeps{
		lookupHost: func(ctx context.Context, host string) ([]string, error) { return []string{"127.0.0.1"}, nil },
		dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr == "127.0.0.1:443" { // the pf rdr rule
				addr = rig.listener
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		findNode: func() string { return "" },
		findMtr:  func() string { return "" },
		// The Node check is given the pf'd port too: node itself cannot
		// use the dial hook.
		run: func(ctx context.Context, env []string, name string, args ...string) ([]byte, []byte, error) {
			for i, a := range args {
				if a == "443" {
					args[i] = lport
				}
			}
			return runCommand(ctx, env, name, args...)
		},
		keychainLogin: func() (string, error) {
			return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"kc-token","expiresAt":%d}}`, time.Now().Add(time.Hour).UnixMilli()), nil
		},
		getenv: func(string) string { return "" },
	}
	rig.s, rig.cfg = s, cfg
	return rig
}

func hopByKey(t *testing.T, r traceResult, key string) traceHop {
	t.Helper()
	for _, h := range r.Hops {
		if h.Key == key {
			return h
		}
	}
	t.Fatalf("no %s hop in %+v", key, r.Hops)
	return traceHop{}
}

func TestTraceEndToEnd(t *testing.T) {
	rig := newTraceRig(t, rigOpts{})
	r := rig.s.runTrace(context.Background(), rig.cfg)

	if r.State != hopOK || !strings.Contains(r.Verdict, "works end to end") {
		t.Fatalf("verdict: %s %q\n%s", r.State, r.Verdict, dumpHops(r))
	}
	want := []string{"dns", "pf", "tls", "credential", "route", "upstream", "failover"}
	for i, k := range want {
		if r.Hops[i].Key != k {
			t.Fatalf("hop %d is %s, want %s", i, r.Hops[i].Key, k)
		}
	}
	if h := hopByKey(t, r, "dns"); !strings.Contains(h.Summary, "127.0.0.1") {
		t.Errorf("dns: %+v", h)
	}
	if h := hopByKey(t, r, "pf"); h.State != hopOK || !strings.Contains(h.Summary, "reaches this gateway") {
		t.Errorf("pf: %+v", h)
	}
	if h := hopByKey(t, r, "tls"); h.State != hopOK || !strings.Contains(h.Detail, "Trust used: Go") || !strings.Contains(h.Detail, "bundle.pem") {
		t.Errorf("tls must verify against the bundle and say so: %+v", h)
	}
	if h := hopByKey(t, r, "route"); h.State != hopOK || !strings.HasPrefix(h.Summary, "primary: no overflow window") {
		t.Errorf("route: %+v", h)
	}
	up := hopByKey(t, r, "upstream")
	for _, w := range []string{"HTTP 200", "claude-haiku-4-5-20251001", "req_trace123", `"pong"`} {
		if !strings.Contains(up.Summary, w) {
			t.Errorf("upstream summary missing %q: %s", w, up.Summary)
		}
	}
	if len(up.Legs) != 2 || !strings.Contains(up.Legs[0].Note, "loopback") || len(up.Legs[0].Hops) != 1 ||
		!strings.Contains(up.Legs[1].Name, "192.0.2.10") || !strings.Contains(up.Legs[1].Note, "brew install mtr") {
		t.Errorf("network legs: %+v", up.Legs)
	}
	if h := hopByKey(t, r, "failover"); h.State != hopSkip || !strings.Contains(h.Summary, "primary answered") {
		t.Errorf("failover: %+v", h)
	}
	if got, _ := rig.upAuth.Load().(string); got != "Bearer kc-token" {
		t.Errorf("the upstream must get Claude Code's credential, got %q", got)
	}
}

// Claude Code's own most recent credential wins over the Keychain login.
func TestTracePrefersClaudeCodesLiveCredential(t *testing.T) {
	rig := newTraceRig(t, rigOpts{})
	req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(`{"model":"claude-haiku-4-5","messages":[]}`))
	req.Header.Set("Authorization", "Bearer live-token")
	rig.s.gateway.ServeHTTP(httptest.NewRecorder(), req)

	r := rig.s.runTrace(context.Background(), rig.cfg)
	if h := hopByKey(t, r, "credential"); h.State != hopOK || !strings.Contains(h.Summary, "Claude Code itself sent") {
		t.Fatalf("credential: %+v", h)
	}
	if got, _ := rig.upAuth.Load().(string); got != "Bearer live-token" {
		t.Fatalf("want the live credential, got %q", got)
	}
}

// 2026-10-02 replayed: the bundle holds a CA the gateway no longer serves.
// Both the Go emulation and, where node is installed, Node itself must name
// the refusal, and the verdict must point at TLS, not anywhere later.
func TestTraceNamesARotatedCA(t *testing.T) {
	rig := newTraceRig(t, rigOpts{bundleFromOtherCA: true})
	r := rig.s.runTrace(context.Background(), rig.cfg)
	h := hopByKey(t, r, "tls")
	if h.State != hopBad || !strings.Contains(h.Summary, "SELF_SIGNED_CERT_IN_CHAIN") {
		t.Fatalf("go emulation: %+v", h)
	}
	if r.State != hopBad || !strings.HasPrefix(r.Verdict, "Broken at TLS, as Claude Code's Node sees it: ") {
		t.Fatalf("the verdict must name TLS: %q", r.Verdict)
	}
	// The rest of the path is still tested, and says it went without checks.
	if up := hopByKey(t, r, "upstream"); up.State != hopOK || !strings.Contains(up.Detail, "certificate checks off") {
		t.Fatalf("upstream after a TLS failure: %+v", up)
	}

	node := findBinary("node", "/opt/homebrew/bin/node", "/usr/local/bin/node")
	if node == "" {
		t.Skip("node not available for the real-Node half")
	}
	rig.s.trace.findNode = func() string { return node }
	r = rig.s.runTrace(context.Background(), rig.cfg)
	h = hopByKey(t, r, "tls")
	if h.State != hopBad || !strings.Contains(h.Summary, "Node REFUSES") || !strings.Contains(h.Summary, "SELF_SIGNED_CERT_IN_CHAIN") ||
		!strings.Contains(h.Detail, "NODE_EXTRA_CA_CERTS="+rig.cfg.Intercept.CABundle) {
		t.Fatalf("real node: %+v", h)
	}

	// And with the right bundle, real Node trusts it.
	ok := newTraceRig(t, rigOpts{})
	ok.s.trace.findNode = func() string { return node }
	r = ok.s.runTrace(context.Background(), ok.cfg)
	if h := hopByKey(t, r, "tls"); h.State != hopOK || !strings.Contains(h.Summary, "Node trusts") {
		t.Fatalf("real node, right bundle: %+v", h)
	}
}

func TestTraceWithoutACredentialSaysSoAndSendsNothing(t *testing.T) {
	rig := newTraceRig(t, rigOpts{})
	rig.s.trace.keychainLogin = func() (string, error) { return "", errors.New("not found") }
	r := rig.s.runTrace(context.Background(), rig.cfg)
	if h := hopByKey(t, r, "credential"); h.State != hopBad || !strings.Contains(h.Detail, "Use Claude Code once") {
		t.Fatalf("credential: %+v", h)
	}
	if h := hopByKey(t, r, "upstream"); h.State != hopSkip {
		t.Fatalf("upstream: %+v", h)
	}
	if rig.upHits.Load() != 0 {
		t.Fatal("nothing may be sent without a credential")
	}
	if !strings.HasPrefix(r.Verdict, "Broken at Credential") {
		t.Fatalf("verdict: %q", r.Verdict)
	}
}

func TestTraceMissingHostsRedirect(t *testing.T) {
	rig := newTraceRig(t, rigOpts{})
	rig.s.trace.lookupHost = func(ctx context.Context, host string) ([]string, error) { return []string{"160.79.104.10"}, nil }
	r := rig.s.runTrace(context.Background(), rig.cfg)
	if h := hopByKey(t, r, "dns"); h.State != hopBad || !strings.Contains(h.Summary, "bypasses Burst") {
		t.Fatalf("dns: %+v", h)
	}
	for _, k := range []string{"pf", "tls", "route", "upstream", "failover"} {
		if h := hopByKey(t, r, k); h.State != hopSkip {
			t.Errorf("%s after a missing redirect must be skipped: %+v", k, h)
		}
	}
	if rig.upHits.Load() != 0 {
		t.Fatal("a test message must never go straight to Anthropic")
	}
}

// The live shape on 2026-10-02: the hosts entry is IPv4 only and Go's
// lookup (getaddrinfo with AI_ALL) adds Anthropic's real AAAA from DNS. An
// ordinary lookup, Claude Code's, returns 127.0.0.1 alone, so this is green
// and the trace dials the loopback answer even when it is listed second.
func TestTraceHostsRedirectWithRealAAAA(t *testing.T) {
	rig := newTraceRig(t, rigOpts{})
	rig.s.trace.lookupHost = func(ctx context.Context, host string) ([]string, error) {
		return []string{"2607:6bc0::10", "127.0.0.1"}, nil
	}
	r := rig.s.runTrace(context.Background(), rig.cfg)
	h := hopByKey(t, r, "dns")
	if h.State != hopOK || strings.Contains(h.Summary, "2607") || !strings.Contains(h.Detail, "2607:6bc0::10") {
		t.Fatalf("dns: %+v", h)
	}
	if pf := hopByKey(t, r, "pf"); pf.State != hopOK {
		t.Fatalf("pf must be reached through 127.0.0.1: %+v", pf)
	}
}

// A primary error with no secondary: the failover hop is grey, optional.
func TestTracePrimaryErrorWithoutSecondary(t *testing.T) {
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	rig := newTraceRig(t, rigOpts{upstreamStatus: 500})
	r := rig.s.runTrace(context.Background(), rig.cfg)
	if up := hopByKey(t, r, "upstream"); up.State != hopBad || !strings.Contains(up.Summary, "HTTP 500") || !strings.Contains(up.Summary, "Overloaded") {
		t.Fatalf("upstream: %+v", up)
	}
	if fo := hopByKey(t, r, "failover"); fo.State != hopSkip || !strings.Contains(fo.Summary, "skipped (optional)") {
		t.Fatalf("failover: %+v", fo)
	}
}

func TestFailoverHop(t *testing.T) {
	ok := router.TraceHop{Slot: "primary", Route: "anthropic", Status: 200}
	fail := router.TraceHop{Slot: "primary", Route: "anthropic", Status: 429, Note: "rate limited"}
	sec := router.TraceHop{Slot: "secondary", Route: "together", Model: "glm", Status: 200}
	secBad := router.TraceHop{Slot: "secondary", Route: "together", Status: 401}
	cases := []struct {
		name  string
		tr    router.Trace
		has   bool
		state string
		want  string
	}{
		{"answered", router.Trace{Hops: []router.TraceHop{ok}}, true, hopSkip, "not needed"},
		{"no secondary", router.Trace{Hops: []router.TraceHop{fail}}, false, hopSkip, "skipped (optional)"},
		{"not a failover error", router.Trace{Hops: []router.TraceHop{fail}}, true, hopWarn, "not tried"},
		{"failed over", router.Trace{Hops: []router.TraceHop{fail, sec}}, true, hopWarn, "secondary answered"},
		{"both failed", router.Trace{Hops: []router.TraceHop{fail, secBad}}, true, hopBad, "failed too"},
		{"direct", router.Trace{Reason: "a window is open", Hops: []router.TraceHop{sec}}, true, hopSkip, "directly"},
	}
	for _, c := range cases {
		h := failoverHop(c.tr, c.has)
		if h.State != c.state || !strings.Contains(h.Summary, c.want) {
			t.Errorf("%s: %+v", c.name, h)
		}
	}
}

func TestParseMtr(t *testing.T) {
	js := `{"report":{"mtr":{"src":"mac","dst":"160.79.104.10","tests":3},"hubs":[
	  {"count":1,"host":"192.168.1.1","Loss%":0.00,"Snt":3,"Last":2.1,"Avg":2.3,"Best":2.0,"Wrst":2.6,"StDev":0.3},
	  {"count":2,"host":"???","Loss%":100.00,"Snt":3,"Last":0.0,"Avg":0.0,"Best":0.0,"Wrst":0.0,"StDev":0.0},
	  {"count":3,"host":"160.79.104.10","Loss%":0.00,"Snt":3,"Last":21.0,"Avg":20.5,"Best":19.9,"Wrst":22.4,"StDev":1.0}]}}`
	h, err := parseMtrJSON([]byte(js))
	if err != nil || len(h) != 3 || h[1].Loss != 100 || h[2].Avg != 20.5 || h[2].Worst != 22.4 {
		t.Fatalf("json: %+v %v", h, err)
	}
	txt := "Start: 2026-10-02T13:00:00+0200\nHOST: mac                       Loss%   Snt   Last   Avg  Best  Wrst StDev\n" +
		"  1.|-- 192.168.1.1                0.0%     3    2.1   2.3   2.0   2.6   0.3\n" +
		"  2.|-- 160.79.104.10              33.3%    3   21.0  20.5  19.9  22.4   1.0\n"
	h, err = parseMtrText([]byte(txt))
	if err != nil || len(h) != 2 || h[1].Loss != 33.3 || h[1].Avg != 20.5 || h[1].Worst != 22.4 {
		t.Fatalf("text: %+v %v", h, err)
	}
}

// mtr present but not permitted (no setuid mtr-packet): the leg says how to
// fix it rather than failing the trace.
func TestTraceMtrNotPermitted(t *testing.T) {
	rig := newTraceRig(t, rigOpts{})
	rig.s.trace.findMtr = func() string { return "/opt/homebrew/sbin/mtr" }
	rig.s.trace.run = func(ctx context.Context, env []string, name string, args ...string) ([]byte, []byte, error) {
		// mtr 0.96 from Homebrew, as it really fails without a setuid mtr-packet.
		return nil, []byte("mtr-packet: Failure to open IPv4 sockets\nmtr-packet: Failure to open IPv6 sockets\nmtr: Failure to start mtr-packet: Invalid argument\n"), errors.New("exit status 1")
	}
	leg := rig.s.upstreamPath(context.Background(), rig.s.traceDeps(), rig.cfg)
	if !strings.Contains(leg.Note, "mtr not permitted") || !strings.Contains(leg.Note, "/opt/homebrew/sbin/mtr-packet") {
		t.Fatalf("leg: %+v", leg)
	}

	rig.s.trace.run = func(ctx context.Context, env []string, name string, args ...string) ([]byte, []byte, error) {
		if len(args) < 6 || args[len(args)-1] != "192.0.2.10" {
			t.Errorf("mtr must target the real address from the gateway's resolver, got %v", args)
		}
		return []byte(`{"report":{"hubs":[{"host":"192.0.2.10","Loss%":0,"Avg":20,"Wrst":25}]}}`), nil, nil
	}
	leg = rig.s.upstreamPath(context.Background(), rig.s.traceDeps(), rig.cfg)
	if len(leg.Hops) != 1 || leg.Hops[0].Host != "192.0.2.10" {
		t.Fatalf("leg: %+v", leg)
	}
}

// The endpoint keeps the admin guard: POST only, the mutation header, and a
// loopback Host. A browser on another origin can do none of these.
func TestTraceEndpointIsGuarded(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	do := func(method, host string, header bool) int {
		req := httptest.NewRequest(method, "http://x/api/trace", nil)
		req.Host = host
		if header {
			req.Header.Set(mutationHeader, "1")
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	if c := do(http.MethodGet, "127.0.0.1:7788", true); c != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", c)
	}
	if c := do(http.MethodPost, "127.0.0.1:7788", false); c != http.StatusForbidden {
		t.Errorf("no mutation header: %d", c)
	}
	if c := do(http.MethodPost, "evil.example:7788", true); c != http.StatusForbidden {
		t.Errorf("foreign Host: %d", c)
	}
}

func dumpHops(r traceResult) string {
	b, _ := json.MarshalIndent(r.Hops, "", "  ")
	return string(b)
}

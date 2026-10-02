package admin

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/claudesettings"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/router"
)

// Send test message: the "agentic trace route". One tiny real request goes
// down exactly the path Claude Code's traffic takes, and every hop on the
// way reports what it saw:
//
//	a. DNS: what the Anthropic hostname resolves to on this Mac
//	b. pf: whether port 443 there reaches this gateway
//	c. TLS: whether a Node client, with NODE_EXTRA_CA_CERTS, trusts what the
//	   gateway serves (the check that would have caught 2026-10-02)
//	   credential: whose auth the request carries
//	d. routing: primary or secondary, and why (from the gateway's own trace)
//	e. upstream: status, latency, model, request id, the start of the reply,
//	   and the network path to Anthropic (mtr)
//	f. failover: whether the secondary was tried, and how it went
//
// Every hop is measured from outside the gateway's own trust where it can
// be: the "path" readiness check uses the gateway's probe of itself, which
// passed throughout an outage in which no client could connect.

// traceModel is the cheapest current model; traceMaxTokens keeps the reply
// to a word or two.
const (
	traceModel     = "claude-haiku-4-5-20251001"
	traceMaxTokens = 16
	tracePrompt    = "Reply with exactly one word: pong"
	// traceSystem is Claude Code's own system prompt opening. A subscription
	// (OAuth) credential is accepted for Claude Code's requests; sending the
	// same opening keeps the test message one of those.
	traceSystem = "You are Claude Code, Anthropic's official CLI for Claude."
)

// claudeCodeKeychainService is where Claude Code keeps its login on macOS.
const claudeCodeKeychainService = "Claude Code-credentials"

// liveCredentialFresh is how recent Claude Code's last credential must be
// to be preferred over its Keychain login: OAuth access tokens are short
// lived, and an hour-old one may already have been refreshed away.
const liveCredentialFresh = 30 * time.Minute

// Hop states. skip is grey: not applicable, or optional and not set up.
const (
	hopOK   = "ok"
	hopWarn = "warn"
	hopBad  = "bad"
	hopSkip = "skip"
)

type traceHop struct {
	Key        string     `json:"key"`
	Name       string     `json:"name"`
	State      string     `json:"state"`
	DurationMS int64      `json:"duration_ms,omitempty"`
	Summary    string     `json:"summary"`
	Detail     string     `json:"detail,omitempty"`
	Legs       []traceLeg `json:"legs,omitempty"`
}

// traceLeg is one network leg of the path, with its hops as mtr saw them.
type traceLeg struct {
	Name string   `json:"name"`
	Note string   `json:"note,omitempty"`
	Hops []netHop `json:"hops,omitempty"`
}

type netHop struct {
	Host  string  `json:"host"`
	Loss  float64 `json:"loss_pct"`
	Avg   float64 `json:"avg_ms"`
	Worst float64 `json:"worst_ms"`
}

type traceResult struct {
	State   string     `json:"state"`
	Verdict string     `json:"verdict"`
	Mode    string     `json:"mode"`
	Target  string     `json:"target"`
	Hops    []traceHop `json:"hops"`
}

// traceDeps is everything the trace touches outside this process, as fields
// so tests run it without the network, node, mtr or the Keychain.
type traceDeps struct {
	lookupHost func(ctx context.Context, host string) ([]string, error)
	dial       func(ctx context.Context, network, addr string) (net.Conn, error)
	findNode   func() string
	findMtr    func() string
	run        func(ctx context.Context, env []string, name string, args ...string) (stdout, stderr []byte, err error)
	// keychainLogin returns Claude Code's stored login (JSON).
	keychainLogin func() (string, error)
	getenv        func(string) string
	// settingsBaseURL is ANTHROPIC_BASE_URL from Claude Code's settings.
	settingsBaseURL func() string
}

func (s *Server) traceDeps() traceDeps {
	d := s.trace
	if d.lookupHost == nil {
		d.lookupHost = net.DefaultResolver.LookupHost
	}
	if d.dial == nil {
		d.dial = (&net.Dialer{Timeout: 3 * time.Second}).DialContext
	}
	if d.findNode == nil {
		d.findNode = func() string {
			return findBinary("node", "/opt/homebrew/bin/node", "/usr/local/bin/node")
		}
	}
	if d.findMtr == nil {
		d.findMtr = func() string {
			return findBinary("mtr", "/opt/homebrew/sbin/mtr", "/usr/local/sbin/mtr", "/opt/homebrew/bin/mtr", "/usr/local/bin/mtr")
		}
	}
	if d.run == nil {
		d.run = runCommand
	}
	if d.keychainLogin == nil {
		load := s.loadKey
		d.keychainLogin = func() (string, error) { return load(claudeCodeKeychainService, "") }
	}
	if d.getenv == nil {
		d.getenv = os.Getenv
	}
	if d.settingsBaseURL == nil {
		d.settingsBaseURL = func() string {
			if p, err := claudesettings.Path(); err == nil {
				if root, err := claudesettings.Read(p); err == nil {
					return claudesettings.BaseURL(root)
				}
			}
			return ""
		}
	}
	return d
}

// findBinary looks on PATH first, then in the usual Homebrew places: the
// gateway runs under launchd, whose PATH has none of them.
func findBinary(name string, fallbacks ...string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, p := range fallbacks {
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

func runCommand(ctx context.Context, env []string, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if env != nil {
		cmd.Env = env
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

// handleTrace runs the trace. POST and mutation-guarded like Test secondary:
// it spends real (tiny) tokens. Never an HTTP error for a failed hop: the
// trace is the answer.
func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !traceMu.TryLock() {
		http.Error(w, "a test message is already on its way; wait for its trace", http.StatusConflict)
		return
	}
	defer traceMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
	defer cancel()
	writeJSON(w, s.runTrace(ctx, cfg))
}

func ms(d time.Duration) int64 {
	if n := d.Milliseconds(); n > 0 {
		return n
	}
	return 1
}

func (s *Server) runTrace(ctx context.Context, cfg config.Config) traceResult {
	d := s.traceDeps()
	transparent := cfg.Intercept.Transparent()
	res := traceResult{Mode: config.InterceptBaseURL}

	var target *url.URL
	if transparent {
		res.Mode = config.InterceptTransparent
		target = &url.URL{Scheme: "https", Host: cfg.Intercept.Host}
	} else {
		raw := d.settingsBaseURL()
		if raw == "" {
			raw = "http://" + cfg.Listen
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			u = &url.URL{Scheme: "http", Host: cfg.Listen}
		}
		target = u
	}
	res.Target = target.String()

	// The network path to Anthropic runs alongside everything else: mtr
	// takes a few seconds and needs nothing from the other hops.
	pathCh := make(chan traceLeg, 1)
	go func() { pathCh <- s.upstreamPath(ctx, d, cfg) }()

	// a. DNS
	host := target.Hostname()
	port := target.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[target.Scheme]
	}
	dialAddr := net.JoinHostPort(host, port)
	reachable := true
	dns := traceHop{Key: "dns", Name: "DNS on this Mac"}
	if ip := net.ParseIP(host); ip != nil {
		dns.State, dns.Summary = hopSkip, host+" is an address, so there is nothing to resolve (base-url mode names the gateway directly)"
	} else {
		start := time.Now()
		lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		addrs, err := d.lookupHost(lctx, host)
		cancel()
		dns.DurationMS = ms(time.Since(start))
		switch {
		case err != nil:
			dns.State, dns.Summary = hopBad, fmt.Sprintf("%s did not resolve: %v", host, err)
			reachable = false
		case transparent && allLoopback(addrs):
			dns.State = hopOK
			dns.Summary = fmt.Sprintf("%s resolves to %s: the /etc/hosts redirect sends Claude Code to this Mac", host, strings.Join(addrs, ", "))
			dialAddr = net.JoinHostPort(addrs[0], port)
		case transparent:
			dns.State = hopBad
			dns.Summary = fmt.Sprintf("%s resolves to %s, Anthropic's real address: the /etc/hosts redirect is missing, so Claude Code goes straight to Anthropic and bypasses Burst", host, strings.Join(addrs, ", "))
			dns.Detail = "Run the transparent install again from Setup."
			reachable = false
		default:
			dns.State, dns.Summary = hopOK, fmt.Sprintf("%s resolves to %s", host, strings.Join(addrs, ", "))
			dialAddr = net.JoinHostPort(addrs[0], port)
		}
	}
	res.Hops = append(res.Hops, dns)

	// b. pf redirect
	pf := traceHop{Key: "pf", Name: "pf redirect to the gateway"}
	var loopbackMS int64
	switch {
	case !transparent:
		pf.State, pf.Summary = hopSkip, "not used in base-url mode: Claude Code connects to "+target.Host+" directly"
	case !reachable:
		pf.State, pf.Summary = hopSkip, "not reached: DNS does not send the hostname here"
	default:
		// One connection, not a bare TCP probe first: a connection closed
		// before its handshake is exactly what the certificate check counts
		// as a client hanging up, and the trace must not add to that.
		var dialErr error
		var dialDur time.Duration
		counted := func(ctx context.Context, network, addr string) (net.Conn, error) {
			start := time.Now()
			dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			c, err := d.dial(dctx, network, addr)
			dialDur, dialErr = time.Since(start), err
			return c, err
		}
		ours, status, herr := s.isOurGateway(ctx, counted, host, dialAddr)
		loopbackMS = ms(dialDur)
		pf.DurationMS = loopbackMS
		switch {
		case dialErr != nil:
			pf.State = hopBad
			pf.Summary = fmt.Sprintf("nothing accepts %s: the pf rule that forwards 443 to the gateway (%s) is not loaded", dialAddr, cfg.Listen)
			pf.Detail = dialErr.Error() + ". Arm the pf redirect guard under Guards, or run the transparent install again."
			loopbackMS = 0
			reachable = false
		case herr != nil:
			pf.State, pf.Summary = hopWarn, fmt.Sprintf("%s accepts connections, but /healthz failed: %v", dialAddr, herr)
		case ours:
			pf.State, pf.Summary = hopOK, fmt.Sprintf("%s reaches this gateway (its /healthz answered HTTP %d)", dialAddr, status)
		default:
			pf.State = hopBad
			pf.Summary = fmt.Sprintf("%s answers, but it is not this gateway (HTTP %d without the gateway's own fields): something else holds 443", dialAddr, status)
			reachable = false
		}
	}
	res.Hops = append(res.Hops, pf)

	// c. TLS as Node sees it
	tlsHop := traceHop{Key: "tls", Name: "TLS, as Claude Code's Node sees it"}
	switch {
	case target.Scheme != "https":
		tlsHop.State, tlsHop.Summary = hopSkip, "plain HTTP to the gateway in base-url mode, no certificate involved"
	case !reachable:
		tlsHop.State, tlsHop.Summary = hopSkip, "not reached"
	default:
		start := time.Now()
		tlsHop.State, tlsHop.Summary, tlsHop.Detail = s.nodeTLS(ctx, d, cfg, host, dialAddr)
		tlsHop.DurationMS = ms(time.Since(start))
	}
	res.Hops = append(res.Hops, tlsHop)

	// Credential
	cred, credHop := s.traceCredential(d)
	res.Hops = append(res.Hops, credHop)

	// d, e, f
	route := traceHop{Key: "route", Name: "Gateway routing decision"}
	up := traceHop{Key: "upstream", Name: "Upstream answer"}
	fo := traceHop{Key: "failover", Name: "Failover"}
	switch {
	case !reachable:
		route.State, route.Summary = hopSkip, "not reached: the request cannot get to the gateway"
		up.State, up.Summary = hopSkip, "not sent"
		fo.State, fo.Summary = hopSkip, "not reached"
	case cred == nil:
		route.State, route.Summary = hopSkip, "not sent: no credential to send it with (see above)"
		up.State, up.Summary = hopSkip, "not sent"
		fo.State, fo.Summary = hopSkip, "not sent"
	default:
		s.sendTraced(ctx, d, cfg, target, dialAddr, cred, &route, &up, &fo)
	}

	leg := <-pathCh
	local := traceLeg{Name: "this Mac to the gateway"}
	switch {
	case transparent && loopbackMS > 0:
		local.Note = "loopback, one local hop (TCP connect time)"
		local.Hops = []netHop{{Host: strings.Trim(dialAddr[:strings.LastIndex(dialAddr, ":")], "[]"), Avg: float64(loopbackMS), Worst: float64(loopbackMS)}}
	case !transparent:
		local.Note = "loopback, one local hop: Claude Code connects to " + target.Host
	default:
		local.Note = "not measured: the gateway was not reached"
	}
	up.Legs = []traceLeg{local, leg}
	res.Hops = append(res.Hops, route, up, fo)

	res.State, res.Verdict = traceVerdict(res.Hops)
	return res
}

func allLoopback(addrs []string) bool {
	if len(addrs) == 0 {
		return false
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil || !ip.IsLoopback() {
			return false
		}
	}
	return true
}

// isOurGateway asks /healthz at addr under the intercepted name. Trust is
// not this hop's question (hop c's is), so verification is off here.
func (s *Server) isOurGateway(ctx context.Context, dial func(context.Context, string, string) (net.Conn, error), host, addr string) (bool, int, error) {
	tr := &http.Transport{
		DialContext:     func(ctx context.Context, network, _ string) (net.Conn, error) { return dial(ctx, network, addr) },
		TLSClientConfig: &tls.Config{ServerName: host, InsecureSkipVerify: true}, // #nosec G402 -- only identifies who answers; trust is checked separately
	}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 4 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/healthz", nil)
	if err != nil {
		return false, 0, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return false, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return strings.Contains(string(body), `"overflow"`), resp.StatusCode, nil
}

// nodeTLSScript connects the way Claude Code's Node does and reports Node's
// own verdict. rejectUnauthorized is off only so the reason can be read
// rather than thrown; `authorized` is the verdict Claude Code acts on.
const nodeTLSScript = `
const tls = require("tls");
const [host, port, addr] = process.argv.slice(1);
let done = false;
const say = o => { if (!done) { done = true; process.stdout.write(JSON.stringify(o)); } };
const s = tls.connect({host: addr, port: Number(port), servername: host, rejectUnauthorized: false}, () => {
  const chain = [];
  const seen = new Set();
  let c = s.getPeerCertificate(true);
  while (c && c.subject && !seen.has(c.fingerprint256)) {
    seen.add(c.fingerprint256);
    chain.push({subject: c.subject.CN || c.subject.O || "", issuer: c.issuer.CN || c.issuer.O || ""});
    c = c.issuerCertificate;
  }
  say({authorized: s.authorized, error: s.authorizationError ? String(s.authorizationError) : "", chain, node: process.version});
  s.end();
});
s.on("error", e => say({connect_error: String(e && e.message || e)}));
s.setTimeout(5000, () => { say({connect_error: "timed out after 5s"}); s.destroy(); });
`

type nodeTLSResult struct {
	Authorized   bool   `json:"authorized"`
	Error        string `json:"error"`
	ConnectError string `json:"connect_error"`
	Node         string `json:"node"`
	Chain        []struct {
		Subject string `json:"subject"`
		Issuer  string `json:"issuer"`
	} `json:"chain"`
}

func (s *Server) nodeTLS(ctx context.Context, d traceDeps, cfg config.Config, host, addr string) (state, summary, detail string) {
	bundle := cfg.Intercept.CABundle
	ahost, aport, _ := net.SplitHostPort(addr)
	staleNote := " A fresh process reads the bundle now; a Claude Code session started earlier keeps the bundle it read at its own start, which only the certificate check on the dashboard can see."
	if node := d.findNode(); node != "" {
		env := append(os.Environ(), "NODE_EXTRA_CA_CERTS="+bundle)
		nctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		out, errOut, err := d.run(nctx, env, node, "-e", nodeTLSScript, host, aport, ahost)
		cancel()
		var r nodeTLSResult
		if jerr := json.Unmarshal(bytes.TrimSpace(out), &r); jerr != nil {
			msg := strings.TrimSpace(string(errOut))
			if err != nil && msg == "" {
				msg = err.Error()
			}
			return hopWarn, "could not run the Node check: " + firstLine(msg, 200), "node: " + node
		}
		trust := fmt.Sprintf("Trust used: Node %s at %s, its bundled roots plus NODE_EXTRA_CA_CERTS=%s.", r.Node, node, bundle)
		if w := strings.TrimSpace(string(errOut)); w != "" {
			// Node warns, but carries on, when the bundle cannot be read.
			trust += " Node warned: " + firstLine(w, 300)
		}
		chain := describeChain(r.Chain)
		switch {
		case r.ConnectError != "":
			return hopBad, "Node could not complete a TLS connection: " + r.ConnectError, trust
		case r.Authorized:
			return hopOK, "Node trusts the gateway's certificate" + chain, trust + staleNote
		default:
			return hopBad, fmt.Sprintf("Node REFUSES the gateway's certificate: %s%s. Claude Code would fail the same way", r.Error, chain),
				trust + " Check that NODE_EXTRA_CA_CERTS in Claude Code's environment names this bundle and that the bundle holds the current Claude Burst CA (claude-burst enable puts it back)."
		}
	}

	// No node: verify in Go against the bundle alone. Node's own roots are
	// public CAs, which cannot vouch for a local certificate anyway, and the
	// macOS keychain is deliberately left out: a CA trusted there but missing
	// from the bundle passes every Mac app and still fails Claude Code.
	trust := "Trust used: Go, emulating Node, against " + bundle + " only (node was not found on PATH or in Homebrew)."
	pool, n, err := bundlePool(bundle)
	if err != nil {
		return hopBad, "the NODE_EXTRA_CA_CERTS bundle cannot be read: " + err.Error(), trust
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := d.dial(dctx, "tcp", addr)
	if err != nil {
		return hopBad, "could not connect: " + err.Error(), trust
	}
	conn := tls.Client(raw, &tls.Config{ServerName: host, InsecureSkipVerify: true}) // #nosec G402 -- verified by hand below, OpenSSL-style
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := conn.HandshakeContext(dctx); err != nil {
		return hopBad, "TLS handshake failed: " + err.Error(), trust
	}
	certs := conn.ConnectionState().PeerCertificates
	if verr := verifyLikeOpenSSL(certs, pool, host); verr != nil {
		return hopBad, "the certificate would be REFUSED: " + verr.Error(),
			fmt.Sprintf("%s The bundle holds %d certificate(s).", trust, n)
	}
	return hopOK, "the certificate verifies against the bundle", trust + staleNote
}

func describeChain(chain []struct {
	Subject string `json:"subject"`
	Issuer  string `json:"issuer"`
}) string {
	if len(chain) == 0 {
		return ""
	}
	parts := make([]string, 0, len(chain))
	for _, c := range chain {
		parts = append(parts, c.Subject)
	}
	return " (chain: " + strings.Join(parts, " > ") + ")"
}

func bundlePool(path string) (*x509.CertPool, int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	pool := x509.NewCertPool()
	n := 0
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		if c, err := x509.ParseCertificate(blk.Bytes); err == nil {
			pool.AddCert(c)
			n++
		}
	}
	if n == 0 {
		return nil, 0, fmt.Errorf("%s holds no certificates", path)
	}
	return pool, n, nil
}

// verifyLikeOpenSSL verifies the served chain the way OpenSSL reports it:
// Go's verifier quietly ignores an untrusted self-signed certificate in the
// served chain, OpenSSL names it SELF_SIGNED_CERT_IN_CHAIN, which is the
// error Claude Code showed on 2026-10-02.
func verifyLikeOpenSSL(certs []*x509.Certificate, roots *x509.CertPool, host string) error {
	if len(certs) == 0 {
		return errors.New("the server sent no certificate")
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	_, err := certs[0].Verify(x509.VerifyOptions{DNSName: host, Roots: roots, Intermediates: inter})
	if err == nil {
		return nil
	}
	var ua x509.UnknownAuthorityError
	if errors.As(err, &ua) {
		for _, c := range certs[1:] {
			if bytes.Equal(c.RawIssuer, c.RawSubject) && c.CheckSignatureFrom(c) == nil {
				return fmt.Errorf("SELF_SIGNED_CERT_IN_CHAIN (the served chain ends in %q, which the bundle does not hold)", c.Subject.CommonName)
			}
		}
		return fmt.Errorf("UNABLE_TO_GET_ISSUER_CERT_LOCALLY (%v)", err)
	}
	return err
}

// traceCredential picks the auth the test message carries, preferring
// exactly what Claude Code itself sent.
func (s *Server) traceCredential(d traceDeps) (http.Header, traceHop) {
	hop := traceHop{Key: "credential", Name: "Credential"}
	live, at := s.gateway.ClientCredential()
	age := time.Since(at).Round(time.Minute)
	if live != nil && time.Since(at) < liveCredentialFresh {
		hop.State, hop.Summary = hopOK, fmt.Sprintf("the credential Claude Code itself sent %s ago, forwarded the same way", ageText(age))
		return live, hop
	}
	if raw, err := d.keychainLogin(); err == nil {
		var login struct {
			OAuth struct {
				AccessToken string `json:"accessToken"`
				ExpiresAt   int64  `json:"expiresAt"`
			} `json:"claudeAiOauth"`
		}
		if json.Unmarshal([]byte(raw), &login) == nil && login.OAuth.AccessToken != "" {
			exp := time.UnixMilli(login.OAuth.ExpiresAt)
			if login.OAuth.ExpiresAt == 0 || time.Now().Before(exp) {
				h := http.Header{}
				h.Set("Authorization", "Bearer "+login.OAuth.AccessToken)
				h.Set("Anthropic-Beta", "oauth-2025-04-20")
				hop.State, hop.Summary = hopOK, "Claude Code's saved login, from the Keychain (Claude Code has not sent a request recently)"
				return h, hop
			}
			hop.Detail = fmt.Sprintf("Claude Code's saved login in the Keychain expired at %s; Claude Code refreshes it on its next request.", exp.Local().Format("15:04"))
		}
	}
	if live != nil {
		hop.State, hop.Summary = hopWarn, fmt.Sprintf("the credential Claude Code sent %s ago; it may have expired since", ageText(age))
		return live, hop
	}
	if k := d.getenv("ANTHROPIC_API_KEY"); k != "" {
		h := http.Header{}
		h.Set("X-Api-Key", k)
		hop.State, hop.Summary = hopOK, "ANTHROPIC_API_KEY from the gateway's environment"
		return h, hop
	}
	hop.State = hopBad
	hop.Summary = "no credential available, so no test message was sent"
	hop.Detail = strings.TrimSpace(hop.Detail + " Claude Code has not sent a request since the gateway started, no usable Claude Code login was found in the Keychain, and ANTHROPIC_API_KEY is unset. Use Claude Code once, then press Send test message again.")
	return nil, hop
}

func ageText(d time.Duration) string {
	if d < time.Minute {
		return "under a minute"
	}
	if d < time.Hour {
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
	return fmt.Sprintf("%.1f h", d.Hours())
}

type traceReply struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// sendTraced sends the message through the real path and fills hops d, e
// and f from the reply and the gateway's own record of it.
func (s *Server) sendTraced(ctx context.Context, d traceDeps, cfg config.Config, target *url.URL, dialAddr string, cred http.Header,
	route, up, fo *traceHop) {

	body, _ := json.Marshal(map[string]any{
		"model": traceModel, "max_tokens": traceMaxTokens, "system": traceSystem,
		"messages": []map[string]string{{"role": "user", "content": tracePrompt}},
	})
	tok := s.gateway.BeginTrace()

	send := func(insecure bool) (*http.Response, time.Duration, error) {
		tc := &tls.Config{ServerName: target.Hostname()}
		if insecure {
			tc.InsecureSkipVerify = true // #nosec G402 -- only after hop c already reported the trust failure, to test the rest of the path
		} else if pool, _, err := bundlePool(cfg.Intercept.CABundle); err == nil {
			tc.RootCAs = pool
		}
		tr := &http.Transport{
			TLSClientConfig: tc,
			// The address hop a resolved, so the request goes exactly where
			// Claude Code's would.
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return d.dial(ctx, network, dialAddr)
			},
		}
		defer tr.CloseIdleConnections()
		u := *target
		u.Path = strings.TrimRight(u.Path, "/") + "/v1/messages"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Anthropic-Version", "2023-06-01")
		req.Header.Set("User-Agent", "claude-burst-trace")
		for k, v := range cred {
			req.Header[k] = append([]string(nil), v...)
		}
		// No Origin, ever: the gateway refuses browser-originated requests.
		req.Header.Set(router.TraceHeader, tok)
		start := time.Now()
		resp, err := (&http.Client{Transport: tr, Timeout: 40 * time.Second}).Do(req)
		if err != nil {
			return nil, time.Since(start), err
		}
		// Read inside the transport's lifetime.
		b, rerr := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(b))
		return resp, time.Since(start), rerr
	}

	resp, took, err := send(false)
	insecureNote := ""
	var certErr *tls.CertificateVerificationError
	if err != nil && target.Scheme == "https" && errors.As(err, &certErr) {
		insecureNote = " Sent with certificate checks off, because the TLS hop above failed: this tests the rest of the path, which Claude Code cannot reach until that is fixed."
		resp, took, err = send(true)
	}

	tr, seen := s.gateway.EndTrace(tok)

	// e. upstream
	up.DurationMS = ms(took)
	if err != nil {
		up.State, up.Summary, up.Detail = hopBad, "the request failed before an answer: "+err.Error(), strings.TrimSpace(insecureNote)
	} else {
		raw, _ := io.ReadAll(resp.Body)
		var rep traceReply
		_ = json.Unmarshal(raw, &rep)
		rid := resp.Header.Get("Request-Id")
		if rid == "" {
			rid = rep.ID
		}
		if resp.StatusCode < 400 {
			up.State = hopOK
			model := rep.Model
			if model == "" {
				model = traceModel
			}
			up.Summary = fmt.Sprintf("HTTP %d in %d ms from %s, request id %s, replied %q", resp.StatusCode, up.DurationMS, model, orDash(rid), firstWords(rep.text(), 8))
		} else {
			up.State = hopBad
			msg := firstLine(string(raw), 300)
			if rep.Error != nil {
				msg = rep.Error.Type + ": " + rep.Error.Message
			}
			up.Summary = fmt.Sprintf("HTTP %d in %d ms, request id %s: %s", resp.StatusCode, up.DurationMS, orDash(rid), msg)
		}
		up.Detail = strings.TrimSpace(insecureNote)
	}

	// d. routing
	if !seen {
		route.State = hopBad
		route.Summary = "the request never reached the gateway"
		if err != nil {
			route.Detail = err.Error()
		}
		fo.State, fo.Summary = hopSkip, "not reached"
		if up.State == hopOK {
			// Answered, but not by way of the gateway: it went around it.
			up.State = hopWarn
			up.Summary += ", but NOT through Burst"
		}
		return
	}
	route.Summary = fmt.Sprintf("%s: %s (gateway request %s)", orDash(tr.Slot), orDash(tr.Reason), tr.RequestID)
	route.State = hopOK
	if tr.Slot == "secondary" {
		route.State = hopWarn
	}
	if len(tr.Hops) > 0 {
		h := tr.Hops[0]
		up.Detail = strings.TrimSpace(fmt.Sprintf("first hop: %s via %s to %s, HTTP %d in %d ms. %s", h.Slot, h.Route, h.Destination, h.Status, h.DurationMS, up.Detail))
	}

	// f. failover
	*fo = failoverHop(tr, s.gateway.HasSecondary())
}

// failoverHop reads the gateway's hops for what happened after the first.
func failoverHop(tr router.Trace, hasSecondary bool) traceHop {
	fo := traceHop{Key: "failover", Name: "Failover"}
	if len(tr.Hops) == 0 {
		fo.State, fo.Summary = hopSkip, "no upstream hop was recorded"
		return fo
	}
	first := tr.Hops[0]
	last := tr.Hops[len(tr.Hops)-1]
	if first.Slot == "secondary" {
		fo.State = hopSkip
		fo.Summary = "not needed: the request went to the secondary directly, because " + tr.Reason
		return fo
	}
	if len(tr.Hops) == 1 {
		if first.Status > 0 && first.Status < 400 {
			fo.State, fo.Summary = hopSkip, "not needed: the primary answered"
			return fo
		}
		if !hasSecondary {
			fo.State, fo.Summary = hopSkip, "skipped (optional): no secondary is set up, so the primary's answer went back as it was"
			return fo
		}
		fo.State, fo.Summary = hopWarn, "the secondary was not tried: this failure is not one that fails over"
		fo.Detail = first.Note
		return fo
	}
	steps := make([]string, 0, len(tr.Hops))
	for _, h := range tr.Hops {
		steps = append(steps, fmt.Sprintf("%s %s (%s) HTTP %d", h.Slot, h.Route, orDash(h.Model), h.Status))
	}
	switch {
	case last.Status > 0 && last.Status < 400 && last.Slot == "secondary":
		fo.State = hopWarn
		fo.Summary = "the primary failed and the secondary answered: " + strings.Join(steps, ", then ")
	case last.Status > 0 && last.Status < 400:
		fo.State = hopWarn
		fo.Summary = "the primary refused this model and a fallback model answered: " + strings.Join(steps, ", then ")
	default:
		fo.State = hopBad
		fo.Summary = "failover was tried and failed too: " + strings.Join(steps, ", then ")
	}
	fo.Detail = first.Note
	return fo
}

func (r traceReply) text() string {
	var b strings.Builder
	for _, c := range r.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

func firstWords(s string, n int) string {
	f := strings.Fields(s)
	if len(f) > n {
		return strings.Join(f[:n], " ") + " ..."
	}
	return strings.Join(f, " ")
}

func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		s = s[:max] + " ..."
	}
	return s
}

// upstreamPath runs mtr from this Mac to Anthropic's REAL address, resolved
// the way the gateway's own upstream dials resolve it (bypassing the
// /etc/hosts redirect, which would otherwise point mtr at 127.0.0.1).
func (s *Server) upstreamPath(ctx context.Context, d traceDeps, cfg config.Config) traceLeg {
	host, addrs, ok, err := s.gateway.ResolveUpstream(ctx)
	if !ok {
		u, perr := url.Parse(cfg.Primary.BaseURL)
		if perr != nil || u.Hostname() == "" {
			return traceLeg{Name: "gateway to Anthropic", Note: "no upstream host to trace"}
		}
		host = u.Hostname()
		lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		addrs, err = d.lookupHost(lctx, host)
		cancel()
	}
	leg := traceLeg{Name: "gateway to " + host}
	if err != nil || len(addrs) == 0 {
		leg.Note = fmt.Sprintf("could not resolve %s's real address: %v", host, err)
		return leg
	}
	ip := addrs[0]
	leg.Name = fmt.Sprintf("gateway to %s (%s)", host, ip)
	mtr := d.findMtr()
	if mtr == "" {
		leg.Note = "mtr not available: install it with `brew install mtr` to see the network path here"
		return leg
	}
	mctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, errOut, rerr := d.run(mctx, nil, mtr, "--report", "--report-cycles", "3", "--json", "-n", ip)
	hops, perr := parseMtrJSON(out)
	if perr != nil && !mtrDenied(errOut) && mctx.Err() == nil {
		// An mtr too old for --json: the plain report instead.
		out, errOut, rerr = d.run(mctx, nil, mtr, "-n", "-c", "3", "--report", ip)
		hops, perr = parseMtrText(out)
	}
	switch {
	case mtrDenied(errOut):
		leg.Note = "mtr not permitted: on macOS its helper mtr-packet needs root. Either run `sudo chown root " + mtrPacket(mtr) +
			" && sudo chmod u+s " + mtrPacket(mtr) + "` once, or run `sudo " + mtr + " -n " + ip + "` yourself"
	case mctx.Err() != nil:
		leg.Note = "mtr did not finish within 15s"
	case perr != nil || len(hops) == 0:
		msg := firstLine(string(errOut), 200)
		if msg == "" && rerr != nil {
			msg = rerr.Error()
		}
		leg.Note = "mtr ran but its report could not be read: " + orDash(msg)
	default:
		leg.Hops = hops
		leg.Note = fmt.Sprintf("mtr, 3 cycles, %d hops", len(hops))
	}
	return leg
}

func mtrPacket(mtr string) string {
	if i := strings.LastIndex(mtr, "/"); i >= 0 {
		return mtr[:i+1] + "mtr-packet"
	}
	return "mtr-packet"
}

func mtrDenied(stderr []byte) bool {
	e := strings.ToLower(string(stderr))
	return strings.Contains(e, "operation not permitted") || strings.Contains(e, "permission denied") ||
		strings.Contains(e, "failure to open") || strings.Contains(e, "must be root")
}

func parseMtrJSON(b []byte) ([]netHop, error) {
	var r struct {
		Report struct {
			Hubs []struct {
				Host string  `json:"host"`
				Loss float64 `json:"Loss%"`
				Avg  float64 `json:"Avg"`
				Wrst float64 `json:"Wrst"`
			} `json:"hubs"`
		} `json:"report"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(b), &r); err != nil {
		return nil, err
	}
	if len(r.Report.Hubs) == 0 {
		return nil, errors.New("no hops in mtr's report")
	}
	out := make([]netHop, 0, len(r.Report.Hubs))
	for _, h := range r.Report.Hubs {
		out = append(out, netHop{Host: h.Host, Loss: h.Loss, Avg: h.Avg, Worst: h.Wrst})
	}
	return out, nil
}

// "  1.|-- 192.168.1.1   0.0%     3    2.1   2.3   2.0   2.6   0.3"
var mtrLine = regexp.MustCompile(`^\s*\d+\.\|--\s+(\S+)\s+([\d.]+)%?\s+\d+\s+[\d.]+\s+([\d.]+)\s+[\d.]+\s+([\d.]+)`)

func parseMtrText(b []byte) ([]netHop, error) {
	var out []netHop
	for _, line := range strings.Split(string(b), "\n") {
		m := mtrLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		loss, _ := strconv.ParseFloat(m[2], 64)
		avg, _ := strconv.ParseFloat(m[3], 64)
		worst, _ := strconv.ParseFloat(m[4], 64)
		out = append(out, netHop{Host: m[1], Loss: loss, Avg: avg, Worst: worst})
	}
	if len(out) == 0 {
		return nil, errors.New("no hops in mtr's report")
	}
	return out, nil
}

// traceVerdict is the plain-English line at the top: the first broken hop
// is the answer, since everything after it is a consequence.
func traceVerdict(hops []traceHop) (string, string) {
	for _, h := range hops {
		if h.State == hopBad {
			return hopBad, "Broken at " + h.Name + ": " + h.Summary + "."
		}
	}
	var warns []string
	for _, h := range hops {
		if h.State == hopWarn {
			warns = append(warns, h.Name+": "+h.Summary)
		}
	}
	if len(warns) > 0 {
		return hopWarn, "Works, with something to look at. " + strings.Join(warns, ". ") + "."
	}
	var up traceHop
	for _, h := range hops {
		if h.Key == "upstream" {
			up = h
		}
	}
	return hopOK, "Claude Code's path works end to end: the test message went through Burst and was answered in " +
		strconv.FormatInt(up.DurationMS, 10) + " ms."
}

// traceMu keeps two presses from running two traces at once.
var traceMu sync.Mutex

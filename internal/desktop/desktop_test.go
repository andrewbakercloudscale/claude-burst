package desktop

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The User-Agents are the ones Claude Code 2.1.325 builds (FW() in its
// bundle, read 17 Nov 2026): the entrypoint is the second item in the
// parentheses, and Desktop's Code tab and Cowork add the SDK's items.
func TestClientOfNamesEachFrontEnd(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"claude-cli/2.1.325 (external, cli)":                                                    ClientCLI,
		"claude-cli/2.1.325 (external, claude-desktop, agent-sdk/0.2.1, client-app/claude-app)": ClientDesktop,
		"claude-cli/2.1.325 (external, claude-desktop-3p)":                                      ClientDesktop,
		"claude-cli/2.1.325 (external, local-agent, agent-sdk/0.2.1)":                           ClientCowork,
		"claude-cli/2.1.325 (external, sdk-cli)":                                                ClientSDK,
		"claude-cli/2.1.325 (external, claude-vscode)":                                          ClientVSCode,
		"claude-cli/2.1.325":                              ClientCLI,
		"claude-cli/2.1.325 (external, ../../etc/passwd)": "other",
		"claude-cli/2.1.325 (external, remote_mobile)":    "remote_mobile",
		"Mozilla/5.0 (Macintosh)":                         "",
		"":                                                "",
	}
	for ua, want := range cases {
		if got := ClientOf(ua); got != want {
			t.Errorf("ClientOf(%q) = %q, want %q", ua, got, want)
		}
	}
	if !IsDesktop(ClientDesktop) || !IsDesktop(ClientCowork) || IsDesktop(ClientCLI) {
		t.Error("IsDesktop")
	}
}

func testCA(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeDesktop lays out a Claude Desktop install as found on 17 Nov 2026,
// in a temporary directory: nothing on the real Mac is read.
func fakeDesktop(t *testing.T) Paths {
	t.Helper()
	root := t.TempDir()
	p := Paths{App: filepath.Join(root, "Applications", "Claude.app"),
		Support: filepath.Join(root, "Library", "Application Support", "Claude"), Projects: filepath.Join(root, ".claude", "projects")}
	write(t, filepath.Join(p.App, "Contents", "Info.plist"),
		"<plist><dict>\n    <key>CFBundleShortVersionString</key>\n    <string>5.11492.5</string>\n</dict></plist>\n")
	for _, v := range []string{"2.1.325", "2.1.9", "2.1.321"} {
		write(t, filepath.Join(p.Support, "claude-code", v, "485fdac4332e", "claude.app", "Contents", "MacOS", "claude"), "#!/bin/sh\n")
	}
	// A version folder whose download never finished holds no binary.
	if err := os.MkdirAll(filepath.Join(p.Support, "claude-code", "2.1.400"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInspectReadsDesktopAndItsCABundle(t *testing.T) {
	t.Parallel()
	p := fakeDesktop(t)
	burst := testCA(t, "claude-burst local CA")
	other := testCA(t, "Some Root")

	// No bundle yet: Desktop writes it at launch.
	st := Inspect(p, burst, time.Now().Add(-time.Hour))
	if !st.Installed || st.Version != "5.11492.5" || st.CABundle != "" {
		t.Fatalf("status %+v", st)
	}
	if got := strings.Join(st.CodeVersions, " "); got != "2.1.9 2.1.321 2.1.325" {
		t.Errorf("code versions %q", got)
	}
	if ok, msg := st.Advice("transparent"); !ok || !strings.Contains(msg, "next start") {
		t.Errorf("no bundle: %v %q", ok, msg)
	}

	// A bundle written before the CA was trusted system-wide: Desktop's
	// sessions refuse the gateway until it restarts.
	write(t, p.CABundle(), string(other))
	st = Inspect(p, burst, time.Now().Add(-time.Hour))
	ok, msg := st.Advice("transparent")
	if !st.CAChecked || st.HasCA || ok || !strings.Contains(msg, "trust-ca-systemwide.sh") || !strings.Contains(msg, "reopen Claude Desktop") {
		t.Fatalf("stale bundle: %+v %v %q", st, ok, msg)
	}

	// Rewritten with it (and re-wrapped, as Desktop's own writer does).
	rewrapped := strings.ReplaceAll(string(burst), "\n", "\r\n")
	write(t, p.CABundle(), string(other)+rewrapped)
	st = Inspect(p, burst, time.Now().Add(-time.Hour))
	if ok, _ := st.Advice("transparent"); !st.HasCA || !ok {
		t.Fatalf("bundle with the CA: %+v", st)
	}

	// base-url mode has no CA to compare: the Code tab follows settings.json.
	st = Inspect(p, nil, time.Now())
	if ok, msg := st.Advice("base-url"); st.CAChecked || !ok || !strings.Contains(msg, "settings.json") {
		t.Errorf("base-url: %+v %q", st, msg)
	}
}

func TestInspectWithoutDesktop(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	st := Inspect(Paths{App: filepath.Join(root, "nope.app"), Support: filepath.Join(root, "s"), Projects: filepath.Join(root, "p")}, nil, time.Now())
	if st.Installed || st.Sessions != 0 || len(st.CodeVersions) != 0 {
		t.Fatalf("%+v", st)
	}
	if ok, msg := st.Advice("transparent"); !ok || msg != "Claude Desktop is not installed" {
		t.Errorf("%v %q", ok, msg)
	}
}

// Code-tab transcripts are told apart by their entrypoint, and only recent
// ones are counted.
func TestInspectCountsDesktopTranscripts(t *testing.T) {
	t.Parallel()
	p := fakeDesktop(t)
	meta := `{"type":"custom-title","sessionId":"x"}` + "\n"
	write(t, filepath.Join(p.Projects, "-a", "d1.jsonl"), meta+`{"type":"user","entrypoint":"claude-desktop","cwd":"/x"}`+"\n")
	write(t, filepath.Join(p.Projects, "-a", "c1.jsonl"), meta+`{"type":"user","entrypoint":"cli","cwd":"/x"}`+"\n")
	write(t, filepath.Join(p.Projects, "-b", "d2.jsonl"), `{"type":"user","entrypoint":"claude-desktop"}`+"\n")
	old := filepath.Join(p.Projects, "-b", "d3.jsonl")
	write(t, old, `{"type":"user","entrypoint":"claude-desktop"}`+"\n")
	long := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(old, long, long); err != nil {
		t.Fatal(err)
	}
	st := Inspect(p, nil, time.Now().Add(-7*24*time.Hour))
	if st.Sessions != 2 || st.LastSession.IsZero() {
		t.Fatalf("sessions %d last %v", st.Sessions, st.LastSession)
	}
	if got := TranscriptEntrypoint(filepath.Join(p.Projects, "-a", "c1.jsonl")); got != "cli" {
		t.Errorf("entrypoint %q", got)
	}
}

func TestBundleHasComparesCertificatesNotText(t *testing.T) {
	t.Parallel()
	a, b := testCA(t, "a"), testCA(t, "b")
	if BundleHas(b, a) || !BundleHas(append(append([]byte("# comment\n"), b...), a...), a) || BundleHas(nil, a) || BundleHas(a, nil) {
		t.Error("BundleHas")
	}
}

func writeMetrics(t *testing.T, path string, lines ...string) {
	t.Helper()
	write(t, path, strings.Join(lines, "\n")+"\n")
}

// Check says whether Desktop's sessions reach Burst, and only blames Burst
// for missing ones once its records name clients at all.
func TestCheckSaysWhetherDesktopReachesBurst(t *testing.T) {
	t.Parallel()
	p := fakeDesktop(t)
	ca := testCA(t, "claude-burst local CA")
	write(t, p.CABundle(), string(ca))
	now := time.Now()
	recent := now.Add(-time.Hour).UTC().Format(time.RFC3339)
	old := now.Add(-10 * 24 * time.Hour).UTC().Format(time.RFC3339)
	mpath := filepath.Join(t.TempDir(), "metrics.jsonl")

	// No Desktop transcripts and no records: nothing to say against it.
	writeMetrics(t, mpath, `{"time":"`+recent+`","route":"anthropic"}`)
	if inst, ok, msg := Check(p, ca, mpath, "transparent", now); !inst || !ok || !strings.Contains(msg, "No Claude Desktop requests") {
		t.Fatalf("idle: %v %v %q", inst, ok, msg)
	}

	// Code sessions ran, but the records predate the client field: Burst
	// cannot tell, so it does not fail.
	write(t, filepath.Join(p.Projects, "-a", "d1.jsonl"), `{"type":"user","entrypoint":"claude-desktop"}`+"\n")
	if _, ok, _ := Check(p, ca, mpath, "transparent", now); !ok {
		t.Fatal("untagged records were taken as Desktop missing")
	}

	// Records name their clients, only the terminal's arrived: Desktop's
	// sessions went around Burst.
	writeMetrics(t, mpath, `{"time":"`+recent+`","route":"anthropic","client":"cli"}`, `{"time":"`+old+`","client":"claude-desktop"}`)
	if _, ok, msg := Check(p, ca, mpath, "transparent", now); ok || !strings.Contains(msg, "none of their requests reached Burst") {
		t.Fatalf("missing Desktop: %v %q", ok, msg)
	}

	// They arrived.
	writeMetrics(t, mpath, `{"time":"`+recent+`","client":"claude-desktop"}`, `{"time":"`+recent+`","client":"cowork"}`, "not json")
	if _, ok, msg := Check(p, ca, mpath, "transparent", now); !ok || !strings.HasPrefix(msg, "2 requests from Claude Desktop") {
		t.Fatalf("seen: %v %q", ok, msg)
	}

	// The CA missing from Desktop's bundle outranks everything else.
	write(t, p.CABundle(), string(testCA(t, "other")))
	if _, ok, msg := Check(p, ca, mpath, "transparent", now); ok || !strings.Contains(msg, "do not trust Burst") {
		t.Fatalf("untrusted: %v %q", ok, msg)
	}
}

// A metrics.jsonl past the read limit is read from its tail, and the line
// the seek cut in half is skipped rather than miscounted.
func TestReadSeenReadsTheTail(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "metrics.jsonl")
	now := time.Now().UTC().Format(time.RFC3339)
	line := `{"time":"` + now + `","client":"cli","note":"` + strings.Repeat("x", 1000) + `"}` + "\n"
	var b strings.Builder
	for b.Len() < seenTail+5000 {
		b.WriteString(line)
	}
	b.WriteString(`{"time":"` + now + `","client":"claude-desktop"}` + "\n")
	write(t, path, b.String())
	seen := ReadSeen(path, time.Now().Add(-time.Hour))
	if seen.ByClient[ClientDesktop] != 1 || !seen.Tagged || seen.ByClient[ClientCLI] == 0 || seen.ByClient[ClientCLI] > seenTail/len(line)+1 {
		t.Fatalf("%+v", seen)
	}
}

func TestDefaultPathsHonourTheApplicationsOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(ApplicationsDirEnv, dir)
	if got := DefaultPaths("/Users/x").App; got != filepath.Join(dir, "Claude.app") && got != "/Users/x/Applications/Claude.app" {
		t.Fatalf("App = %q", got)
	}
}

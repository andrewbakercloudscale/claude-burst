// Package desktop knows what Claude Desktop (/Applications/Claude.app) does
// with Claude Code, so Burst can tell whether its sessions go through the
// gateway.
//
// What was found on 17 Nov 2026 (Desktop 5.11492.5, bundled Claude Code
// 2.1.325), by reading Desktop's own log and code on a Mac running Burst:
//
//   - The Code tab runs its own Claude Code, not the one on PATH:
//     ~/Library/Application Support/Claude/claude-code/<version>/<hash>/
//     claude.app/Contents/MacOS/claude. Its transcripts land in
//     ~/.claude/projects like the terminal's, with "entrypoint":
//     "claude-desktop", and its requests say so in the User-Agent:
//     "claude-cli/2.1.325 (external, claude-desktop, ...)". It reads
//     ~/.claude/settings.json, so base-url mode reaches it.
//   - Cowork (Desktop's "local agent" sessions) runs the same binary with
//     CLAUDE_CONFIG_DIR set to a temporary directory and its own
//     ANTHROPIC_BASE_URL, so ~/.claude/settings.json is never read there:
//     only transparent mode reaches it. Its User-Agent says "local-agent".
//   - Desktop does not pass the shell's NODE_EXTRA_CA_CERTS on. It writes
//     its own bundle, ~/Library/Application Support/Claude/ca-bundle.pem,
//     from the CAs the System keychain trusts plus the NODE_EXTRA_CA_CERTS
//     Desktop itself was started with (launchd's, not the shell's), and
//     points every Claude Code it starts at that. So in transparent mode
//     Desktop's sessions trust the gateway only when the CA is trusted
//     system-wide (scripts/trust-ca-systemwide.sh), and only from the next
//     time Desktop starts: the bundle is written at launch.
//
// Nothing here starts, stops or changes Claude Desktop; it only reads.
package desktop

import (
	"bufio"
	"bytes"
	"encoding/pem"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Clients, as ClientOf names them.
const (
	ClientCLI     = "cli"            // the terminal's claude
	ClientDesktop = "claude-desktop" // Claude Desktop's Code tab
	ClientCowork  = "cowork"         // Claude Desktop's Cowork (local agent)
	ClientSDK     = "sdk"            // the Agent SDK, headless runs
	ClientVSCode  = "vscode"         // the VS Code extension
)

// ClientOf names the Claude Code front end a request came from, from its
// User-Agent: "claude-cli/<version> (external, <entrypoint>, ...)". It is
// "" for anything that is not Claude Code (a browser, curl, Codex). The
// entrypoint is the sender's own word, so only a short plain token is ever
// returned: it goes into metrics.jsonl and the log.
func ClientOf(ua string) string {
	if !strings.HasPrefix(ua, "claude-cli/") {
		return ""
	}
	open := strings.IndexByte(ua, '(')
	if open < 0 {
		return ClientCLI
	}
	inner := strings.TrimSuffix(ua[open+1:], ")")
	parts := strings.Split(inner, ",")
	if len(parts) < 2 {
		return ClientCLI
	}
	ep := strings.TrimSpace(parts[1])
	switch ep {
	case "cli":
		return ClientCLI
	case "claude-desktop", "claude-desktop-3p":
		return ClientDesktop
	case "local-agent", "local_agent":
		return ClientCowork
	case "sdk-cli", "sdk-ts", "sdk-py":
		return ClientSDK
	case "claude-vscode":
		return ClientVSCode
	}
	if !plainToken(ep) {
		return "other"
	}
	return ep
}

func plainToken(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '-' && c != '_' && (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

// IsDesktop reports whether client is one of Claude Desktop's.
func IsDesktop(client string) bool { return client == ClientDesktop || client == ClientCowork }

// Paths are where Claude Desktop and its sessions keep things. Tests point
// them at a temporary directory.
type Paths struct {
	App      string // Claude.app
	Support  string // ~/Library/Application Support/Claude
	Projects string // ~/.claude/projects, where Code-tab transcripts land
}

// DefaultPaths are this Mac's: Claude.app in /Applications, or in
// ~/Applications when it was dragged there instead.
func DefaultPaths(home string) Paths {
	app := "/Applications/Claude.app"
	if _, err := os.Stat(filepath.Join(app, "Contents", "Info.plist")); err != nil {
		if alt := filepath.Join(home, "Applications", "Claude.app"); exists(filepath.Join(alt, "Contents", "Info.plist")) {
			app = alt
		}
	}
	projects := filepath.Join(home, ".claude", "projects")
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		projects = filepath.Join(d, "projects")
	}
	return Paths{
		App:      app,
		Support:  filepath.Join(home, "Library", "Application Support", "Claude"),
		Projects: projects,
	}
}

// ScratchRoot is where Desktop makes a folder for a session started
// without one ("scratch-2026-11-17-cae9f8"). None is a repository.
func (p Paths) ScratchRoot() string { return filepath.Join(p.Support, "scratch-workspaces") }

// CABundle is the bundle Desktop hands every Claude Code it starts.
func (p Paths) CABundle() string { return filepath.Join(p.Support, "ca-bundle.pem") }

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// Status is what Inspect found.
type Status struct {
	Installed bool   `json:"installed"`
	App       string `json:"app,omitempty"`
	Version   string `json:"version,omitempty"`
	// CodeVersions are the Claude Code versions Desktop has downloaded for
	// its Code tab, oldest first.
	CodeVersions []string `json:"code_versions,omitempty"`
	// CABundle is Desktop's own bundle; CABundleAt is when Desktop last
	// wrote it, which is when it last started.
	CABundle   string    `json:"ca_bundle,omitempty"`
	CABundleAt time.Time `json:"ca_bundle_at,omitempty"`
	// HasCA: the bundle holds Burst's CA. Only meaningful when CAChecked
	// (transparent mode, and the bundle could be read).
	CAChecked bool `json:"ca_checked"`
	HasCA     bool `json:"has_ca"`
	// Sessions are Code-tab transcripts written in the window Inspect was
	// given, and LastSession the newest of them.
	Sessions    int       `json:"sessions"`
	LastSession time.Time `json:"last_session,omitempty"`
}

var versionRe = regexp.MustCompile(`<key>CFBundleShortVersionString</key>\s*<string>([^<]{1,40})</string>`)

// Inspect reads what Claude Desktop has on disk. caPEM is Burst's CA in
// transparent mode (nil otherwise): Inspect then says whether Desktop's
// bundle holds it. Transcripts modified since since are counted.
func Inspect(p Paths, caPEM []byte, since time.Time) Status {
	st := Status{}
	if b, err := os.ReadFile(filepath.Join(p.App, "Contents", "Info.plist")); err == nil {
		st.Installed, st.App = true, p.App
		if m := versionRe.FindSubmatch(b); m != nil {
			st.Version = string(m[1])
		}
	}
	st.CodeVersions = codeVersions(filepath.Join(p.Support, "claude-code"))
	if fi, err := os.Stat(p.CABundle()); err == nil {
		st.CABundle, st.CABundleAt = p.CABundle(), fi.ModTime()
		if len(caPEM) > 0 {
			if b, err := os.ReadFile(p.CABundle()); err == nil {
				st.CAChecked = true
				st.HasCA = BundleHas(b, caPEM)
			}
		}
	}
	st.Sessions, st.LastSession = desktopTranscripts(p.Projects, since)
	return st
}

// codeVersions lists <dir>/<version> holding a bundled Claude Code.
func codeVersions(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		bins, _ := filepath.Glob(filepath.Join(dir, e.Name(), "*", "claude.app", "Contents", "MacOS", "claude"))
		if len(bins) > 0 {
			out = append(out, e.Name())
		}
	}
	sort.Slice(out, func(i, j int) bool { return versionLess(out[i], out[j]) })
	return out
}

func versionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if as[i] == bs[i] {
			continue
		}
		ai, aerr := atoi(as[i])
		bi, berr := atoi(bs[i])
		if aerr || berr {
			return as[i] < bs[i]
		}
		return ai < bi
	}
	return len(as) < len(bs)
}

func atoi(s string) (int, bool) {
	n := 0
	if s == "" {
		return 0, true
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, true
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, false
}

// BundleHas reports whether the PEM bundle holds the certificate in caPEM,
// compared as DER, so line wrapping and comments do not matter.
func BundleHas(bundle, caPEM []byte) bool {
	want, _ := pem.Decode(caPEM)
	if want == nil {
		return false
	}
	for rest := bundle; ; {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			return false
		}
		if blk.Type == "CERTIFICATE" && bytes.Equal(blk.Bytes, want.Bytes) {
			return true
		}
	}
}

// desktopTranscripts counts transcripts under projects modified since
// since whose entrypoint is Claude Desktop's.
func desktopTranscripts(projects string, since time.Time) (n int, last time.Time) {
	dirs, err := os.ReadDir(projects)
	if err != nil {
		return 0, time.Time{}
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(projects, d.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			fi, err := f.Info()
			if err != nil || fi.ModTime().Before(since) {
				continue
			}
			if TranscriptEntrypoint(filepath.Join(projects, d.Name(), f.Name())) == "claude-desktop" {
				n++
				if fi.ModTime().After(last) {
					last = fi.ModTime()
				}
			}
		}
	}
	return n, last
}

// TranscriptEntrypoint is the first "entrypoint" a transcript records
// ("cli", "claude-desktop", "sdk-cli"), or "". It is on the first message
// line, after a few metadata lines, so the scan is short.
func TranscriptEntrypoint(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for i := 0; i < 200 && sc.Scan(); i++ {
		line := sc.Bytes()
		j := bytes.Index(line, []byte(`"entrypoint":"`))
		if j < 0 {
			continue
		}
		rest := line[j+len(`"entrypoint":"`):]
		if k := bytes.IndexByte(rest, '"'); k > 0 && k <= 32 {
			return string(rest[:k])
		}
	}
	return ""
}

// Advice says what keeps Desktop's sessions out of Burst, if anything, for
// the dashboard's check and diagnose. mode is the intercept mode.
func (s Status) Advice(mode string) (ok bool, msg string) {
	if !s.Installed {
		return true, "Claude Desktop is not installed"
	}
	if mode != "transparent" {
		return true, "Claude Desktop's Code tab reads ~/.claude/settings.json, so it goes through Burst; Cowork sessions do not (they ignore settings.json: use transparent mode for them)"
	}
	if s.CABundle == "" {
		return true, "Claude Desktop has not written its CA bundle yet: it does on its next start"
	}
	if !s.CAChecked {
		return true, "Burst's CA could not be compared with Claude Desktop's bundle"
	}
	if !s.HasCA {
		return false, "Claude Desktop's sessions do not trust Burst: its CA bundle (" + s.CABundle + ", written " +
			s.CABundleAt.Local().Format("2 Jan 15:04") + ") lacks Burst's CA. Trust the CA system-wide (sudo scripts/trust-ca-systemwide.sh), then quit and reopen Claude Desktop"
	}
	return true, "Claude Desktop's CA bundle holds Burst's CA, so its Code and Cowork sessions trust the gateway"
}

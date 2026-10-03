package admin

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/repo"
)

// stubPermissions replaces every outside call the permissions handlers
// make: security and codesign answer as given, open is recorded, and the
// folders are never read unless a test supplies readDir.
func stubPermissions(t *testing.T, codesignOut string, identity bool, readDir func(string) ([]os.DirEntry, error)) *[]string {
	t.Helper()
	var calls []string
	oldExec, oldRead, oldExe := permExec, permReadDir, executablePath
	t.Cleanup(func() { permExec, permReadDir, executablePath = oldExec, oldRead, oldExe })
	repo.ResetAccess()
	t.Cleanup(repo.ResetAccess)
	executablePath = func() (string, error) { return "/fake/bin/claude-burst", nil }
	permExec = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		switch name {
		case "security":
			if identity {
				return []byte(`  1) ABC "` + signingName + `"` + "\n     1 valid identities found\n"), nil
			}
			return []byte("     0 valid identities found\n"), nil
		case "codesign":
			return []byte(codesignOut), nil
		}
		return nil, nil
	}
	if readDir == nil {
		readDir = func(p string) ([]os.DirEntry, error) {
			t.Errorf("read %s: only Check may look inside the folders", p)
			return nil, nil
		}
	}
	permReadDir = readDir
	return &calls
}

func getPermissions(t *testing.T, s *Server) permissionsResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/permissions", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/api/permissions status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got permissionsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// A build signed with the local certificate is ok, and the GET looks at
// neither Desktop, Documents nor Downloads: looking is what asks.
func TestPermissionsSignedAndNeverLooks(t *testing.T) {
	s := newTestServer(t)
	calls := stubPermissions(t, "Identifier="+signingIdentifier+"\nAuthority="+signingName+"\n", true, nil)
	got := getPermissions(t, s)
	sg := got.Signing
	if sg.State != "ok" || !sg.Signed || !sg.IdentityReady || sg.Identifier != signingIdentifier {
		t.Errorf("signing: %+v", sg)
	}
	if len(*calls) != 2 || !strings.HasPrefix((*calls)[0], "security find-identity") || (*calls)[1] != "codesign -dvv /fake/bin/claude-burst" {
		t.Errorf("calls: %q", *calls)
	}
	if len(got.Folders) != 3 {
		t.Fatalf("folders: %+v", got.Folders)
	}
	for _, f := range got.Folders {
		if f.State != repo.NotSeen || f.Fix == "" {
			t.Errorf("%s: %+v", f.Folder, f)
		}
	}
}

// An ad hoc build says why the prompt keeps coming back and how to stop it.
func TestPermissionsAdHocSaysSetUpSigning(t *testing.T) {
	s := newTestServer(t)
	stubPermissions(t, "Identifier=a.out\nSignature=adhoc\n", false, nil)
	sg := getPermissions(t, s).Signing
	if sg.State != "warn" || sg.Signed || sg.IdentityReady || sg.Authority != "ad hoc" {
		t.Errorf("signing: %+v", sg)
	}
	if !strings.Contains(sg.Fix, "Set up signing") {
		t.Errorf("fix: %q", sg.Fix)
	}
}

// Check reads each folder, reports what macOS answered, and feeds the
// answers into what the GET shows afterwards.
func TestPermissionsCheckRecordsAnswers(t *testing.T) {
	s := newTestServer(t)
	home := os.Getenv("HOME")
	var read []string
	stubPermissions(t, "Signature=adhoc\n", false, func(p string) ([]os.DirEntry, error) {
		read = append(read, p)
		switch filepath.Base(p) {
		case "Documents":
			return nil, &fs.PathError{Op: "open", Path: p, Err: syscall.EPERM}
		case "Downloads":
			return nil, &fs.PathError{Op: "open", Path: p, Err: syscall.ENOENT}
		}
		return nil, nil
	})
	rr := mutate(t, s, "/api/permissions-check", "{}")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Results []struct{ Folder, State string }
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"Desktop": repo.Allowed, "Documents": repo.Denied, "Downloads": "missing"}
	for _, r := range resp.Results {
		if want[r.Folder] != r.State {
			t.Errorf("%s: %q, want %q", r.Folder, r.State, want[r.Folder])
		}
	}
	if len(read) != 3 || read[0] != filepath.Join(home, "Desktop") {
		t.Errorf("read: %q", read)
	}
	permReadDir = func(p string) ([]os.DirEntry, error) { t.Errorf("GET read %s", p); return nil, nil }
	for _, f := range getPermissions(t, s).Folders {
		if f.Folder == "Documents" && (f.State != repo.Denied || !strings.Contains(f.Fix, "Files and Folders")) {
			t.Errorf("Documents after check: %+v", f)
		}
		if f.Folder == "Desktop" && f.State != repo.Allowed {
			t.Errorf("Desktop after check: %+v", f)
		}
	}
}

// Set up signing refuses without the script and never opens Terminal then;
// with it, Terminal runs a script that runs signing-setup.sh.
func TestSigningSetupOpensTerminal(t *testing.T) {
	s := newTestServer(t)
	stubPermissions(t, "", false, nil)
	var opened []string
	old := launchTerminal
	t.Cleanup(func() { launchTerminal = old })
	launchTerminal = func(p string) error { opened = append(opened, p); return nil }

	scripts := t.TempDir()
	s.rootHelper = filepath.Join(scripts, "transparent-root.sh")
	if rr := mutate(t, s, "/api/signing-setup", "{}"); rr.Code != http.StatusConflict || len(opened) != 0 {
		t.Fatalf("no script: status=%d opened=%q", rr.Code, opened)
	}

	setup := filepath.Join(scripts, "signing-setup.sh")
	if err := os.WriteFile(setup, []byte("#!/bin/zsh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	rr := mutate(t, s, "/api/signing-setup", "{}")
	if rr.Code != http.StatusOK || len(opened) != 1 {
		t.Fatalf("status=%d opened=%q body=%s", rr.Code, opened, rr.Body.String())
	}
	body, err := os.ReadFile(opened[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "zsh "+shellQuote(setup)) {
		t.Errorf("script does not run signing-setup.sh:\n%s", body)
	}
}

// Open Privacy settings opens Files and Folders, through the stub.
func TestOpenPrivacySettings(t *testing.T) {
	s := newTestServer(t)
	calls := stubPermissions(t, "", false, nil)
	if rr := mutate(t, s, "/api/open-privacy-settings", "{}"); rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if len(*calls) != 1 || (*calls)[0] != "open "+privacySettingsURL {
		t.Errorf("calls: %q", *calls)
	}
}

// Check also runs git in a checkout under Desktop, the way the update check
// does: macOS asks about git on its own, and an unanswered prompt shows as
// waiting rather than a GitHub failure.
func TestPermissionsCheckAsksForGit(t *testing.T) {
	s := newTestServer(t)
	stubPermissions(t, "Signature=adhoc\n", false, func(string) ([]os.DirEntry, error) { return nil, nil })
	checkout := filepath.Join(os.Getenv("HOME"), "Desktop", "claude-burst")
	if err := os.MkdirAll(filepath.Join(checkout, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.rootHelper = filepath.Join(checkout, "scripts", "transparent-root.sh")
	oldG, oldWait := runGit, permCheckWait
	t.Cleanup(func() { runGit, permCheckWait = oldG, oldWait })
	permCheckWait = 50 * time.Millisecond
	var ran []string
	runGit = func(ctx context.Context, dir string, args ...string) (string, error) {
		ran = append(ran, dir+" "+strings.Join(args, " "))
		<-ctx.Done() // macOS is showing its prompt
		return "", ctx.Err()
	}
	rr := mutate(t, s, "/api/permissions-check", "{}")
	var resp struct {
		Results []struct{ Folder, State, Detail string }
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	last := resp.Results[len(resp.Results)-1]
	if last.Folder != "git in Desktop" || last.State != "waiting" || !strings.Contains(last.Detail, "git may read Desktop") {
		t.Errorf("git result: %+v", last)
	}
	if len(ran) != 1 || ran[0] != checkout+" rev-parse --git-dir" {
		t.Errorf("git ran: %q", ran)
	}
}

// A checkout in no protected folder has nothing for macOS to ask about.
func TestPermissionsCheckSkipsGitOutsideProtectedFolders(t *testing.T) {
	s := newTestServer(t)
	stubPermissions(t, "Signature=adhoc\n", false, func(string) ([]os.DirEntry, error) { return nil, nil })
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "scripts"), 0o755)
	s.rootHelper = filepath.Join(dir, "scripts", "transparent-root.sh")
	oldG := runGit
	t.Cleanup(func() { runGit = oldG })
	runGit = func(ctx context.Context, dir string, args ...string) (string, error) {
		t.Errorf("git ran: %v", args)
		return "", nil
	}
	rr := mutate(t, s, "/api/permissions-check", "{}")
	if strings.Contains(rr.Body.String(), "git in") {
		t.Errorf("body: %s", rr.Body.String())
	}
}

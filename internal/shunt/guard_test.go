package shunt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLines(t *testing.T, dir, name string, n int) string {
	t.Helper()
	p := filepath.Join(dir, name)
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteString("line of code\n")
	}
	if err := os.WriteFile(p, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func readIn(path string) HookInput {
	var in HookInput
	in.ToolName = "Read"
	in.ToolInput.FilePath = path
	return in
}

func bashIn(cmd, cwd string) HookInput {
	var in HookInput
	in.ToolName = "Bash"
	in.Cwd = cwd
	in.ToolInput.Command = cmd
	return in
}

var opt = GuardOptions{Read: true, MinLines: 350}

func TestReadThreshold(t *testing.T) {
	dir := t.TempDir()
	big := writeLines(t, dir, "big.go", 400)
	edge := writeLines(t, dir, "edge.go", 350)
	under := writeLines(t, dir, "under.go", 349)

	if d := Decide(readIn(big), opt); !d.Deny || d.Lines < 350 {
		t.Errorf("400-line file should be denied, got %+v", d)
	}
	if d := Decide(readIn(edge), opt); !d.Deny {
		t.Errorf("a file at exactly the threshold is delegated")
	}
	if d := Decide(readIn(under), opt); d.Deny {
		t.Errorf("349 lines is under the threshold")
	}
}

func TestReadOffMeansAllow(t *testing.T) {
	big := writeLines(t, t.TempDir(), "big.go", 1000)
	if d := Decide(readIn(big), GuardOptions{Read: false, MinLines: 350}); d.Deny {
		t.Errorf("read switched off must never deny")
	}
}

func TestWindowedReadAlwaysPasses(t *testing.T) {
	big := writeLines(t, t.TempDir(), "big.go", 1000)
	one := 1.0
	in := readIn(big)
	in.ToolInput.Limit = &one
	if d := Decide(in, opt); d.Deny {
		t.Errorf("a windowed read is the escape hatch and must pass")
	}
	in = readIn(big)
	in.ToolInput.Offset = &one
	if d := Decide(in, opt); d.Deny {
		t.Errorf("an offset read must pass")
	}
}

func TestDenyMessageNamesTheWayOut(t *testing.T) {
	big := writeLines(t, t.TempDir(), "my file.go", 500)
	d := Decide(readIn(big), GuardOptions{Read: true, MinLines: 350, Bin: "/opt/bin/claude-burst"})
	for _, want := range []string{"/opt/bin/claude-burst shunt read", "--question", "offset and limit", "'" + big + "'"} {
		if !strings.Contains(d.Reason, want) {
			t.Errorf("denial message missing %q:\n%s", want, d.Reason)
		}
	}
}

func TestExemptions(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".claude", "skills"), 0o755)
	cases := map[string]string{
		"claude config":   writeLines(t, filepath.Join(dir, ".claude", "skills"), "SKILL.md", 900),
		"dotenv":          writeLines(t, dir, ".env", 900),
		"pem":             writeLines(t, dir, "server.pem", 900),
		"missing":         filepath.Join(dir, "nope.go"),
		"directory":       dir,
		"empty path":      "",
		"relative no cwd": "big.go",
	}
	for name, p := range cases {
		if d := Decide(readIn(p), opt); d.Deny {
			t.Errorf("%s: must not be denied, got %+v", name, d)
		}
	}
}

func TestBinaryFileIsLeftAlone(t *testing.T) {
	p := filepath.Join(t.TempDir(), "blob.bin")
	b := append([]byte{0, 1, 2}, []byte(strings.Repeat("x\n", 2000))...)
	os.WriteFile(p, b, 0o644)
	if d := Decide(readIn(p), opt); d.Deny {
		t.Errorf("binary files go through Claude Code's own reader")
	}
}

func TestUnterminatedLastLineCounts(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.txt")
	os.WriteFile(p, []byte(strings.Repeat("a\n", 349)+"last"), 0o644)
	if d := Decide(readIn(p), opt); !d.Deny {
		t.Errorf("349 newlines plus an unterminated line is 350 lines")
	}
}

func TestBashForms(t *testing.T) {
	dir := t.TempDir()
	big := writeLines(t, dir, "big.go", 800)
	writeLines(t, dir, "small.go", 20)

	deny := []string{
		"cat big.go",
		"cat -n big.go",
		"less big.go",
		"more big.go",
		"cat 'big.go'",
		"head -n 500 big.go",
		"head -500 big.go",
		"tail -n +1 big.go",
		"cat small.go big.go",
		"cat " + big,
	}
	for _, c := range deny {
		if d := Decide(bashIn(c, dir), opt); !d.Deny {
			t.Errorf("should deny %q", c)
		}
	}
	allow := []string{
		"cat small.go",
		"head big.go",             // default 10 lines
		"head -n 20 big.go",       // deliberate window
		"tail -20 big.go",         // deliberate window
		"tail -f big.go",          // follow is not a read
		"cat big.go | grep foo",   // only filtered output reaches the model
		"cat big.go > /tmp/x",     // redirect
		"cat big.go && echo done", // chain
		"cat $HOME/big.go",        // substitution
		"cat *.go",                // glob: unknowable
		"ls big.go",
		"grep -c foo big.go",
		"cat",
		"",
	}
	for _, c := range allow {
		if d := Decide(bashIn(c, dir), opt); d.Deny {
			t.Errorf("should allow %q, denied: %s", c, d.Path)
		}
	}
}

func TestOtherToolsIgnored(t *testing.T) {
	var in HookInput
	in.ToolName = "Edit"
	in.ToolInput.FilePath = writeLines(t, t.TempDir(), "big.go", 900)
	if d := Decide(in, opt); d.Deny {
		t.Errorf("only Read and Bash are guarded")
	}
}

func TestIsSensitive(t *testing.T) {
	yes := []string{".env", "/a/b/.env.production", "id_rsa", "id_ed25519.pub", "/h/.ssh/config", "cert.pem", "x.key", "terraform.tfstate", ".npmrc", "/h/.aws/credentials", "credentials.json", "prod.tfvars"}
	no := []string{".env.example", ".env.sample", "main.go", "keyboard.go", "monkey.txt", "environment.md", "README.md", "id.go"}
	for _, p := range yes {
		if !IsSensitive(p) {
			t.Errorf("%s should be sensitive", p)
		}
	}
	for _, p := range no {
		if IsSensitive(p) {
			t.Errorf("%s should not be sensitive", p)
		}
	}
}

// The refusal is all a blocked session has to go on, and a real one retried the
// same cat four times in two minutes instead of following it. It has to say
// not to, and offer the windowed way out that is never blocked.
func TestDenyMessageForbidsRetryAndOffersAWindow(t *testing.T) {
	big := writeLines(t, t.TempDir(), "big.go", 500)
	d := Decide(bashIn("cat "+big, ""), GuardOptions{Read: true, MinLines: 350, Bin: "/opt/bin/claude-burst"})
	if !d.Deny {
		t.Fatal("expected a denial")
	}
	for _, want := range []string{"Do NOT retry", "shunt read --question", "offset and limit", "sed -n 'START,ENDp'"} {
		if !strings.Contains(d.Reason, want) {
			t.Errorf("message missing %q:\n%s", want, d.Reason)
		}
	}
	// the sed window it recommends must itself pass the guard
	if got := Decide(bashIn("sed -n '1,120p' "+big, ""), opt); got.Deny {
		t.Errorf("the escape hatch the message offers must not be blocked")
	}
}

package admin

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The dashboard's Share button sends the same one line the README tells
// people to paste: a stale copy would install from a path that no longer
// exists, and nobody sharing it would notice.
func TestShareSendsTheReadmeInstallLine(t *testing.T) {
	page := string(indexHTML)
	for _, id := range []string{`id="shareTop"`, `id="shareDialog"`, `id="shareCopy"`, `id="shareNative"`, `id="shareWa"`, `id="shareMail"`} {
		if !strings.Contains(page, id) {
			t.Errorf("admin.html lacks %s", id)
		}
	}
	m := regexp.MustCompile(`const SHARE_LINE = "([^"]+)";`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("admin.html has no SHARE_LINE")
	}
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "> "+m[1]+"\n") {
		t.Errorf("the shared install line %q is not the README's", m[1])
	}
}

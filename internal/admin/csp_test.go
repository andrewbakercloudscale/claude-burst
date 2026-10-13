package admin

import (
	"regexp"
	"strings"
	"testing"
)

// The page is served with a policy that names its own inline scripts and no
// others. The hashes come from the embedded file, so this checks there is
// one per inline script and that nothing loosens script-src.
func TestIndexIsServedWithAScriptPolicy(t *testing.T) {
	n := len(inlineScript.FindAll(indexHTML, -1))
	if n == 0 {
		t.Fatal("found no inline script in the page: the pattern no longer matches it")
	}
	if got := strings.Count(indexCSP, "'sha256-"); got != n {
		t.Errorf("policy has %d script hashes, the page has %d inline scripts", got, n)
	}
	script := indexCSP[strings.Index(indexCSP, "script-src"):]
	script = script[:strings.Index(script, ";")]
	for _, loose := range []string{"unsafe-inline", "unsafe-eval", "*", "http:", "https:"} {
		if strings.Contains(script, loose) {
			t.Errorf("script-src allows %s: %s", loose, script)
		}
	}
	if regexp.MustCompile(`(?i)<script\s[^>]*>\s*[^<\s]`).Match(indexHTML) {
		t.Error("a script tag with attributes has an inline body, which the hash pattern does not cover")
	}
}

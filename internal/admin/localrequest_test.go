package admin

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

// localRequest is httptest.NewRequest as the dashboard's own page sends it:
// from this Mac. httptest's default caller is 192.0.2.1, which the guard
// refuses, as it must.
func localRequest(method, target string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	r.RemoteAddr = "127.0.0.1:50000"
	return r
}

// The dashboard has no login because only this Mac can reach it. That has
// to hold whatever address it listens on: a caller from another machine is
// refused even when it names a loopback Host and sends the mutation header.
func TestTheGuardRefusesCallersThatAreNotThisMac(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for addr, want := range map[string]int{
		"127.0.0.1:5000": http.StatusNoContent, "[::1]:5000": http.StatusNoContent,
		"192.168.1.20:5000": http.StatusForbidden, "10.0.0.7:5000": http.StatusForbidden,
		"[fe80::1]:5000": http.StatusForbidden, "": http.StatusForbidden,
	} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:7788/api/restart", nil)
		r.RemoteAddr = addr
		r.Header.Set(mutationHeader, "1")
		w := httptest.NewRecorder()
		guard("", ok).ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("caller %q: status %d, want %d", addr, w.Code, want)
		}
	}
}

// Another site's page cannot call the API, with or without the Host it
// wants: the browser names where the request came from. The page itself,
// curl and the mod are unaffected, and a link to the dashboard still opens.
func TestTheGuardRefusesAnotherSitesPage(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, c := range []struct {
		path, origin, site string
		want               int
	}{
		{"/api/state", "", "", http.StatusNoContent},
		{"/api/state", "http://127.0.0.1:7788", "same-origin", http.StatusNoContent},
		{"/api/state", "", "none", http.StatusNoContent},
		{"/api/test-connection", "", "cross-site", http.StatusForbidden},
		{"/api/test-connection", "http://evil.example", "", http.StatusForbidden},
		{"/api/state", "http://127.0.0.1:3000", "same-site", http.StatusForbidden},
		{"/api/state", "null", "", http.StatusForbidden},
		{"/", "", "cross-site", http.StatusNoContent},
	} {
		r := localRequest("GET", "http://127.0.0.1:7788"+c.path, nil)
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if c.site != "" {
			r.Header.Set("Sec-Fetch-Site", c.site)
		}
		w := httptest.NewRecorder()
		guard("", ok).ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%+v: status %d", c, w.Code)
		}
	}
}

// Showing the stored key needs Touch ID. Pointing the secondary at another
// host with the key field left blank would send that same key there, so it
// needs Touch ID too; the same host, or a key typed in again, does not.
func TestMovingAStoredKeyToANewHostNeedsTouchID(t *testing.T) {
	s, home, stored := newSecondaryTestServer(t)
	writeConfig(t, home)
	asked := ""
	s.authenticate = func(reason string) error { asked = reason; return errors.New("User canceled authentication") }
	post := func(base, key string) int {
		asked = ""
		return postSecondary(t, s, `{"provider":"openai-compatible","base_url":"`+base+`","model":"glm-4.7","keychain_service":"claude-burst-zai","api_key":"`+key+`"}`).Code
	}
	if code := post("https://api.z.ai/v4", "sk-first"); code != http.StatusOK || asked != "" {
		t.Fatalf("a first save with its key: status %d, asked %q", code, asked)
	}
	if stored["claude-burst-zai"] != "sk-first" {
		t.Fatal("key not stored")
	}
	if code := post("https://api.z.ai/v5", ""); code != http.StatusOK || asked != "" {
		t.Fatalf("the same host: status %d, asked %q", code, asked)
	}
	if code := post("https://evil.example/v4", ""); code != http.StatusForbidden || !strings.Contains(asked, "evil.example") {
		t.Fatalf("a new host with the stored key: status %d, asked %q", code, asked)
	}
	if got := loadSavedConfig(t, home).Secondary.BaseURL; got != "https://api.z.ai/v5" {
		t.Fatalf("a refused change was saved: %q", got)
	}
	if code := post("https://other.example/v4", "sk-second"); code != http.StatusOK || asked != "" {
		t.Fatalf("a new host with its own key typed in: status %d, asked %q", code, asked)
	}
}

// Every API route answers a POST without the mutation header with a
// refusal: 403 from mutating, or 405 from readOnly. A route registered with
// neither would let any page on this Mac's browser change things with a
// plain form post. The routes are read from the source, so a new one is
// covered the day it is added.
func TestNoRouteAcceptsAPostWithoutTheMutationHeader(t *testing.T) {
	s, _, _ := newSecondaryTestServer(t)
	c, _, _ := newTestConsole(t)
	route := regexp.MustCompile(`mux\.HandleFunc\("(/api/[^"]+)"`)
	for file, h := range map[string]http.Handler{"admin.go": s.Handler(), "console.go": c.Handler()} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		paths := route.FindAllStringSubmatch(string(src), -1)
		if len(paths) < 5 {
			t.Fatalf("%s: found %d routes, the pattern no longer matches how they are registered", file, len(paths))
		}
		for _, m := range paths {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, localRequest("POST", "http://127.0.0.1:7788"+m[1], strings.NewReader("{}")))
			if w.Code != http.StatusForbidden && w.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: a POST without the header got %d, want 403 or 405", file, m[1], w.Code)
			}
		}
	}
}

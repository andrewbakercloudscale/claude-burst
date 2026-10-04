package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInspectEndpointsBeforeAnyRequest(t *testing.T) {
	s := newTestServer(t)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/inspect", nil))
	if rr.Code != http.StatusOK || strings.TrimSpace(rr.Body.String()) != "[]" {
		t.Fatalf("list = %d %q", rr.Code, rr.Body)
	}
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/inspect?session=nope", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown session = %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/inspect-item?session=nope&i=0", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown item = %d", rr.Code)
	}
}

// Remove takes only an item the session's latest request shows as
// removable: anything else is refused, and nothing is stored.
func TestInspectRemoveRefusesUnknownItems(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	for body, want := range map[string]int{
		`{"session":"S"}`:                                http.StatusBadRequest,
		`{"engine":"claude","session":"S","id":"ab12"}`: http.StatusNotFound,
		`{"engine":"codex","session":"S","id":"ab12"}`:  http.StatusConflict, // no Codex gateway in this server
	} {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/inspect/remove", strings.NewReader(body))
		req.Header.Set(mutationHeader, "1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("%s: %d %s, want %d", body, rec.Code, rec.Body, want)
		}
	}
	if got := s.gateway.Removals().For("claude:S"); len(got) != 0 {
		t.Errorf("stored %v", got)
	}
}

package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Only an audited HANDOFF.md that exists can be shown in Finder, so the
// endpoint cannot be used to open or reveal any other path.
func TestHandoverRevealOnlyAuditedFiles(t *testing.T) {
	s := newTestServer(t)
	var opened []string
	old := revealFile
	revealFile = func(p string) error { opened = append(opened, p); return nil }
	t.Cleanup(func() { revealFile = old })
	for _, body := range []string{`{"root":"/etc"}`, `{"root":""}`, `nope`} {
		rec := httptest.NewRecorder()
		req := localRequest(http.MethodPost, "http://127.0.0.1:7788/api/handover-reveal", strings.NewReader(body))
		req.Header.Set("X-Claude-Burst-Admin", "1")
		s.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Errorf("%s: status 200, want a refusal", body)
		}
	}
	if len(opened) != 0 {
		t.Fatalf("opened %v", opened)
	}
}

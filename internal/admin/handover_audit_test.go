package admin

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The file endpoint serves audited handover files only, and delete-all
// refuses a request that does not spell out the confirmation.
func TestHandoverAuditEndpointsAreNarrow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	secret := filepath.Join(home, "secret.txt")
	if err := os.WriteFile(secret, []byte("do not serve"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{}

	for _, root := range []string{home, "/etc", "../.."} {
		w := httptest.NewRecorder()
		s.handleHandoverFile(w, localRequest(http.MethodGet, "/api/handover-file?root="+root, nil))
		if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "do not serve") {
			t.Fatalf("root %q: want 404, got %d %s", root, w.Code, w.Body)
		}
	}

	for _, body := range []string{``, `{}`, `{"confirm":"yes"}`} {
		w := httptest.NewRecorder()
		s.handleHandoverDelete(w, localRequest(http.MethodPost, "/api/handover-delete", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body %q: want 400, got %d", body, w.Code)
		}
	}

	w := httptest.NewRecorder()
	s.handleHandoverAudit(w, localRequest(http.MethodGet, "/api/handover-audit", nil))
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("no log yet: want an empty list, got %d %s", w.Code, w.Body)
	}
}

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

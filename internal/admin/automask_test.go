package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

func TestAutomaskSwitchesAreSavedAndShown(t *testing.T) {
	s := newTestServer(t)
	writeConfig(t, os.Getenv("HOME"))

	get := func() automaskStatus {
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, localRequest(http.MethodGet, "http://127.0.0.1/api/automask", nil))
		var st automaskStatus
		if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
			t.Fatalf("%d %s", rr.Code, rr.Body)
		}
		return st
	}
	st := get()
	if st.Enabled || len(st.Rules) < 13 || !st.Rules[0].On || st.Rules[0].ID != "privatekey" {
		t.Fatalf("defaults = %+v", st)
	}

	rr := mutate(t, s, "/api/automask-save", `{"enabled":true,"rules":{"card":true,"email":true,"said":false}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rr.Code, rr.Body)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	// Only the rules away from their default are stored.
	if !cfg.Automask.Enabled || len(cfg.Automask.Rules) != 2 || !cfg.Automask.Rules["email"] || cfg.Automask.Rules["said"] {
		t.Fatalf("saved %+v", cfg.Automask)
	}
	on := map[string]bool{}
	for _, r := range get().Rules {
		on[r.ID] = r.On
	}
	if !on["card"] || !on["email"] || on["said"] || on["phone"] {
		t.Fatalf("shown %+v", on)
	}

	if rr := mutate(t, s, "/api/automask-save", `{"enabled":true,"rules":{"nope":true}}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown rule accepted: %d", rr.Code)
	}
}

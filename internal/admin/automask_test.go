package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

	// The word list is cleaned and kept by a save that does not name it.
	mutate(t, s, "/api/automask-save", `{"enabled":true,"words":[" Bluebird ","bluebird","ab","acme"]}`)
	mutate(t, s, "/api/automask-save", `{"enabled":true}`)
	if st := get(); len(st.Words) != 2 || st.Words[0] != "Bluebird" || st.Words[1] != "acme" || st.Recent == nil {
		t.Fatalf("words = %q, recent = %v", st.Words, st.Recent)
	}
	mutate(t, s, "/api/automask-save", `{"enabled":true,"words":[]}`)
	if st := get(); len(st.Words) != 0 {
		t.Fatalf("words not cleared: %q", st.Words)
	}

	// Where it applies: everywhere until some are named, kept by a save
	// that does not say, and naming all of them is everywhere again.
	if st := get(); len(st.Providers) != 0 || len(st.Choices) != 3 || st.Choices[2].ID != "chatgpt" {
		t.Fatalf("providers = %+v choices = %+v", st.Providers, st.Choices)
	}
	mutate(t, s, "/api/automask-save", `{"enabled":true,"providers":["chatgpt","secondary"]}`)
	mutate(t, s, "/api/automask-save", `{"enabled":true}`)
	if st := get(); strings.Join(st.Providers, ",") != "secondary,chatgpt" {
		t.Fatalf("providers = %+v", st.Providers)
	}
	if !s.gateway.AutomaskCovers("chatgpt") || s.gateway.AutomaskCovers("anthropic") {
		t.Error("the running gateway must follow the save")
	}
	mutate(t, s, "/api/automask-save", `{"enabled":true,"providers":["chatgpt","secondary","anthropic"]}`)
	if st := get(); len(st.Providers) != 0 {
		t.Fatalf("all of them is everywhere: %+v", st.Providers)
	}
	if rr := mutate(t, s, "/api/automask-save", `{"enabled":true,"providers":["openai"]}`); rr.Code != http.StatusBadRequest {
		t.Errorf("unknown provider: %d", rr.Code)
	}
	if rr := mutate(t, s, "/api/automask-save", `{"enabled":true,"rules":{"nope":true}}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown rule accepted: %d", rr.Code)
	}
}

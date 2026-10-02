package admin

import (
	"errors"
	"net/http"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// Every dashboard handler that changes config.json goes through updateConfig,
// so its load, change and save happen under config.Update's lock. Before
// this (found by the 2026-10-02 review) each handler loaded, changed and
// saved on its own, and two saves a moment apart, or a save racing the CLI,
// silently dropped the earlier one.

// httpError is how a change function inside updateConfig refuses a request
// with a specific status. Returning it aborts the update: nothing is saved.
type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

func badRequest(err error) error { return &httpError{http.StatusBadRequest, err.Error()} }

func serverError(msg string) error { return &httpError{http.StatusInternalServerError, msg} }

// errNothingToChange aborts an update that turned out to change nothing; the
// client is told so and config.json is not rewritten.
var errNothingToChange = errors.New("nothing to change")

// updateConfig applies fn to the latest config.json under the config lock
// and saves it. On success it returns the config as saved. On any failure it
// has already answered the request and returns ok=false; the caller just
// returns.
func updateConfig(w http.ResponseWriter, fn func(*config.Config) error) (saved config.Config, ok bool) {
	err := config.Update(func(c *config.Config) error {
		if err := fn(c); err != nil {
			return err
		}
		saved = *c
		return nil
	})
	var he *httpError
	switch {
	case err == nil:
		return saved, true
	case errors.Is(err, errNothingToChange):
		writeJSON(w, map[string]string{"ok": "nothing to change"})
	case config.IsUnreadable(err):
		http.Error(w, "config.json does not parse, fix it before changing this: "+err.Error(), http.StatusInternalServerError)
	case errors.As(err, &he):
		http.Error(w, he.msg, he.code)
	default:
		http.Error(w, "saving config.json: "+err.Error(), http.StatusInternalServerError)
	}
	return config.Config{}, false
}

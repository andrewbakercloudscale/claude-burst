package router

import (
	"encoding/json"
	"testing"
)

// The part of a cache write that went to the one-hour cache is read apart:
// it costs more than the five-minute one.
func TestOneHourCacheWritesAreReadFromUsage(t *testing.T) {
	var u map[string]any
	if err := json.Unmarshal([]byte(`{"cache_read_input_tokens":7498,"cache_creation_input_tokens":248,"cache_creation":{"ephemeral_5m_input_tokens":48,"ephemeral_1h_input_tokens":200}}`), &u); err != nil {
		t.Fatal(err)
	}
	var tok tokenUsage
	readCacheUsage(u, &tok)
	// A later block with nothing in it changes nothing.
	readCacheUsage(map[string]any{"cache_creation": map[string]any{}}, &tok)
	if tok.cacheRead != 7498 || tok.cacheWrite != 248 || tok.cacheWrite1h != 200 {
		t.Fatalf("%+v", tok)
	}
}

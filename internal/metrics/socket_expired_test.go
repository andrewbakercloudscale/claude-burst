package metrics

import "testing"

// ChatGPT ending an idle Codex WebSocket at 60 minutes is not a failed turn;
// any other 400 still is.
func TestSocketExpiryIsNoError(t *testing.T) {
	expired := Event{HTTPStatus: 400, Note: `{"type":"invalid_request_error","code":"websocket_connection_limit_reached"}`}
	if IsError(expired) {
		t.Error("an expired socket counted as an error")
	}
	if !IsError(Event{HTTPStatus: 400, Note: "bad request"}) {
		t.Error("a real 400 not counted")
	}
}

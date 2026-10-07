package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// threadContinues is the message id a request continues, "" when it sends
// its own history.
func threadContinues(top map[string]json.RawMessage) string {
	var th struct {
		Type string `json:"type"`
		Prev string `json:"previous_message_id"`
	}
	if json.Unmarshal(top["thread"], &th) != nil || th.Type != "continue" {
		return ""
	}
	return th.Prev
}

// maxThreads is how many responses are remembered for the requests that
// continue them: a session continues its latest one.
const maxThreads = 4096

// threadResponse is what is kept of a response for the request that
// continues it: the conversation it belongs to, the context it reported,
// and the summary its history was sent with ("" for none; the summary's
// hash otherwise). A thread goes on from the history the API holds, so a
// thread that began before a summary was swapped in still carries
// everything the summary replaced.
//
// removedMsgs and removedBytes are what that summary took out of the request
// it went into. Each request that continues the thread goes without them
// too, and is logged so: with nothing logged, the request after a swap read
// as the summary dropped, every compaction of a thread as lasting one turn,
// and on 6 Oct 2026 a repository's learned Compact at went back to the fixed
// one because "sessions go on 1 turns after a compaction".
type threadResponse struct {
	key          string
	context      int64
	summary      string
	removedMsgs  int
	removedBytes int64
}

// noteThread remembers the response msgID. Caller holds c.mu.
func (c *compactor) noteThread(msgID string, r threadResponse) {
	if msgID == "" {
		return
	}
	if c.threads == nil {
		c.threads = map[string]threadResponse{}
	}
	if _, known := c.threads[msgID]; !known {
		c.threadIDs = append(c.threadIDs, msgID)
		if len(c.threadIDs) > maxThreads {
			delete(c.threads, c.threadIDs[0])
			c.threadIDs = c.threadIDs[1:]
		}
	}
	c.threads[msgID] = r
}

// threadAsk is the latest time a session and model were asked for their
// history (askForHistory).
type threadAsk struct {
	at   time.Time
	prev string // the response the refused request continued
	// orphan is the thread's conversation when it was known only by its
	// last response (after a restart), "" otherwise: the history that comes
	// back is that conversation under its real name, and this one is gone.
	orphan string
	// context is what the thread held, for the history that comes back: it
	// can arrive under a conversation nobody has measured (after a restart
	// the thread is known only by its last response).
	context int64
	// deaf: the same request came again instead of the history, so this
	// client does not replay and is not asked again.
	deaf bool
}

// The least time between two requests for a session's history: to start a
// summary, and to swap in one that is ready. Each is one request refused
// and one sent whole from cache, so neither is dear, but a history that
// comes back and still cannot be compacted must not be asked for on every
// tool call.
const (
	askToStartGap = 10 * time.Minute
	askToSwapGap  = 30 * time.Second
)

// applyThreadRequest passes a request that continues a message thread
// through as it is, under the conversation whose response it continues, so
// the context its response reports is that conversation's. A response from
// before a restart is not known: the thread is then a conversation of its
// own, named by that response, until its history is next sent whole. When
// that conversation has a compaction to make, the request is marked to be
// answered with a request for the history instead (compactInfo.replay).
func (s *Server) applyThreadRequest(in *http.Request, body []byte, msgs []json.RawMessage, cfg config.CompactionConfig, prefix, prev string) ([]byte, *http.Request) {
	now := time.Now()
	// Only where the request would go to Anthropic: the secondary has no
	// thread to lose, and takes a request that continues one as it comes.
	primary := !s.forcedOverflow(now) && !s.modelInOverflow(requestModel(body), now)
	sid := in.Header.Get("x-claude-code-session-id")
	wanted := s.inspect.wanted(sid)
	s.compaction.mu.Lock()
	from := s.compaction.threads[prev]
	key := from.key
	if key == "" || !strings.HasPrefix(key, prefix) {
		key, from = prefix+"thread-"+prev[max(0, len(prev)-12):], threadResponse{}
	}
	st := s.compaction.state(key)
	st.seen = now
	st.thread = true
	// The thread's own size: Claude Code's side requests send the same
	// conversation whole, compacted, and their responses report that size.
	// On 6 Oct 2026 a panel read "115k, compacted" over a thread at 408k.
	if from.context > 0 {
		st.lastContext = from.context
	}
	ci := compactInfo{key: key, thread: true, threadSummary: from.summary, limit: cfg.CompactAtTokens,
		inForce: st.summary != "" && st.swapAt > 0}
	if from.summary != "" {
		ci.removedMsgs, ci.removedBytes = from.removedMsgs, from.removedBytes
	}
	behind := from.key != "" && st.summary != "" && st.swapAt > 0 && from.summary != st.hash
	ask := s.compaction.asks[prefix]
	window := time.Duration(cfg.WindowMinutes) * time.Minute
	gap, why := time.Duration(0), ""
	switch {
	case !primary, ask != nil && ask.deaf, st.pending:
	case ask != nil && ask.prev == prev:
		ask.deaf = true
		s.logger.Printf("req=%s compaction cannot ask for the history session=%s: the request refused %s ago came again as it was, so this client does not send its conversation whole when a thread is gone; its threads are left alone until the gateway restarts",
			requestIDFrom(in.Context()), key, now.Sub(ask.at).Round(time.Second))
	case behind:
		// The summary went into a request that was not this thread's (a
		// side request sent whole takes it first), or into one the thread
		// did not go on from.
		gap, why = askToSwapGap, "its summary is in force, and this thread began before it"
	case st.next != "" && (endsInPrompt(msgs) || cfg.MidTurn && !s.compaction.midTurnOff):
		gap, why = askToSwapGap, "its summary is ready to swap in"
	case st.next == "" && st.lastContext >= cfg.CompactAtTokens && (st.startedAt.IsZero() || now.Sub(st.startedAt) >= window):
		gap, why = askToStartGap, fmt.Sprintf("it holds %dk, over its limit of %dk", st.lastContext/1000, cfg.CompactAtTokens/1000)
	case wanted != "":
		// /burst-prune and /burst-dump: what was removed is still in the
		// history the API holds, and the inspector has only what was last
		// sent whole.
		gap, why = askToSwapGap, wanted
	}
	if why != "" && (ask == nil || now.Sub(ask.at) >= gap) {
		if s.compaction.asks == nil {
			s.compaction.asks = map[string]*threadAsk{}
		}
		s.compaction.asks[prefix] = &threadAsk{at: now, prev: prev, context: st.lastContext}
		if strings.HasPrefix(key, prefix+"thread-") {
			s.compaction.asks[prefix].orphan = key
		}
		ci.replay = why
		// Whatever the reason, the history that comes back serves the
		// inspector too.
		s.inspect.asked(sid)
		if behind {
			// What comes back is sent with the summary: its size is not
			// known until the response, and the thread's is no longer it.
			s.compaction.asks[prefix].context, st.lastContext = 0, 0
		}
	}
	s.compaction.mu.Unlock()
	return body, in.WithContext(context.WithValue(in.Context(), compactInfoKey{}, ci))
}

// askForHistory answers a request that continues a thread the way the API
// does when the thread's state has expired: 404 thread_not_found, to which
// Claude Code sends the conversation whole, starting a new thread. Nothing
// goes upstream, so it is no failure of Anthropic's and counts as none.
func (s *Server) askForHistory(w http.ResponseWriter, in *http.Request) {
	ci := compactInfoFrom(in.Context())
	rid := requestIDFrom(in.Context())
	s.logger.Printf("req=%s compaction asked for the history session=%s: %s, and a request that continues a thread holds nothing to compact; answered 404 thread_not_found, as the API does for a thread that has expired, so Claude Code sends the conversation whole",
		rid, ci.key, ci.replay)
	id := "req_burst_" + rid
	type detail struct {
		Code string `json:"error_code"`
	}
	type apiError struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Details detail `json:"details"`
	}
	b, _ := json.Marshal(struct {
		Type      string   `json:"type"`
		Error     apiError `json:"error"`
		RequestID string   `json:"request_id"`
	}{"error", apiError{"not_found_error", "No thread state was found for the requested `previous_message_id`. Replay the full conversation with `thread: {\"type\": \"create\"}` to start a new Thread.", detail{"thread_not_found"}}, id})
	w.Header().Set("content-type", "application/json")
	w.Header().Set("request-id", id)
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write(b)
}

// sideRequestMarkers identify requests Claude Code makes on its own, beside
// the conversation, with the conversation's history: the away recap is sent
// minutes after a turn ends, when nobody is there. Taken as a plain prompt,
// on 2026-10-01 it swapped a summary in with no hook to show it (the notice
// waited 80 minutes and a restart lost it), and later dropped a waiting
// summary because it alters the last message.
var sideRequestMarkers = [][]byte{
	[]byte("The user stepped away and is coming back. Recap in"),
}

// isSideRequest reports whether the last message is one of Claude Code's
// own side requests rather than a prompt the user sent.
func isSideRequest(msgs []json.RawMessage) bool {
	last := msgs[len(msgs)-1]
	for _, m := range sideRequestMarkers {
		if bytes.Contains(last, m) {
			return true
		}
	}
	return false
}

// applySideRequest sends a side request with the summary already in force,
// when it still fits, and changes nothing: no swap, no drop, no start, and
// no context noted (its compactInfo has no key), because the conversation
// did not move.
func (s *Server) applySideRequest(in *http.Request, top map[string]json.RawMessage, body []byte, msgs []json.RawMessage, key string) ([]byte, *http.Request) {
	s.compaction.mu.Lock()
	st := s.compaction.sessions[key]
	if st == nil || st.summary == "" {
		// Read only: a side request is never what a conversation is known by.
		prefix := key[:strings.LastIndex(key, "|")+1]
		for k, d := range s.compaction.sessions {
			if strings.HasPrefix(k, prefix) && d.summary != "" && d.fits(msgs) {
				st = d
				break
			}
		}
	}
	var summary string
	var p0, swapAt int
	if st != nil && st.summary != "" && st.swapAt > 0 && len(msgs) > st.p0 && st.hashOf(msgs, st.p0) == st.hash {
		summary, p0, swapAt = st.summary, st.p0, st.swapAt
	}
	s.compaction.mu.Unlock()
	if summary == "" {
		return body, in
	}
	out := rewriteWithSummary(msgs, summary, p0, swapAt)
	nb, err := json.Marshal(out)
	if err != nil {
		return body, in
	}
	top["messages"] = nb
	newBody, err := json.Marshal(top)
	if err != nil {
		return body, in
	}
	ci := compactInfo{applied: true, removedMsgs: len(msgs) - len(out), removedBytes: int64(len(body) - len(newBody))}
	return newBody, in.WithContext(context.WithValue(in.Context(), compactInfoKey{}, ci))
}

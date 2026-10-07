package router

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Compaction before the cache goes cold.
//
// Claude Code on a subscription writes its conversation to the one-hour
// cache. A session left alone for an hour comes back to nothing cached, and
// its next request writes the whole context again at the write price, forty
// times a read on Opus 5.5. In the 14 days to 7 Oct 2026 that happened 24
// times on this Mac, 4.1M tokens, about $33, and Compact at had no say in
// it: most of those sessions were under it.
//
// So a session that has been idle for idleCompactAfter is summarised while
// its cache can still be read, from the last request it sent. The summary
// waits, and the request that comes back carries it: what is written then
// is what a compaction leaves, not the history. If nobody comes back the
// summary call was the whole cost, a cached read and the summary's output.
//
// Only where the one-hour cache is in use (the request says so), only at
// FloorTokens or more, and only between idleCompactAfter and
// idleCompactUntil by the wall clock: a Mac that slept through the hour
// wakes to a cold cache, and a summary then would be the full write this
// is here to avoid.
const (
	idleCompactAfter = 50 * time.Minute
	idleCompactUntil = 58 * time.Minute
)

// longCacheMark is how a request asks for the one-hour cache.
var longCacheMark = []byte(`"ttl":"1h"`)

// idleRequest is a session's latest request, kept so a summary can be
// written from it when nothing follows.
type idleRequest struct {
	in    *http.Request
	top   map[string]json.RawMessage
	msgs  []json.RawMessage
	floor int64
	limit string
	at    time.Time // when it came: its cache entry lasts an hour from here
}

// keepForIdle remembers a request for compactIdle, or forgets the one held
// when this request cannot be summarised that way. Caller holds the lock.
func (st *compactState) keepForIdle(in *http.Request, top map[string]json.RawMessage, body []byte, msgs []json.RawMessage, floor int64, limit string) {
	st.idle = nil
	if !bytes.Contains(body, longCacheMark) {
		return
	}
	own := map[string]json.RawMessage{}
	for _, k := range []string{"model", "system", "tools", "thinking", "metadata"} {
		if v, ok := top[k]; ok {
			own[k] = v
		}
	}
	st.idle = &idleRequest{in: in.Clone(context.Background()), top: own, msgs: msgs, floor: floor, limit: limit, at: time.Now()}
}

// compactIdle starts a summary for each session that has been idle long
// enough and is worth it, and returns how many it started. now is the wall
// clock.
func (s *Server) compactIdle(now time.Time) int {
	now = now.Round(0)
	s.compaction.mu.Lock()
	defer s.compaction.mu.Unlock()
	cfg := s.compaction.cfg
	started := 0
	for key, st := range s.compaction.sessions {
		r := st.idle
		if r == nil {
			continue
		}
		// Idle since anything of this conversation was seen, and the cache
		// counted from the request held: a request beside it (a recap, a
		// thread's turn) is activity and reads none of that entry.
		idle, age := now.Sub(st.seen.Round(0)), now.Sub(r.at.Round(0))
		if !cfg.Enabled || cfg.NoIdleCompaction {
			st.idle = nil
			continue
		}
		if idle < idleCompactAfter {
			continue
		}
		// One look, whatever comes of it: the request is not kept past it.
		st.idle = nil
		window := time.Duration(cfg.WindowMinutes) * time.Minute
		switch {
		case age >= idleCompactUntil:
			s.logger.Printf("compaction idle skipped session=%s: its latest whole request is %s old when looked at, so its cache may be gone already (a Mac asleep, or the session went on in another way)", key, age.Round(time.Minute))
			continue
		case st.pending || st.next != "" || st.lastContext < r.floor:
			continue
		case !st.startedAt.IsZero() && now.Sub(st.startedAt.Round(0)) < window:
			continue
		}
		msgs := r.msgs
		view, offset := msgs, 0
		if st.summary != "" && st.swapAt > 0 {
			if len(msgs) <= st.p0 {
				continue
			}
			view = rewriteWithSummary(msgs, st.summary, st.p0, st.swapAt)
			offset = len(msgs) - len(view)
		}
		if !hasAssistant(msgs) {
			continue
		}
		safeP, _ := compactionBoundary(view, promptBoundaries(msgs), offset)
		p := len(msgs)
		st.startedAt, st.pending = time.Now(), true
		history := append([]json.RawMessage(nil), view...)
		hash, safeHash := st.hashOf(msgs, p), ""
		if safeP > 0 {
			safeHash = st.hashOf(msgs, safeP)
		}
		st.pendingMarks = messageMarks(msgs, p)
		s.logger.Printf("compaction start session=%s context=%dk (%s) summarising %d of %d messages: idle %s, before its one-hour cache expires", key, st.lastContext/1000, r.limit, p, len(msgs), idle.Round(time.Minute))
		st.notice("idle %s at %dk: %d messages summarised before the cache expired, so coming back writes the summary and not the history", idle.Round(time.Minute), st.lastContext/1000, p)
		s.compaction.running.Add(1)
		ctx, cancel := context.WithCancelCause(context.Background())
		s.compaction.cancels[key] = cancel
		go s.summarise(ctx, r.in, r.top, history, len(view), key, p, hash, safeP, safeHash)
		started++
	}
	if started > 0 {
		s.compaction.save()
	}
	return started
}

// WatchIdleSessions runs compactIdle every minute until ctx ends.
func (s *Server) WatchIdleSessions(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.compactIdle(time.Now())
		}
	}
}

package router

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// What a session really sends, from the usage Anthropic reports back, held
// against the limit it should be compacted at. Compaction judges a session
// by what it measured itself; when it measures nothing (a body it cannot
// read, a conversation it never keyed, a compaction stuck waiting) its own
// figures stand still and nothing on screen moves. On 11 and 12 Nov 2026 a
// session whose repository compacts at 175k sent 350k to 370k a turn for a
// day while the panel read 187k from the day before. This watch needs
// nothing but the reply's usage and the session header, so it still sees
// what compaction does not.

// sentOverGrace is how long past the compaction window a session may keep
// sending over its limit before it is reported: a session over its limit
// legitimately waits out the window and its summary.
var (
	sentOverGrace = 15 * time.Minute
	// sentOverGap ends a run of over-limit turns: none for this long and
	// the session came down (compacted) or stopped.
	sentOverGap = 10 * time.Minute
	// sentOverTurns is the fewest over-limit turns that make a run.
	sentOverTurns = 5
)

const alertSentOver = "sent-over-limit"

type sentRun struct {
	first, last time.Time
	turns       int
	peak        int64
	alerted     bool
}

type sentWatch struct {
	mu   sync.Mutex
	runs map[string]*sentRun // session -> its current run of over-limit turns
	// unread is each session's latest request Burst could not read: a
	// measured context older than it is not what the session sends.
	unread map[string]time.Time
}

// noteUnread records that session sid sent a request Burst could not read.
func (s *Server) noteUnread(sid string, at time.Time) {
	if sid == "" {
		return
	}
	s.sent.mu.Lock()
	if s.sent.unread == nil {
		s.sent.unread = map[string]time.Time{}
	}
	s.sent.unread[sid] = at
	if len(s.sent.unread) > 2000 {
		for k, t := range s.sent.unread {
			if at.Sub(t) > 24*time.Hour {
				delete(s.sent.unread, k)
			}
		}
	}
	s.sent.mu.Unlock()
}

// lastUnread is session sid's latest request Burst could not read, zero if
// none since the gateway started.
func (s *Server) lastUnread(sid string) time.Time {
	s.sent.mu.Lock()
	defer s.sent.mu.Unlock()
	return s.sent.unread[sid]
}

// watchSent holds a primary reply's context against the session's limit.
func (s *Server) watchSent(in *http.Request, tok tokenUsage) {
	sid := in.Header.Get("x-claude-code-session-id")
	ctxTokens := tok.input + tok.cacheRead + tok.cacheWrite
	if sid == "" || ctxTokens <= 0 || in.URL.Path != "/v1/messages" || s.compaction == nil {
		return
	}
	_, root := s.repos.Resolve(sid)
	s.compaction.mu.Lock()
	cfg, override := s.compaction.cfg.ForRepo(root)
	s.compaction.mu.Unlock()
	if !cfg.Enabled || (override != nil && override.Off) || cfg.CompactAtTokens <= 0 {
		return
	}
	now := time.Now()
	s.sent.mu.Lock()
	if s.sent.runs == nil {
		s.sent.runs = map[string]*sentRun{}
	}
	run := s.sent.runs[sid]
	cleared := run != nil && run.alerted && now.Sub(run.last) >= sentOverGap
	if run != nil && now.Sub(run.last) >= sentOverGap {
		delete(s.sent.runs, sid)
		run = nil
	}
	if ctxTokens < cfg.CompactAtTokens {
		// Under the limit says nothing on its own: a subagent or a side
		// call of the same session is small whatever the main conversation
		// sends. Only a gap in over-limit turns ends a run.
		s.sent.mu.Unlock()
		if cleared {
			notice.PublishFor(sid, alertSentOver, notice.OK, "Session back under its compaction limit",
				fmt.Sprintf("Session %s has sent nothing over %dk for %d minutes.", short(sid), cfg.CompactAtTokens/1000, int(sentOverGap.Minutes())))
		}
		return
	}
	if run == nil {
		run = &sentRun{first: now}
		s.sent.runs[sid] = run
	}
	run.last = now
	run.turns++
	run.peak = max(run.peak, ctxTokens)
	window := time.Duration(cfg.WindowMinutes) * time.Minute
	report := !run.alerted && run.turns >= sentOverTurns && now.Sub(run.first) >= window+sentOverGrace
	if report {
		run.alerted = true
	}
	first, turns, peak := run.first, run.turns, run.peak
	if len(s.sent.runs) > 500 {
		for k, r := range s.sent.runs {
			if now.Sub(r.last) >= sentOverGap {
				delete(s.sent.runs, k)
			}
		}
	}
	s.sent.mu.Unlock()
	if !report {
		return
	}
	mins := int(now.Sub(first).Minutes())
	s.logger.Printf("req=%s warn compaction not holding session=%s: %d turns over %d minutes sent more than its %dk limit, up to %dk; compaction is not keeping this session down",
		requestIDFrom(in.Context()), sid, turns, mins, cfg.CompactAtTokens/1000, peak/1000)
	notice.PublishFor(sid, alertSentOver, notice.Warn, "Session sending over its compaction limit",
		fmt.Sprintf("Session %s has sent more than %dk on %d turns over %d minutes (up to %dk), past the %d minute compaction window. Compaction is not keeping it down; each turn costs the whole context.",
			short(sid), cfg.CompactAtTokens/1000, turns, mins, peak/1000, cfg.WindowMinutes))
}

func short(sid string) string { return sid[:min(8, len(sid))] }

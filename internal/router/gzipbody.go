package router

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// Claude Code can gzip its request bodies (CLAUDE_CODE_GZIP_REQUEST_BODIES,
// and a rollout flag that turns it on per process as it starts). A gzipped
// body is not JSON, so everything Burst reads in a request read nothing:
// compaction, the inspector, automask, the model and the pricing. On
// 12 Nov 2026 every session started at the 09:50 boot the day before had it
// on, and a session whose repository compacts at 175k went out at 368k on
// every turn, with nothing in the log to say so: compaction never saw it.
//
// So a gzipped body is opened once, as it arrives, and everything after
// sees JSON. It goes upstream to Anthropic gzipped again, as the client sent
// it (see gzipForUpstream); other providers rebuild the body and take it
// plain.

type gzippedKey struct{}

var errInflatedTooLarge = errors.New("the request body is larger than max_request_mb once uncompressed")

// gunzipRequest opens a gzip-encoded body, at most limit bytes once opened.
// A request that is not gzip comes back as it was, with ok false.
func gunzipRequest(r *http.Request, body []byte, limit int64) (*http.Request, []byte, bool, error) {
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Content-Encoding")), "gzip") || len(body) == 0 {
		return r, body, false, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return r, body, false, err
	}
	plain, err := io.ReadAll(io.LimitReader(zr, limit+1))
	if err != nil {
		return r, body, false, err
	}
	if int64(len(plain)) > limit {
		return r, body, false, errInflatedTooLarge
	}
	r = r.WithContext(context.WithValue(r.Context(), gzippedKey{}, true))
	r.Header.Del("Content-Encoding")
	r.ContentLength = int64(len(plain))
	return r, plain, true, nil
}

// clientGzipped is whether the client sent this request's body gzipped.
func clientGzipped(ctx context.Context) bool {
	v, _ := ctx.Value(gzippedKey{}).(bool)
	return v
}

// gzipForUpstream compresses body again for a client that sent it gzipped:
// a long conversation is megabytes of JSON, and the client chose not to
// send that over its network plain. The level is gzip's default, which
// costs milliseconds on a body this size.
func gzipForUpstream(body []byte) ([]byte, error) {
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	if _, err := zw.Write(body); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// alertUnreadable says so, on screen and in the log, when a turn's body
// cannot be read: compaction, the inspector, automask and pricing all depend
// on it, and each of them used to skip such a request without a word. On
// 11 and 12 Nov 2026 every request of seven sessions was unreadable (gzip,
// before Burst opened it) for 24 hours; the log said only model="" 456
// times, and nobody was told. Once a session an hour, so it is seen and
// does not repeat on every turn.
func (s *Server) alertUnreadable(r *http.Request, body []byte, rid string) {
	if r.URL.Path != "/v1/messages" || len(body) == 0 || requestModel(body) != "" {
		return
	}
	sid := r.Header.Get("x-claude-code-session-id")
	now := time.Now()
	s.noteUnread(sid, now)
	s.alerts.mu.Lock()
	if s.alerts.unreadable == nil {
		s.alerts.unreadable = map[string]time.Time{}
	}
	last := s.alerts.unreadable[sid]
	if now.Sub(last) < time.Hour {
		s.alerts.mu.Unlock()
		return
	}
	s.alerts.unreadable[sid] = now
	if len(s.alerts.unreadable) > 500 {
		for k, at := range s.alerts.unreadable {
			if now.Sub(at) >= time.Hour {
				delete(s.alerts.unreadable, k)
			}
		}
	}
	s.alerts.mu.Unlock()
	why := "it is not JSON with a model in it"
	if enc := r.Header.Get("Content-Encoding"); enc != "" {
		why = "it is compressed (Content-Encoding " + enc + "), which Burst does not open"
	}
	who := "a session"
	if sid != "" {
		who = "session " + short(sid)
	}
	s.logger.Printf("req=%s warn stage=read_body reason=unreadable session=%s: %s, so compaction, the inspector and pricing skip it and it is sent as it came", rid, sid, why)
	notice.Publish(alertUnreadable, notice.Warn, "Burst cannot read Claude Code's requests",
		fmt.Sprintf("Requests from %s go through Burst unread: %s. Compaction does not measure or compact that session, so it sends its whole context on every turn.", who, why))
}

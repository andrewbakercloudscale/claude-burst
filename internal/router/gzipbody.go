package router

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
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

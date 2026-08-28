package router

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// PassthroughProvider forwards a request unchanged to a base URL, without
// injecting or holding any credential of its own. It backs two configured
// provider names:
//
//   - "anthropic" (provider type "oauth-passthrough"): the gateway relies on
//     Claude Code's own saved Max/Pro OAuth subscription credential, which
//     Claude Code sends on every request.
//   - "anthropic-api-key": the gateway relies on Claude Code's own
//     ANTHROPIC_API_KEY (set in Claude Code's env, not the gateway's), sent
//     as the x-api-key header on every request.
//
// Both cases are mechanically identical: forward whatever auth header
// Claude Code already sent. They differ only in which FailoverDetector is
// paired with them (see failover.go).
type PassthroughProvider struct {
	name string
	base *url.URL
}

func NewPassthroughProvider(name string, base *url.URL) *PassthroughProvider {
	return &PassthroughProvider{name: name, base: base}
}

func (p *PassthroughProvider) Name() string { return p.name }

func (p *PassthroughProvider) Prepare(ctx context.Context, in *http.Request, body []byte) (*http.Request, string, error) {
	model := requestModel(body)
	req, err := buildForwardRequest(ctx, p.base, in, body, false)
	if err != nil {
		return nil, "", &ProviderError{Status: http.StatusBadGateway, Stage: "build_request", Model: model, Err: err}
	}
	return req, model, nil
}

// buildForwardRequest builds the outbound request against base, copying the
// inbound method/path/query/body and headers. When stripInboundAuth is
// true, the caller is a provider that injects its own credential (e.g.
// Bedrock) and wants inbound Cookie/Authorization/x-api-key headers removed
// first so they can't leak to a different backend.
func buildForwardRequest(ctx context.Context, base *url.URL, in *http.Request, body []byte, stripInboundAuth bool) (*http.Request, error) {
	u := *base
	basePath := strings.TrimRight(u.Path, "/")
	u.Path = basePath + in.URL.Path
	u.RawQuery = in.URL.RawQuery
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, in.Method, u.String(), rd)
	if err != nil {
		return nil, err
	}
	for k, vals := range in.Header {
		lk := strings.ToLower(k)
		if lk == "host" || lk == "content-length" || lk == "connection" || lk == "accept-encoding" {
			continue
		}
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	if stripInboundAuth {
		req.Header.Del("Cookie")
		req.Header.Del("Authorization")
		req.Header.Del("x-api-key")
	}
	return req, nil
}

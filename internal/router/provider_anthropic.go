package router

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
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
	// path.Join cleans ".."/"." segments before they ever reach the
	// credentialed upstream. Confirm the cleaned result still lands under
	// basePath rather than trusting the join alone: an inbound path with
	// enough ".." segments to fully escape basePath would otherwise land on
	// an unintended path at the SAME host, which for the Bedrock provider
	// still carries its injected credential.
	joined := path.Join(basePath, in.URL.Path)
	if basePath != "" && joined != basePath && !strings.HasPrefix(joined, basePath+"/") {
		return nil, fmt.Errorf("request path %q escapes base path %q", in.URL.Path, basePath)
	}
	u.Path = joined
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

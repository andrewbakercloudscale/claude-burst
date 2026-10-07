package router

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
)

// primaryRetryDelays are the waits between retries of a primary request that
// failed at the transport level, about 30 seconds in all. A variable for tests.
var primaryRetryDelays = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}

// withModel rewrites the "model" field of an Anthropic request body, leaving
// everything else byte-for-byte as the client sent it.
func withModel(body []byte, model string) ([]byte, error) {
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("cannot retarget request body: %w", err)
	}
	v["model"] = model
	return json.Marshal(v)
}

// forward drives one hop of the proxy through Provider p: build the outbound
// request, call it, and either relay success, pass an ordinary error straight
// back to the client, or -- when allowFailover is true and fd decides the
// failure warrants it -- activate the overflow window and replay the same
// request to s.secondary. The secondary hop is always invoked with
// allowFailover=false, so a failure never chains past two hops.
// clientFor picks the bounded client for inference and the unbounded one for
// pass-through control-plane traffic. Built defensively: a Server assembled in
// a test without passthroughClient still works.
func (s *Server) clientFor(path string) *http.Client {
	if !isInference(path) && s.passthroughClient != nil {
		return s.passthroughClient
	}
	return s.client
}

func (s *Server) forward(w http.ResponseWriter, in *http.Request, body []byte, slot string, p Provider, fd FailoverDetector, allowFailover bool, note string, ladder []string) {
	rid := requestIDFrom(in.Context())
	start := time.Now()

	req, model, err := p.Prepare(in.Context(), in, body)
	if err != nil {
		status, stage, reqModel := http.StatusBadGateway, "build_request", ""
		var perr *ProviderError
		if errors.As(err, &perr) {
			status, stage, reqModel = perr.Status, perr.Stage, perr.Model
		}
		s.logger.Printf("req=%s error stage=%s route=%s requested_model=%q err=%v", rid, stage, p.Name(), reqModel, err)
		http.Error(w, err.Error(), status)
		s.writeMetric(in, slot, p.Name(), reqModel, reqModel, status, start, tokenUsage{}, "", 0, stage+" failed: "+err.Error(), "")
		return
	}

	// The actual outbound URL this hop is sent to -- scheme+host+path, no
	// query -- so metrics.jsonl and the admin UI can show which real backend
	// served a request rather than just the configured slot name, which is
	// what the "still going to primary?" confusion in practice turns out to
	// be: the slot label was right, but nothing showed the URL to check it
	// against.
	destination := req.URL.Scheme + "://" + req.URL.Host + req.URL.Path

	// What context pruning did to this request, recorded on failures as well
	// as successes: comparing the two is how a prune that confuses the model
	// shows up, and a row that only exists when the request worked cannot
	// show that.
	pruned := prunedUsage(req.Context())

	// The model that actually served, for metrics and the admin view. Falls
	// back to the requested model for passthrough providers, which serve with
	// exactly what was asked for.
	serveModel := model
	if sm, ok := p.(ServeModeler); ok {
		serveModel = sm.ServeModel(model)
	}

	resp, err := s.clientFor(in.URL.Path).Do(req)
	// A dead connection on the FIRST attempt gets one immediate retry on a
	// fresh connection (below), then the timed ladder; the ladder does not
	// repeat that first retry. This runs with or without a secondary: it is
	// the cheapest recovery there is, and on a Mac with nowhere to fail over
	// it is the one retry Burst makes before Claude Code's own.
	freshRetried := false
	if err != nil && in.Context().Err() == nil && (isStaleWriteFailure(err) || slot == "primary" && deadConnection(err)) {
		// The kept-alive connection died under us (a network switch), and the
		// write onto it failed, so the server never saw the request. Drop the
		// pooled connections and send it again on a fresh one, once, BEFORE
		// treating it as a failure: counting this against the primary is what
		// armed a failover window on 2026-09-21 that then sent a healthy
		// primary's traffic to a secondary that was also unreachable.
		s.clientFor(in.URL.Path).CloseIdleConnections()
		if retry, _, perr := p.Prepare(in.Context(), in, body); perr == nil {
			s.logger.Printf("req=%s retry route=%s reason=%q -> one more attempt on a fresh connection", rid, p.Name(), err)
			req = retry
			resp, err = s.clientFor(in.URL.Path).Do(req)
			freshRetried = true
			// Not counted here. If the retry also failed, the single
			// fd.OnError after the ladder below counts this request, once.
			// It used to be counted here AND there, so one request whose
			// fresh-connection retry also failed was "2 failures within
			// 60s" on its own (req=33a2ad7292388370, 2026-10-02 12:04, a
			// broken pipe on a phone hotspot): any transport threshold of 2
			// was met by a single request.
		}
	}
	if resp != nil {
		s.recent.add(RecentResponse{
			Time: start, RequestID: rid, Method: in.Method, Path: in.URL.Path,
			Slot: slot, Route: p.Name(), Model: serveModel, Status: resp.StatusCode,
			DurationMS: time.Since(start).Milliseconds(), Headers: filterHeaders(resp.Header),
			Destination: destination,
		})
	}
	if err != nil {
		// Logged on every transport-level failure, not just the ones that
		// exhaust failover: on 2026-09-03 primary timed out mid-connection and
		// secondary's DNS lookup failed outright ~90s later, and the log gave
		// no way to tell whether that was one continuous network outage or two
		// unrelated failures. This snapshot answers that next time.
		// A client that went away needs no network diagnosis: until
		// 2026-10-03 every cancelled request (Esc, a heartbeat dropped at
		// session end) ran a DNS probe and logged a full snapshot, and these
		// lines were 16% of the log's bytes.
		var np netProbe // set whenever the client is still there, the only case that reads it
		if in.Context().Err() == nil {
			probe := s.probe
			if probe == nil {
				probe = probeNetwork
			}
			np = probe()
			s.logSnapshot(rid, p.Name(), err, np)
		}
		// The client's own context is what Prepare was given, so if it is
		// done, this request died because the CALLER went away -- not because
		// the upstream failed. Checked here as well as in the detector
		// because this is the authoritative signal (the detector only sees a
		// wrapped error string away from it), and because the consequence
		// here is concrete: replaying to the secondary would spend money on a
		// paid provider generating a response that nobody is left to read.
		if in.Context().Err() != nil {
			s.logger.Printf("req=%s client_gone route=%s err=%v (no failover, not replayed)", rid, p.Name(), err)
			s.writeMetric(in, slot, p.Name(), serveModel, model, metrics.StatusClientClosed, start, pruned, "", 0, "client cancelled: "+err.Error(), destination)
			return
		}
		if slot == "secondary" {
			// The secondary just failed at the transport level. If the window
			// that sent traffic here was armed by an outage (not by a rate limit,
			// which the primary would only refuse again), it is not helping:
			// release it so the next request tries the primary rather than
			// queueing behind a secondary that cannot answer.
			//
			// Keyed by the model the CLIENT asked for, read from the body: what
			// Prepare returns for a secondary is that provider's own model id,
			// and the window was armed under the requested one.
			s.releaseOutageWindow(requestModel(body))
		}
		if !np.dnsOK && !peerAnswered(err) && !isClientCancellation(err) {
			// Silence from the far side while this machine cannot resolve any
			// name: the network is down, and the secondary is behind the same
			// network. Failing over would only make the request wait on a second
			// dead host (four Together timeouts in a row, 2026-09-21), and it
			// would arm a window blaming a model for a laptop changing WiFi.
			s.notePrimaryFailure(slot, err)
			s.alertNetworkDown()
			// One line when it starts and one a minute after: every request
			// of an outage fails the same way (928 of these in one hour on
			// 6 Oct 2026), and each still has its own done line and metric.
			if ok, skipped := s.repeats.allow("network down", time.Now()); ok {
				s.logger.Printf("req=%s no_failover route=%s reason=%q (local network unavailable: control DNS failed)%s", rid, p.Name(), "network down", andMore(skipped))
			}
			http.Error(w, "local network unavailable (DNS is failing on this machine) -- not failing over, since the secondary is behind the same network: "+err.Error(), http.StatusBadGateway)
			s.writeMetric(in, slot, p.Name(), serveModel, model, http.StatusBadGateway, start, pruned, "", 0, "local network unavailable; not failed over: "+err.Error(), destination)
			return
		}
		if np.webDown && !isClientCancellation(err) {
			// Names resolve but the control request got nowhere either: the
			// fault is this Mac's network, and the secondary is behind the
			// same network, so nothing fails over. A reset counts here too,
			// unlike above: the carrier sends it, not Anthropic, and the
			// control request proves it.
			s.notePrimaryFailure(slot, err)
			if s.primaryAnsweredWithin(webDownGrace) {
				// Anthropic answered another request moments ago: slow, not
				// cut. On 2026-10-06 a congested phone hotspot took 7 to 12
				// seconds over replies that take half a second, the control
				// request ran out its 4, and "connections from this Mac are
				// cut" went up on screen in the same second a reply arrived.
				// No alert. Still no failover: it is the network.
				s.logger.Printf("req=%s no_failover route=%s reason=%q (control HTTPS failed: %v, but the primary answered within %s: no alert)", rid, p.Name(), "network slow", np.webErr, webDownGrace)
				http.Error(w, "this Mac's network is slow or dropping connections -- not failing over, since the secondary is behind the same network: "+err.Error(), http.StatusBadGateway)
				s.writeMetric(in, slot, p.Name(), serveModel, model, http.StatusBadGateway, start, pruned, "", 0, "local network slow; not failed over: "+err.Error(), destination)
				return
			}
			s.alertNetworkBlocked()
			s.logger.Printf("req=%s no_failover route=%s reason=%q (local network not passing traffic: control HTTPS failed: %v)", rid, p.Name(), "network blocked", np.webErr)
			http.Error(w, "this Mac's network is not passing traffic (a phone out of data, a captive portal or a dead uplink) -- not failing over, since the secondary is behind the same network: "+err.Error(), http.StatusBadGateway)
			s.writeMetric(in, slot, p.Name(), serveModel, model, http.StatusBadGateway, start, pruned, "", 0, "local network not passing traffic; not failed over: "+err.Error(), destination)
			return
		}
		// A transport error on the primary is retried for about 30 seconds
		// before it can count towards failing over -- and only once the local
		// network is known to be up, so a dead network still answers fast.
		// Anthropic is almost never what failed: it is a network change, a
		// stalled mobile link or a DNS hiccup, and each of those sent requests
		// to the paid secondary on 2026-09-30 when a single failure was enough.
		// Nothing has reached the client yet at this point, so a retry is
		// invisible to it. With nowhere to fail over to it does not run:
		// the error goes straight back and Claude Code's own retries, which
		// show on screen, do the same job.
		for i, d := range primaryRetryDelays {
			// Only errors with no bytes read from the server are retried at
			// all. A read-side reset may mean the server already ran the
			// request, so replaying it could run one generation twice
			// (TestReadSideResetIsNotRetried). A stale write already had its
			// one immediate fresh-connection retry above; the ladder then
			// waits for the network to recover instead of hammering.
			if err == nil || slot != "primary" || !allowFailover || in.Context().Err() != nil || freshRetried || !safeToResend(err) {
				break
			}
			s.logger.Printf("req=%s retry route=%s attempt=%d of %d in %s reason=%q", rid, p.Name(), i+2, len(primaryRetryDelays)+1, d, err)
			select {
			case <-in.Context().Done():
			case <-time.After(d):
			}
			if in.Context().Err() != nil {
				break
			}
			s.clientFor(in.URL.Path).CloseIdleConnections()
			retry, _, perr := p.Prepare(in.Context(), in, body)
			if perr != nil {
				break
			}
			req = retry
			resp, err = s.clientFor(in.URL.Path).Do(req)
			if err == nil {
				s.logger.Printf("req=%s retry route=%s attempt=%d worked; no failover", rid, p.Name(), i+2)
			}
		}
		if err == nil {
			// A retry recovered. Fall through to the normal response path:
			// before this, the code below ran with a nil err, and err.Error()
			// panicked the handler instead of returning the good response.
			s.recent.add(RecentResponse{
				Time: start, RequestID: rid, Method: in.Method, Path: in.URL.Path,
				Slot: slot, Route: p.Name(), Model: serveModel, Status: resp.StatusCode,
				DurationMS: time.Since(start).Milliseconds(), Headers: filterHeaders(resp.Header),
				Destination: destination,
			})
		} else if in.Context().Err() != nil {
			// The client left during the retries (Claude Code gives up on a
			// turn after a while). Failing over now would arm a window and
			// raise an alert for a request nobody is waiting for, as it did
			// on 2026-10-04 at 17:16 (req=5dfd9304c61e335b).
			s.logger.Printf("req=%s client_gone route=%s err=%v (left during retries; no failover, not replayed)", rid, p.Name(), err)
			s.writeMetric(in, slot, p.Name(), serveModel, model, metrics.StatusClientClosed, start, pruned, "", 0, "client cancelled during retries: "+err.Error(), destination)
			return
		} else {
			s.notePrimaryFailure(slot, err)
			if allowFailover {
				if d := fd.OnError(err); d.Failover {
					s.activateOverflow(model, d.ResetAt, d.Claim, d.Reason)
					s.replayElsewhere(w, in, body, slot, p, model, serveModel, destination, start, 0, d, ladder,
						fmt.Sprintf("transport error: %v", err))
					return
				}
			}
			s.logger.Printf("req=%s error stage=upstream_call route=%s err=%v", rid, p.Name(), err)
			http.Error(w, p.Name()+" upstream error: "+err.Error(), http.StatusBadGateway)
			s.writeMetric(in, slot, p.Name(), serveModel, model, http.StatusBadGateway, start, pruned, "", 0, "upstream call failed: "+err.Error(), destination)
			return
		}
	}
	s.notePrimaryAnswered(slot)

	// Successful responses must stream immediately; don't buffer them.
	if resp.StatusCode < 400 {
		if fd != nil {
			fd.OnSuccess()
		}
		var tok tokenUsage
		if t, ok := p.(Translator); ok {
			tok, err = t.TranslateResponse(w, resp, model)
			if err != nil {
				// The response has likely already started writing to the
				// client by this point (translation is itself streaming);
				// there's nothing safe left to do but log it.
				s.logger.Printf("req=%s error stage=translate_response route=%s model=%q err=%v", rid, p.Name(), model, err)
			}
		} else {
			tok = s.relay(rid, w, resp, model)
		}
		tok.prunedBytes, tok.prunedResults, tok.truncatedResults = pruned.prunedBytes, pruned.prunedResults, pruned.truncatedResults
		tok.repeatedCalls, tok.rerunsAfterStub = pruned.repeatedCalls, pruned.rerunsAfterStub
		if slot == "primary" {
			s.noteSessionContext(in, tok)
		}
		// A call that is not a model request, answered quickly with nothing
		// to note (a heartbeat, an event upload), is in the minute's quiet
		// count from ServeHTTP and in metrics.jsonl: not a line of its own.
		if isInference(in.URL.Path) || note != "" || time.Since(start) >= quietSlow {
			s.logger.Printf("req=%s ok route=%s model=%q status=%d dur_ms=%d in_tok=%d out_tok=%d note=%q",
				rid, p.Name(), model, resp.StatusCode, time.Since(start).Milliseconds(), tok.input, tok.output, note)
		}
		s.writeMetric(in, slot, p.Name(), serveModel, model, resp.StatusCode, start, tok, "", 0, note, destination)
		return
	}

	errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	_ = resp.Body.Close()

	// A 400 on a request carrying an unproven mid-turn compaction swap: undo
	// the swap and send what it would have been without it. Nothing has been
	// written to the client yet, so Claude Code sees only a slower reply,
	// never the error. Before the failover detector, so the swap's 400 is
	// never counted against the primary.
	if resp.StatusCode == http.StatusBadRequest && slot == "primary" && s.rejectMidTurn(in, errorExcerpt(errBody)) {
		orig := compactInfoFrom(in.Context()).original
		resent, in2 := s.applyCompaction(in, orig)
		// As handle does after compaction: what the user removed stays
		// removed on the resend too.
		resent = s.applyRemovals(in2.Header.Get("x-claude-code-session-id"), resent)
		s.logger.Printf("req=%s retry route=%s reason=%q -> resending without the mid-turn compaction swap", rid, p.Name(), "mid-turn swap rejected")
		s.forward(w, in2, resent, slot, p, fd, allowFailover, note, ladder)
		return
	}

	if allowFailover {
		if d := fd.OnResponse(resp.StatusCode, resp.Header, errBody); d.Failover {
			s.activateOverflow(model, d.ResetAt, d.Claim, d.Reason)
			s.replayElsewhere(w, in, body, slot, p, model, serveModel, destination, start, resp.StatusCode, d, ladder,
				fmt.Sprintf("status=%d", resp.StatusCode))
			return
		}
	}

	reason := errorExcerpt(errBody)
	s.logger.Printf("req=%s upstream_error route=%s model=%q status=%d note=%q reason=%q", rid, p.Name(), model, resp.StatusCode, note, reason)
	if resp.StatusCode == http.StatusBadRequest && slot == "secondary" {
		// A 400 from a translated request is usually a structure the
		// translation got wrong; the shape (never the content) is what
		// fixing it needs.
		s.logger.Printf("req=%s rejected_request_shape %s", rid, messageSkeleton(body))
	}
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(errBody)
	s.writeMetric(in, slot, p.Name(), serveModel, model, resp.StatusCode, start, pruned, "", 0, "upstream error; no failover: "+reason, destination)
}

// messageSkeleton describes a Messages request's shape -- each message's
// role and its blocks' types, with tool ids -- and nothing of its content.
func messageSkeleton(body []byte) string {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return "(unparseable)"
	}
	parts := make([]string, 0, len(req.Messages))
	for _, m := range req.Messages {
		var blocks []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			ToolUseID string `json:"tool_use_id"`
		}
		if json.Unmarshal(m.Content, &blocks) != nil {
			parts = append(parts, m.Role+"[str]")
			continue
		}
		kinds := make([]string, 0, len(blocks))
		for _, b := range blocks {
			k := b.Type
			if id := b.ID + b.ToolUseID; id != "" {
				k += ":" + id
			}
			kinds = append(kinds, k)
		}
		parts = append(parts, m.Role+"["+strings.Join(kinds, " ")+"]")
	}
	return strings.Join(parts, " ")
}

// maxErrorExcerpt bounds how much of an upstream error body reaches the log
// and metrics: enough for the provider's message, not a whole echoed prompt.
const maxErrorExcerpt = 300

// errorExcerpt is the provider's own statement of why a request failed, on
// one line. From a JSON error body only the error's type and message are
// kept: the rest of such a body can echo the request back, and the log and
// the metrics hold no conversation text. A body that is not JSON (a proxy's
// page, a plain line) is kept from its start.
func errorExcerpt(body []byte) string {
	var j struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Error   json.RawMessage
	}
	s := ""
	if json.Unmarshal(body, &j) == nil {
		var e struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		var plain string
		switch {
		case json.Unmarshal(j.Error, &e) == nil && (e.Type != "" || e.Message != ""):
			s = e.Type + ": " + e.Message
			if e.Type == "" || e.Message == "" {
				s = e.Type + e.Message
			}
		case json.Unmarshal(j.Error, &plain) == nil && plain != "":
			s = plain
		case j.Message != "":
			s = j.Message
		default:
			return fmt.Sprintf("(%d bytes of JSON with no error message)", len(body))
		}
	} else {
		s = string(body)
	}
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxErrorExcerpt {
		s = strings.ToValidUTF8(s[:maxErrorExcerpt], "") + "..."
	}
	return s
}

// replayElsewhere is where a failover decision turns into a second hop: down
// the fallback chain to another Claude model on the subscription, or, when
// the chain is exhausted or empty, out to the paid secondary.
//
// The chain is tried first on purpose. A rejection names one model; the rungs
// are models the subscription may still be serving, and they cost nothing.
// Only when none is available does this spend money. If neither is possible
// the upstream's own error goes back to the client unchanged, which is the
// behaviour a gateway with no secondary always had.
func (s *Server) replayElsewhere(w http.ResponseWriter, in *http.Request, body []byte,
	slot string, p Provider, model, serveModel, destination string, start time.Time, status int,
	d FailoverDecision, ladder []string, trigger string) {

	rid := requestIDFrom(in.Context())

	for len(ladder) > 0 {
		rung := ladder[0]
		ladder = ladder[1:]
		// Skip a rung refused while this request was in flight, but only
		// when there is somewhere after it: the last place left is always
		// worth asking.
		if s.modelInOverflow(rung, time.Now()) && (len(ladder) > 0 || s.secondaryReady()) {
			continue
		}
		downgraded, err := withModel(body, rung)
		if err != nil {
			s.logger.Printf("req=%s error stage=downgrade model=%q err=%v", rid, rung, err)
			break
		}
		s.writeMetric(in, slot, p.Name(), serveModel, model, status, start, tokenUsage{}, d.Claim, d.ResetAt,
			d.Reason+"; request replayed to "+rung, destination)
		s.logger.Printf("req=%s failover route=%s model=%q claim=%s reason=%q (%s) -> replaying on the subscription as %q",
			rid, p.Name(), model, d.Claim, d.Reason, trigger, rung)
		// The rung can be refused too; when it is, this same path carries on
		// to the next rung or the secondary. With neither left, its refusal
		// goes back to Claude Code as Anthropic sent it.
		s.forward(w, in, downgraded, "primary", s.primary, s.primaryDetector, s.secondaryReady() || len(ladder) > 0, "downgraded from "+model, ladder)
		return
	}

	if !s.secondaryReady() {
		s.logger.Printf("req=%s no_failover_target route=%s model=%q claim=%s reason=%q (%s): fallback chain exhausted and no secondary configured",
			rid, p.Name(), model, d.Claim, d.Reason, trigger)
		http.Error(w, p.Name()+" refused this model ("+d.Reason+") and there is no fallback chain rung or secondary provider left to try", http.StatusServiceUnavailable)
		s.writeMetric(in, slot, p.Name(), serveModel, model, status, start, tokenUsage{}, d.Claim, d.ResetAt,
			d.Reason+"; no fallback target", destination)
		return
	}

	s.writeMetric(in, slot, p.Name(), serveModel, model, status, start, tokenUsage{}, d.Claim, d.ResetAt,
		d.Reason+"; request replayed to secondary", destination)
	s.logger.Printf("req=%s failover route=%s model=%q claim=%s reason=%q (%s) -> replaying to secondary",
		rid, p.Name(), model, d.Claim, d.Reason, trigger)
	s.forward(w, in, body, "secondary", s.secondary, nil, false, d.Reason, nil)
}

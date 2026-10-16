package metrics

import (
	"math"
	"time"
)

// A held event is an Event in about half the memory: 160 bytes against
// 328. The counts are 32-bit, which a single request never comes near,
// and the strings that repeat from event to event (session, model, route,
// note and so on) are an index into the file's own table, so each is
// stored once. An event with a count that does not fit is held whole
// instead. TestPackingKeepsEveryField fails when a field is added to
// Event and not here.
type packedEvent struct {
	time      time.Time
	requestID string
	resetAt   int64
	prunedUSD float64
	usd       float64

	sessionID, agentID, slot, route, model, requestedModel, destination, limitClaim, note uint32

	httpStatus, durationMS                                               int32
	inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens         int32
	cacheWrite1hTokens, prunedBytes, prunedToolResults, truncatedResults int32
	repeatedCalls, rerunsAfterStub, compactedMessages, compactedBytes    int32

	pricingUnknown, repriced bool
}

// strTable is a file's repeated strings. Index 0 is the empty string.
type strTable struct {
	strs []string
	idx  map[string]uint32
}

func (t *strTable) intern(s string) uint32 {
	if s == "" {
		return 0
	}
	if i, ok := t.idx[s]; ok {
		return i
	}
	if t.idx == nil {
		t.idx = map[string]uint32{}
		t.strs = append(t.strs, "")
	}
	i := uint32(len(t.strs))
	t.strs = append(t.strs, s)
	t.idx[s] = i
	return i
}

func fits32(vs ...int64) bool {
	for _, v := range vs {
		if v < math.MinInt32 || v > math.MaxInt32 {
			return false
		}
	}
	return true
}

// pack returns e packed, or false when a count is too big to pack.
func pack(e *Event, t *strTable) (packedEvent, bool) {
	if !fits32(int64(e.HTTPStatus), e.DurationMS, e.InputTokens, e.OutputTokens, e.CacheReadTokens,
		e.CacheWriteTokens, e.CacheWrite1hTokens, e.PrunedBytes, e.PrunedToolResults, e.TruncatedToolResults,
		e.RepeatedCalls, e.RerunsAfterStub, e.CompactedMessages, e.CompactedBytes) {
		return packedEvent{}, false
	}
	return packedEvent{
		time: e.Time, requestID: e.RequestID, resetAt: e.ResetAt, prunedUSD: e.PrunedUSD, usd: e.APIEquivalentUSD,

		sessionID: t.intern(e.SessionID), agentID: t.intern(e.AgentID), slot: t.intern(e.Slot),
		route: t.intern(e.Route), model: t.intern(e.Model), requestedModel: t.intern(e.RequestedModel),
		destination: t.intern(e.Destination), limitClaim: t.intern(e.LimitClaim), note: t.intern(e.Note),

		httpStatus: int32(e.HTTPStatus), durationMS: int32(e.DurationMS),
		inputTokens: int32(e.InputTokens), outputTokens: int32(e.OutputTokens),
		cacheReadTokens: int32(e.CacheReadTokens), cacheWriteTokens: int32(e.CacheWriteTokens),
		cacheWrite1hTokens: int32(e.CacheWrite1hTokens), prunedBytes: int32(e.PrunedBytes),
		prunedToolResults: int32(e.PrunedToolResults), truncatedResults: int32(e.TruncatedToolResults),
		repeatedCalls: int32(e.RepeatedCalls), rerunsAfterStub: int32(e.RerunsAfterStub),
		compactedMessages: int32(e.CompactedMessages), compactedBytes: int32(e.CompactedBytes),

		pricingUnknown: e.PricingUnknown, repriced: e.Repriced,
	}, true
}

// unpack is pack's inverse; strs is the table the event was packed with.
func (p *packedEvent) unpack(strs []string) Event {
	return Event{
		Time: p.time, RequestID: p.requestID, ResetAt: p.resetAt, PrunedUSD: p.prunedUSD, APIEquivalentUSD: p.usd,

		SessionID: strs[p.sessionID], AgentID: strs[p.agentID], Slot: strs[p.slot],
		Route: strs[p.route], Model: strs[p.model], RequestedModel: strs[p.requestedModel],
		Destination: strs[p.destination], LimitClaim: strs[p.limitClaim], Note: strs[p.note],

		HTTPStatus: int(p.httpStatus), DurationMS: int64(p.durationMS),
		InputTokens: int64(p.inputTokens), OutputTokens: int64(p.outputTokens),
		CacheReadTokens: int64(p.cacheReadTokens), CacheWriteTokens: int64(p.cacheWriteTokens),
		CacheWrite1hTokens: int64(p.cacheWrite1hTokens), PrunedBytes: int64(p.prunedBytes),
		PrunedToolResults: int64(p.prunedToolResults), TruncatedToolResults: int64(p.truncatedResults),
		RepeatedCalls: int64(p.repeatedCalls), RerunsAfterStub: int64(p.rerunsAfterStub),
		CompactedMessages: int64(p.compactedMessages), CompactedBytes: int64(p.compactedBytes),

		PricingUnknown: p.pricingUnknown, Repriced: p.repriced,
	}
}

package router

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// Context pruning for the paid secondary.
//
// WHY ONLY THERE. Every overflow request resends the whole conversation to a
// metered provider: over 2026-08-30..09-28 that was 255 requests averaging
// 96k input tokens against ~700 output, input being >99% of the tokens and
// essentially all of the $40.63. Most of that input is old tool output --
// files read and commands run twenty turns ago that the model will not look
// at again. The primary is the opposite case and is never pruned: it is a
// flat-rate subscription whose context Anthropic caches, and rewriting
// history there would invalidate the cache every turn and burn the usage
// limit faster, not slower. Bedrock is excluded for the same reason.
//
// THREE TECHNIQUES, all deterministic so the same history always prunes to
// the same bytes:
//
//  1. Stub old tool results. Every tool_result except the most recent
//     KeepRecent is replaced with a one-line note saying how much was removed
//     and that the tool can be re-run.
//  2. Move that boundary in steps of Step results, not one per turn. A
//     boundary that advanced every turn would change the start of the prompt
//     every turn and defeat any prefix caching the provider does; stepping it
//     keeps the prefix byte-identical for Step turns at a time.
//  3. Cap any single tool result at MaxToolResultBytes, keeping its head and
//     tail. A 200 KB test log or page snapshot is mostly middle.
//
// The model loses detail it may occasionally need, which is why every stub
// says so and how to get it back; SecondaryPruning.Disabled turns it off.

const (
	defaultPruneKeepRecent   = 10
	defaultPruneStep         = 10
	defaultPruneStubMinBytes = 1024
	defaultPruneMaxBytes     = 40 * 1024
)

// setPruning swaps the provider's policy. Called at startup and by the admin
// page, so a toggle there takes effect on the next request without a
// restart; the pointer is atomic because requests read it concurrently.
func (p *OpenAICompatibleProvider) setPruning(c config.PruneConfig) {
	pol, on := newPrunePolicy(c)
	if !on {
		p.prune.Store(nil)
		return
	}
	p.prune.Store(&pol)
}

// SetSecondaryPruning applies c to the running secondary. It reports false
// when there is no openai-compatible secondary to prune, in which case the
// setting is saved but has nothing to act on.
func (s *Server) SetSecondaryPruning(c config.PruneConfig) bool {
	op, ok := s.secondary.(*OpenAICompatibleProvider)
	if !ok {
		return false
	}
	op.setPruning(c)
	return true
}

type pruneStatsKey struct{}

type pruneStats struct {
	bytesRemoved, stubbed, truncated int64
	// The model's latest tool calls, compared with its earlier ones:
	// repeatedCalls is how many repeat an earlier call exactly (the base
	// rate at which the model repeats itself), rerunsAfterStub how many
	// repeat a call whose output this request stubbed -- pruning costing a
	// round trip instead of saving one.
	repeatedCalls, rerunsAfterStub int64
}

type prunePolicy struct {
	stub, cap                                bool
	keepRecent, step, stubMinBytes, maxBytes int
}

// newPrunePolicy resolves c into a policy with its defaults filled in, and
// reports whether it does anything at all.
func newPrunePolicy(c config.PruneConfig) (prunePolicy, bool) {
	p := prunePolicy{stub: !c.NoStub, cap: !c.NoCap,
		keepRecent: c.KeepRecent, step: c.Step, stubMinBytes: c.StubMinBytes, maxBytes: c.MaxToolResultBytes}
	if p.keepRecent <= 0 {
		p.keepRecent = defaultPruneKeepRecent
	}
	if p.step <= 0 {
		p.step = defaultPruneStep
	}
	if p.stubMinBytes <= 0 {
		p.stubMinBytes = defaultPruneStubMinBytes
	}
	if p.maxBytes <= 0 {
		p.maxBytes = defaultPruneMaxBytes
	}
	return p, !c.Disabled && (p.stub || p.cap)
}

// pruneAnthropicRequest returns body with old tool results stubbed and
// oversized ones cut down. On anything it does not understand it returns
// body unchanged: pruning is a saving, never a reason to fail a request.
func pruneAnthropicRequest(body []byte, p prunePolicy) ([]byte, pruneStats) {
	var req map[string]any
	if json.Unmarshal(body, &req) != nil {
		return body, pruneStats{}
	}
	msgs, _ := req["messages"].([]any)

	var results []map[string]any
	callKey := map[string]string{} // tool_use id -> name + canonical input
	var earlier, latest []string   // call keys before, and in, the last assistant turn
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		blocks, _ := mm["content"].([]any)
		var calls []string
		for _, b := range blocks {
			bm, ok := b.(map[string]any)
			if !ok {
				continue
			}
			switch bm["type"] {
			case "tool_result":
				results = append(results, bm)
			case "tool_use":
				// json.Marshal sorts map keys, so equal inputs give equal keys.
				in, _ := json.Marshal(bm["input"])
				k := fmt.Sprint(bm["name"]) + " " + string(in)
				if id, _ := bm["id"].(string); id != "" {
					callKey[id] = k
				}
				calls = append(calls, k)
			}
		}
		if mm["role"] == "assistant" && len(calls) > 0 {
			earlier = append(earlier, latest...)
			latest = calls
		}
	}

	boundary := 0
	if len(results) > p.keepRecent {
		boundary = (len(results) - p.keepRecent) / p.step * p.step
	}

	var st pruneStats
	stubbedCalls := map[string]bool{}
	for i, r := range results {
		// The text translation would send: the OpenAI side flattens tool
		// results to text and drops images anyway.
		text := flattenTextContent(r["content"])
		switch {
		case p.stub && i < boundary && len(text) >= p.stubMinBytes:
			if id, _ := r["tool_use_id"].(string); callKey[id] != "" {
				stubbedCalls[callKey[id]] = true
			}
			r["content"] = fmt.Sprintf("[claude-burst: this earlier tool output (%d bytes) was removed to save overflow cost. Re-run the tool if you need it again.]", len(text))
			st.bytesRemoved += int64(len(text))
			st.stubbed++
		case p.cap && len(text) > p.maxBytes:
			half := p.maxBytes / 2
			// Byte offsets can split a multi-byte rune; drop the fragment.
			head, tail := strings.ToValidUTF8(text[:half], ""), strings.ToValidUTF8(text[len(text)-half:], "")
			cut := len(text) - len(head) - len(tail)
			r["content"] = head + fmt.Sprintf("\n\n[claude-burst: %d bytes cut from the middle of this tool output to save overflow cost]\n\n", cut) + tail
			st.bytesRemoved += int64(cut)
			st.truncated++
		}
	}
	seen := map[string]bool{}
	for _, k := range earlier {
		seen[k] = true
	}
	for _, k := range latest {
		if seen[k] {
			st.repeatedCalls++
		}
		if stubbedCalls[k] {
			st.rerunsAfterStub++
		}
	}
	if st.stubbed == 0 && st.truncated == 0 {
		return body, st
	}
	out, err := json.Marshal(req)
	if err != nil {
		return body, pruneStats{}
	}
	return out, st
}

func withPruneStats(ctx context.Context, st pruneStats) context.Context {
	return context.WithValue(ctx, pruneStatsKey{}, st)
}

func pruneStatsFrom(ctx context.Context) pruneStats {
	st, _ := ctx.Value(pruneStatsKey{}).(pruneStats)
	return st
}

// prunedUsage is the prune stats as a tokenUsage with no tokens, the shape
// writeMetric takes on every path.
func prunedUsage(ctx context.Context) tokenUsage {
	st := pruneStatsFrom(ctx)
	return tokenUsage{prunedBytes: st.bytesRemoved, prunedResults: st.stubbed, truncatedResults: st.truncated,
		repeatedCalls: st.repeatedCalls, rerunsAfterStub: st.rerunsAfterStub}
}

package router

import (
	"fmt"
	"strings"

	"github.com/andrewbakercloudscale/claude-burst/internal/ctxview"
)

// Prune: the inspector's Remove for many items at once, chosen by a word.
// /burst-prune in a session asks for it. It removes only what Remove can
// (tool results, instruction files, skills, other reminders); a prompt or a
// reply that matches is counted and left, since those are Claude Code's.

// Words that choose by what an item is, not by its name.
const (
	PruneStale   = "stale"   // out of date copies, deleted files, large and old
	PruneResults = "results" // every tool result from before the latest prompt
)

// pruneMinBytes: the note left in an item's place is about 150 bytes, so
// removing less than this saves nothing.
const pruneMinBytes = 512

// PruneResult is what a prune removed, and what matched but stays.
type PruneResult struct {
	Removed int      `json:"removed"`
	Tokens  int64    `json:"tokens"`
	Kept    int      `json:"kept"` // prompts and replies that match: not removable
	Names   []string `json:"names,omitempty"`
	Detail  string   `json:"detail"`
}

// pruneMatches picks the items what names: their indexes, and how many
// prompts and replies match and stay. A tool result matches by its call's
// name or anything in the call's input, so a file read by a relative path
// from a command that named the repository is still found.
func pruneMatches(items []ContextItem, what string) (hit []int, kept int) {
	what = strings.ToLower(strings.TrimSpace(what))
	words := strings.Fields(what)
	if len(words) == 0 {
		return nil, 0
	}
	has := func(s string) bool {
		s = strings.ToLower(s)
		for _, w := range words {
			if !strings.Contains(s, w) {
				return false
			}
		}
		return true
	}
	byCall := map[string]bool{} // a call's name -> its input matches
	for i, it := range items {
		if it.Group == grpReplies {
			if name, ok := strings.CutPrefix(it.Name, "Call: "); ok && has(name+" "+it.Full) {
				byCall[name] = true
			}
		}
		if !it.Removable {
			if (it.Group == grpPrompts || it.Group == grpReplies) && what != PruneStale && what != PruneResults && has(it.Name+" "+it.Full) {
				kept++
			}
			continue
		}
		if it.Removed || it.Bytes < pruneMinBytes {
			continue
		}
		switch what {
		case PruneStale:
			for _, f := range it.Flags {
				if strings.HasPrefix(f, "read again") || strings.HasPrefix(f, "file no longer exists") || strings.HasPrefix(f, "large and") {
					hit = append(hit, i)
					break
				}
			}
		case PruneResults:
			if it.Group == grpResults && it.TurnsAgo >= 1 {
				hit = append(hit, i)
			}
		default:
			if has(it.Name) || (it.Group == grpResults && byCall[it.Name]) {
				hit = append(hit, i)
			}
		}
	}
	return hit, kept
}

// PruneContext removes from session sid's context every removable item what
// names, from its next request on. Nil when no request of the session has
// been seen since the gateway started.
func (s *Server) PruneContext(sid, what string) (*PruneResult, error) {
	rep := s.InspectContext(sid)
	if rep == nil || (len(rep.Items) == 0 && rep.Since > 0) {
		s.WantHistory(sid, "its context is to be pruned, and nothing of it has been seen whole")
		return nil, nil
	}
	hit, kept := pruneMatches(rep.Items, what)
	res := &PruneResult{Kept: kept}
	first := -1
	for _, i := range hit {
		it := rep.Items[i]
		if err := s.removals.Add(RemovalKey(sid), ctxview.Removal{ID: it.ID, Group: it.Group, Name: it.Name, Bytes: it.Bytes}); err != nil {
			return nil, err
		}
		if first < 0 {
			first = i
		}
		res.Removed++
		res.Tokens += it.Tokens
		if len(res.Names) < 5 {
			res.Names = append(res.Names, it.Name)
		}
	}
	if res.Removed > 0 || rep.Since > 0 {
		s.WantHistory(sid, "its context was pruned, and a request that continues a thread leaves what the API holds as it is")
	}
	res.Detail = pruneDetail(rep, res, what, first)
	return res, nil
}

// pruneDetail says what happened in a line or two, for a toast.
func pruneDetail(rep *ContextReport, res *PruneResult, what string, first int) string {
	stay := ""
	if res.Kept > 0 {
		stay = fmt.Sprintf(" %d prompts and replies that mention it stay: those are Claude Code's, and /compact with an instruction drops them.", res.Kept)
	}
	if rep.Since > 0 {
		stay += fmt.Sprintf(" %d requests since %s were not looked at (the session sends only what is new): prune again after the next reply to catch those.", rep.Since, rep.At.Local().Format("15:04"))
	}
	if res.Removed == 0 {
		return fmt.Sprintf("Nothing to prune for %q: no tool result or instruction file in this session's context matches.%s", what, stay)
	}
	var after int64
	for _, it := range rep.Items[first:] {
		after += it.Tokens
	}
	return fmt.Sprintf("Pruned %d items for %q, about %s tokens, from the next request on. That request writes about %s tokens to the cache again, once. /burst-prune undo puts them back.%s",
		res.Removed, what, kTok(res.Tokens), kTok(after-res.Tokens), stay)
}

func kTok(n int64) string {
	if n < 1000 {
		return fmt.Sprint(n)
	}
	return fmt.Sprintf("%dk", (n+500)/1000)
}

// RestoreContext puts back everything removed from session sid's context,
// by Prune or by the inspector, and reports how many items that was.
func (s *Server) RestoreContext(sid string) (int, error) {
	key := RemovalKey(sid)
	n := 0
	for id := range s.removals.For(key) {
		if err := s.removals.Restore(key, id); err != nil {
			return n, err
		}
		n++
	}
	if n > 0 {
		s.WantHistory(sid, "what was pruned from its context is to go back, and a request that continues a thread leaves what the API holds as it is")
	}
	return n, nil
}

package admin

import (
	"sort"

	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
	"github.com/andrewbakercloudscale/claude-burst/internal/repo"
)

// Spend per repository: each session's repository comes from internal/repo,
// which the gateway's per-repository Compact at uses too.

const (
	unknownRepo = repo.Unknown
	tempRepo    = repo.Temp
)

type repoResolver struct{ *repo.Resolver }

func newRepoResolver() *repoResolver { return &repoResolver{repo.New()} }

func (r *repoResolver) resolve(session string) (name, root string) { return r.Resolve(session) }

// repoSpend groups per-session spend by repository, largest spend first.
// saved is compaction's net saving per session id over the same window.
func (r *repoResolver) repoSpend(sessions map[string]metrics.SessionUse, saved map[string]float64) []metrics.RepoUse {
	by := map[string]*metrics.RepoUse{}
	row := func(sid string) *metrics.RepoUse {
		name, root := r.resolve(sid)
		u := by[name]
		if u == nil {
			u = &metrics.RepoUse{Repo: name, Path: root}
			by[name] = u
		}
		return u
	}
	for sid, su := range sessions {
		u := row(sid)
		u.Sessions++
		u.Requests += su.Requests
		u.USD += su.USD
		u.Unpriced = u.Unpriced || su.Unpriced
	}
	for sid, usd := range saved {
		u := row(sid)
		u.SavedUSD += usd
		u.Compacted = true
	}
	out := make([]metrics.RepoUse, 0, len(by))
	for _, u := range by {
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].USD != out[j].USD {
			return out[i].USD > out[j].USD
		}
		return out[i].Repo < out[j].Repo
	})
	return out
}

// RepoSaving is what Pauseless Compaction saved one repository's sessions:
// the savings chart's money, split by repository instead of by day.
type RepoSaving struct {
	Repo        string  `json:"repo"`
	Path        string  `json:"path,omitempty"`
	Sessions    int     `json:"sessions"`
	Compactions int     `json:"compactions"`
	Requests    int     `json:"requests"`
	SavedTokens int64   `json:"saved_tokens"`
	SavedUSD    float64 `json:"saved_usd"`
	SummaryUSD  float64 `json:"summary_usd"`
	RewriteUSD  float64 `json:"rewrite_usd"`
	NetUSD      float64 `json:"net_usd"`
}

// savingsByRepo groups compacted sessions by repository, largest net
// saving first. Built from the same sessions as the chart's totals, so its
// rows add up to the chart's net.
func (r *repoResolver) savingsByRepo(sessions []metrics.CompactedSession) []RepoSaving {
	by := map[string]*RepoSaving{}
	for _, c := range sessions {
		name, root := r.resolve(c.Session)
		u := by[name]
		if u == nil {
			u = &RepoSaving{Repo: name, Path: root}
			by[name] = u
		}
		u.Sessions++
		u.Compactions += c.Compactions
		u.Requests += c.Requests
		u.SavedTokens += c.SavedTokens
		u.SavedUSD += c.SavedUSD
		u.SummaryUSD += c.SummaryUSD
		u.RewriteUSD += c.RewriteUSD
		u.NetUSD += c.NetUSD
	}
	out := make([]RepoSaving, 0, len(by))
	for _, u := range by {
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NetUSD != out[j].NetUSD {
			return out[i].NetUSD > out[j].NetUSD
		}
		return out[i].Repo < out[j].Repo
	})
	return out
}

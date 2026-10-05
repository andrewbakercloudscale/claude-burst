package metrics

import "time"

// Overflow's side of "what Burst saved": the requests the secondary served,
// priced twice. ListUSD is the same tokens at the price of the model Claude
// Code asked for, which is what they would have cost on Anthropic's API;
// PaidUSD is what the secondary charged. The difference is the saving, and
// it is negative when the secondary is the dearer one.
//
// Like every figure here it is API-equivalent: on a subscription those
// requests were not going to be billed at all, they were going to be
// refused at the limit. Only requests with both prices known are counted,
// so an unpriced model leaves the saving short, never invented.

type OverflowStats struct {
	// Requests is every answered request the secondary served in the window,
	// Priced the ones counted in the money.
	Requests int     `json:"requests"`
	Priced   int     `json:"priced"`
	ListUSD  float64 `json:"list_usd"`
	PaidUSD  float64 `json:"paid_usd"`
	SavedUSD float64 `json:"saved_usd"`
	// Daily is every local day of the window, oldest first.
	Daily []OverflowDay `json:"daily"`
}

type OverflowDay struct {
	Date     string  `json:"date"` // 2006-01-02, local
	Requests int     `json:"requests"`
	ListUSD  float64 `json:"list_usd"`
	PaidUSD  float64 `json:"paid_usd"`
	SavedUSD float64 `json:"saved_usd"`
}

// overflowPrices is one secondary request's two prices; ok is false when
// either is unknown or the request was served by the model it asked for.
func overflowPrices(e Event) (list, paid float64, ok bool) {
	if e.RequestedModel == "" || e.RequestedModel == e.Model || e.PricingUnknown {
		return 0, 0, false
	}
	pricerMu.RLock()
	p := pricer
	pricerMu.RUnlock()
	if p == nil {
		return 0, 0, false
	}
	list, ok = p(e.RequestedModel, e.InputTokens, e.OutputTokens, e.CacheReadTokens, e.CacheWriteTokens)
	return list, e.APIEquivalentUSD, ok
}

// OverflowSince is OverflowStats from since to now, at most 92 days.
func OverflowSince(path string, since time.Time) (OverflowStats, error) {
	var st OverflowStats
	now := time.Now()
	if limit := now.AddDate(0, 0, -92); since.Before(limit) {
		since = limit
	}
	index := map[string]int{}
	y, m, d := since.Local().Date()
	for day := time.Date(y, m, d, 0, 0, 0, 0, time.Local); !day.After(now); day = day.AddDate(0, 0, 1) {
		index[day.Format("2006-01-02")] = len(st.Daily)
		st.Daily = append(st.Daily, OverflowDay{Date: day.Format("2006-01-02")})
	}
	for _, f := range historyFiles(path, since) {
		err := scanEvents(f, func(e Event) {
			if e.Time.Before(since) || slotOf(e) != "secondary" || !ok(e) || e.InputTokens+e.OutputTokens+e.CacheReadTokens == 0 {
				return
			}
			i, known := index[e.Time.Local().Format("2006-01-02")]
			if !known {
				return
			}
			day := &st.Daily[i]
			st.Requests++
			day.Requests++
			list, paid, priced := overflowPrices(e)
			if !priced {
				return
			}
			st.Priced++
			st.ListUSD += list
			st.PaidUSD += paid
			st.SavedUSD += list - paid
			day.ListUSD += list
			day.PaidUSD += paid
			day.SavedUSD += list - paid
		})
		if err != nil {
			return st, err
		}
	}
	return st, nil
}

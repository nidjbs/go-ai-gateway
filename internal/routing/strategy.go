package routing

import (
	"sort"
	"time"

	"github.com/nidjbs/go-ai-gateway/internal/config"
)

// Strategy selects the order in which an alias's candidate providers are tried
// for one request. The first element of the ordered list is the primary target;
// the rest serve as fallbacks when it fails, exactly as retry/failover already
// consume the candidate list.
type Strategy string

const (
	// StrategyFallback tries candidates in configured priority order (default).
	StrategyFallback Strategy = "fallback"
	// StrategyLoadBalance picks a weighted-random primary each request.
	StrategyLoadBalance Strategy = "loadbalance"
	// StrategyLeastLatency tries the fastest observed provider first.
	StrategyLeastLatency Strategy = "least_latency"
)

// ParseStrategy normalizes a configured strategy string to a Strategy,
// defaulting to fallback.
func ParseStrategy(s string) Strategy {
	switch Strategy(s) {
	case StrategyLoadBalance:
		return StrategyLoadBalance
	case StrategyLeastLatency:
		return StrategyLeastLatency
	default:
		return StrategyFallback
	}
}

// StrategyFor returns the routing strategy declared for an alias. The alias
// must exist (Resolve has already validated it); an empty config falls back.
func StrategyFor(cfg *config.Config, alias string) Strategy {
	return ParseStrategy(cfg.Aliases[alias].Strategy)
}

// Order reorders candidates according to strategy. candidates is the
// provider.Filter output (already validated and de-duplicated); Order must not
// drop or duplicate entries, only permute them. tracker may be nil; strategies
// that need it degrade to priority order. randFn returns a value in [0, n) and
// is injectable for deterministic tests.
func Order(s Strategy, candidates []Candidate, tracker *LatencyTracker, randFn func(n int) int) []Candidate {
	if len(candidates) < 2 {
		return candidates
	}
	switch s {
	case StrategyLoadBalance:
		return orderLoadBalance(candidates, randFn)
	case StrategyLeastLatency:
		return orderLeastLatency(candidates, tracker)
	default:
		return candidates
	}
}

// orderLoadBalance picks one weighted-random primary and pins the rest in
// priority order behind it. All-zero weights (or none set) keep priority order
// so an unweighted alias behaves like the default fallback chain.
func orderLoadBalance(candidates []Candidate, randFn func(n int) int) []Candidate {
	total := 0
	for _, c := range candidates {
		total += c.Weight
	}
	if total <= 0 {
		return candidates
	}
	r := randFn(total)
	primary := 0
	for i, c := range candidates {
		r -= c.Weight
		if r < 0 {
			primary = i
			break
		}
	}
	rest := make([]Candidate, 0, len(candidates)-1)
	rest = append(rest, candidates[:primary]...)
	rest = append(rest, candidates[primary+1:]...)
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].Priority < rest[j].Priority })
	return append([]Candidate{candidates[primary]}, rest...)
}

// orderLeastLatency sorts observed candidates by EWMA latency (fastest first)
// and pins unobserved ones after them in priority order, so a provider with no
// history is only tried once every observed candidate has failed. Ties break by
// priority so the configured order still reads through.
func orderLeastLatency(candidates []Candidate, tracker *LatencyTracker) []Candidate {
	type entry struct {
		c   Candidate
		lat time.Duration
		obs bool
	}
	entries := make([]entry, 0, len(candidates))
	for _, c := range candidates {
		lat, obs := time.Duration(0), false
		if tracker != nil {
			lat, obs = tracker.EWMA(c)
		}
		entries = append(entries, entry{c: c, lat: lat, obs: obs})
	}
	// Stable sort by (observed desc, latency asc, priority asc).
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].obs != entries[j].obs {
			return entries[i].obs
		}
		if entries[i].lat != entries[j].lat {
			return entries[i].lat < entries[j].lat
		}
		return entries[i].c.Priority < entries[j].c.Priority
	})
	out := make([]Candidate, len(entries))
	for i, e := range entries {
		out[i] = e.c
	}
	return out
}

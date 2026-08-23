package usage

import (
	"context"
	"strings"
	"time"
)

// QueryFilter narrows a usage query; zero values mean no filter.
type QueryFilter struct {
	TeamID   string
	APIKeyID string
	Alias    string
	From     time.Time
	To       time.Time
}

// Summary aggregates usage events (DurationMS sums durations).
type Summary struct {
	Requests     int64 `json:"requests"`
	Successes    int64 `json:"successes"`
	Failures     int64 `json:"failures"`
	Streaming    int64 `json:"streaming"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
	CostMicros   int64 `json:"cost_micros"`
	DurationMS   int64 `json:"duration_ms"`
}

// Bucket is one time-bucket of a Series query.
type Bucket struct {
	Start   time.Time `json:"start"`
	Summary `json:",inline"`
}

// Queryer is the read-only usage query surface; sinks that cannot be
// queried (e.g. the stderr audit sink) simply don't implement it.
type Queryer interface {
	Summary(ctx context.Context, f QueryFilter) (Summary, error)
	// Series aggregates by hour or day bucket.
	Series(ctx context.Context, f QueryFilter, bucket time.Duration) ([]Bucket, error)
}

// buildWhere builds the WHERE clause and args; missing From defaults to
// 24h back, missing To to now.
func buildWhere(f QueryFilter, now time.Time) (string, []any) {
	from := f.From
	to := f.To
	if from.IsZero() {
		from = now.Add(-24 * time.Hour)
	}
	if to.IsZero() {
		to = now
	}
	var conds []string
	var args []any
	conds = append(conds, "started_at >= ?")
	args = append(args, from.UTC())
	conds = append(conds, "started_at < ?")
	args = append(args, to.UTC())
	if f.TeamID != "" {
		conds = append(conds, "team_id = ?")
		args = append(args, f.TeamID)
	}
	if f.APIKeyID != "" {
		conds = append(conds, "api_key_id = ?")
		args = append(args, f.APIKeyID)
	}
	if f.Alias != "" {
		conds = append(conds, "alias = ?")
		args = append(args, f.Alias)
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// summaryAggSQL is the aggregate SELECT shared by Summary and Series.
const summaryAggSQL = `COUNT(*),
	COALESCE(SUM(CASE WHEN success = 1 THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(streaming), 0),
	COALESCE(SUM(input_tokens), 0),
	COALESCE(SUM(output_tokens), 0),
	COALESCE(SUM(total_tokens), 0),
	COALESCE(SUM(total_cost_micros), 0),
	COALESCE(SUM(duration_ms), 0)`

func scanSummary(scan func(dest ...any) error) (Summary, error) {
	var s Summary
	err := scan(&s.Requests, &s.Successes, &s.Failures, &s.Streaming,
		&s.InputTokens, &s.OutputTokens, &s.TotalTokens, &s.CostMicros, &s.DurationMS)
	if err != nil {
		return Summary{}, err
	}
	return s, nil
}

func bucketFormat(bucket time.Duration) string {
	switch bucket {
	case time.Hour:
		return "%Y-%m-%dT%H:00:00Z"
	default:
		return "%Y-%m-%dT00:00:00Z"
	}
}

func bucketLayout(bucket time.Duration) string {
	switch bucket {
	case time.Hour:
		return "2006-01-02T15:04:05Z"
	default:
		return "2006-01-02T00:00:00Z"
	}
}

// normalizeBucket clamps the bucket to hour or day.
func normalizeBucket(bucket time.Duration) time.Duration {
	if bucket == time.Hour {
		return time.Hour
	}
	return 24 * time.Hour
}

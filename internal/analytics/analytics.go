// Package analytics defines the dashboard snapshot independently of HTTP DTOs.
package analytics

import (
	"time"

	"github.com/orpheus-agents/orpheus/internal/session"
)

const MaxCount int64 = 1<<53 - 1

type Request struct {
	Window    string
	From      *time.Time
	To        *time.Time
	Bucket    string
	Timezone  string
	Namespace *string
}

type Query struct {
	AsOf      time.Time
	From      time.Time
	To        time.Time
	Bucket    string
	Timezone  string
	Namespace *string
}

func Invalid(name string) error {
	p := session.Problem(422, "validation_error", "Invalid query parameter.")
	p.Problem.Details = []session.Detail{{Path: []any{"query", name}, Code: "invalid_value"}}
	return p
}

func (r Request) Resolve(asOf time.Time) (Query, error) {
	q := Query{AsOf: asOf.UTC(), Bucket: r.Bucket, Timezone: r.Timezone, Namespace: r.Namespace}
	if err := session.ValidateExternal(r.Namespace, session.NamespaceMaxBytes, "query", "namespace"); err != nil {
		return q, err
	}
	if q.Timezone == "" {
		q.Timezone = "UTC"
	}
	defaultBucket := "hour"
	if r.From != nil || r.To != nil {
		if r.Window != "" {
			return q, Invalid("window")
		}
		if r.From == nil {
			return q, Invalid("from")
		}
		if r.To == nil {
			return q, Invalid("to")
		}
		q.From, q.To = r.From.UTC(), r.To.UTC()
		if !q.From.Before(q.To) || q.To.Sub(q.From) > 31*24*time.Hour {
			return q, Invalid("from")
		}
	} else {
		q.To = q.AsOf
		switch r.Window {
		case "", "24h":
			q.From = q.To.Add(-24 * time.Hour)
		case "7d":
			q.From = q.To.Add(-7 * 24 * time.Hour)
		case "30d":
			q.From = q.To.Add(-30 * 24 * time.Hour)
		case "3m", "6m", "1y":
			months := map[string]int{"3m": 3, "6m": 6, "1y": 12}[r.Window]
			q.From = subtractMonths(q.To, months)
			defaultBucket = "day"
		default:
			return q, Invalid("window")
		}
	}
	if q.Bucket == "" {
		q.Bucket = defaultBucket
	}
	if q.Bucket != "day" && (q.Bucket != "hour" || defaultBucket == "day") {
		return q, Invalid("bucket")
	}
	if q.To.After(q.AsOf) {
		return q, Invalid("to")
	}
	return q, nil
}

func subtractMonths(t time.Time, months int) time.Time {
	first := t.AddDate(0, 0, 1-t.Day()).AddDate(0, -months, 0)
	lastDay := first.AddDate(0, 1, -1).Day()
	return first.AddDate(0, 0, min(t.Day(), lastDay)-1)
}

type StatusCounts struct {
	Accepted   int64 `json:"accepted"`
	Starting   int64 `json:"starting"`
	Running    int64 `json:"running"`
	Cancelling int64 `json:"cancelling"`
	Finalizing int64 `json:"finalizing"`
	Completed  int64 `json:"completed"`
	Failed     int64 `json:"failed"`
	Cancelled  int64 `json:"cancelled"`
}

func (s *StatusCounts) Add(v StatusCounts) {
	s.Accepted += v.Accepted
	s.Starting += v.Starting
	s.Running += v.Running
	s.Cancelling += v.Cancelling
	s.Finalizing += v.Finalizing
	s.Completed += v.Completed
	s.Failed += v.Failed
	s.Cancelled += v.Cancelled
}

type Usage struct {
	InputTokens           string `json:"input_tokens"`
	CachedInputTokens     string `json:"cached_input_tokens"`
	OutputTokens          string `json:"output_tokens"`
	ReasoningOutputTokens string `json:"reasoning_output_tokens"`
	TotalTokens           string `json:"total_tokens"`
}

type Bucket struct {
	From      time.Time    `json:"from"`
	To        time.Time    `json:"to"`
	RunsCount int64        `json:"runs_count"`
	ByStatus  StatusCounts `json:"by_status"`
}

// Namespace holds the period totals of runs whose session has this namespace.
type Namespace struct {
	Namespace      *string      `json:"namespace"`
	RunsCount      int64        `json:"runs_count"`
	ByStatus       StatusCounts `json:"by_status"`
	Usage          Usage        `json:"usage"`
	RuntimeSeconds float64      `json:"runtime_seconds"`
}

type Overview struct {
	AsOf      time.Time `json:"as_of"`
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	Bucket    string    `json:"bucket"`
	Timezone  string    `json:"timezone"`
	Namespace *string   `json:"namespace"`
	Current   struct {
		ActiveSessions int64 `json:"active_sessions"`
	} `json:"current"`
	Period struct {
		RunsCount      int64        `json:"runs_count"`
		ByStatus       StatusCounts `json:"by_status"`
		Usage          Usage        `json:"usage"`
		RuntimeSeconds float64      `json:"runtime_seconds"`
	} `json:"period"`
	Series     []Bucket    `json:"series"`
	Namespaces []Namespace `json:"namespaces"`
}

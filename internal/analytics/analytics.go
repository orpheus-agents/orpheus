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
	if q.Bucket == "" {
		q.Bucket = "hour"
	}
	if q.Bucket != "hour" && q.Bucket != "day" {
		return q, Invalid("bucket")
	}
	if q.Timezone == "" {
		q.Timezone = "UTC"
	}
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
	} else {
		q.To = q.AsOf
		switch r.Window {
		case "", "24h":
			q.From = q.To.Add(-24 * time.Hour)
		case "7d":
			q.From = q.To.Add(-7 * 24 * time.Hour)
		case "30d":
			q.From = q.To.Add(-30 * 24 * time.Hour)
		default:
			return q, Invalid("window")
		}
	}
	if !q.From.Before(q.To) || q.To.Sub(q.From) > 31*24*time.Hour {
		return q, Invalid("from")
	}
	if q.To.After(q.AsOf) {
		return q, Invalid("to")
	}
	return q, nil
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
	InputTokens  string `json:"input_tokens"`
	OutputTokens string `json:"output_tokens"`
	TotalTokens  string `json:"total_tokens"`
}

type Bucket struct {
	From      time.Time    `json:"from"`
	To        time.Time    `json:"to"`
	RunsCount int64        `json:"runs_count"`
	ByStatus  StatusCounts `json:"by_status"`
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
	Series []Bucket `json:"series"`
}

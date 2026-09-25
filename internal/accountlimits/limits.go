// Package accountlimits normalizes provider quota snapshots without depending on transport or storage.
package accountlimits

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"time"
)

var ErrNoData = errors.New("provider returned no rate limits")
var ErrInvalidResponse = errors.New("invalid provider rate limits response")
var ErrUnsupported = errors.New("provider does not support rate limit reads")
var ErrAuthenticationUnavailable = errors.New("provider account authentication is unavailable")

type Window struct {
	UsedPercent      float64    `json:"used_percent"`
	RemainingPercent float64    `json:"remaining_percent"`
	WindowMinutes    *int64     `json:"window_minutes"`
	ResetsAt         *time.Time `json:"resets_at"`
}
type Bucket struct {
	LimitID              string  `json:"limit_id"`
	LimitName            *string `json:"limit_name"`
	PlanType             *string `json:"plan_type"`
	RateLimitReachedType *string `json:"rate_limit_reached_type"`
	Primary              *Window `json:"primary"`
	Secondary            *Window `json:"secondary"`
}
type Snapshot struct {
	Buckets []Bucket `json:"buckets"`
}

// Parse reads a complete account/rateLimits/read result. Unknown provider fields
// are ignored; all known fields are checked before a sample can replace storage.
func Parse(raw []byte) (Snapshot, error) {
	var out Snapshot
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return out, ErrInvalidResponse
	}
	if single, present := fields["rateLimits"]; present && !bytes.Equal(bytes.TrimSpace(single), []byte("null")) {
		if _, err := parseBucket("", single); err != nil {
			return out, err
		}
	}
	if value, present := fields["rateLimitsByLimitId"]; present && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		var entries map[string]json.RawMessage
		if err := json.Unmarshal(value, &entries); err != nil || entries == nil {
			return out, ErrInvalidResponse
		}
		ids := make([]string, 0, len(entries))
		for id := range entries {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		out.Buckets = make([]Bucket, 0, len(ids))
		for _, id := range ids {
			if id == "" {
				return Snapshot{}, ErrInvalidResponse
			}
			bucket, err := parseBucket(id, entries[id])
			if err != nil {
				return Snapshot{}, err
			}
			out.Buckets = append(out.Buckets, bucket)
		}
		return out, nil
	}
	value := fields["rateLimits"]
	if len(value) == 0 || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return out, ErrNoData
	}
	bucket, err := parseBucket("", value)
	if err != nil {
		return out, err
	}
	if bucket.LimitID == "" {
		bucket.LimitID = "legacy"
	}
	out.Buckets = []Bucket{bucket}
	return out, nil
}

func parseBucket(id string, raw json.RawMessage) (Bucket, error) {
	raw = bytes.TrimSpace(raw)
	var wire struct {
		LimitID              *string         `json:"limitId"`
		LimitName            *string         `json:"limitName"`
		PlanType             *string         `json:"planType"`
		RateLimitReachedType *string         `json:"rateLimitReachedType"`
		Primary              json.RawMessage `json:"primary"`
		Secondary            json.RawMessage `json:"secondary"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil || len(raw) == 0 || raw[0] != '{' || (wire.LimitID != nil && id != "" && *wire.LimitID != id) {
		return Bucket{}, ErrInvalidResponse
	}
	if id == "" && wire.LimitID != nil {
		id = *wire.LimitID
	}
	primary, err := parseWindow(wire.Primary)
	if err != nil {
		return Bucket{}, err
	}
	secondary, err := parseWindow(wire.Secondary)
	if err != nil {
		return Bucket{}, err
	}
	return Bucket{LimitID: id, LimitName: wire.LimitName, PlanType: wire.PlanType, RateLimitReachedType: wire.RateLimitReachedType, Primary: primary, Secondary: secondary}, nil
}

func parseWindow(raw json.RawMessage) (*Window, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var wire struct {
		UsedPercent        *float64 `json:"usedPercent"`
		WindowDurationMins *int64   `json:"windowDurationMins"`
		ResetsAt           *int64   `json:"resetsAt"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil || raw[0] != '{' || wire.UsedPercent == nil || math.IsNaN(*wire.UsedPercent) || math.IsInf(*wire.UsedPercent, 0) || *wire.UsedPercent < 0 || (wire.WindowDurationMins != nil && *wire.WindowDurationMins <= 0) {
		return nil, ErrInvalidResponse
	}
	window := &Window{UsedPercent: *wire.UsedPercent, RemainingPercent: max(0, 100-*wire.UsedPercent), WindowMinutes: wire.WindowDurationMins}
	if wire.ResetsAt != nil {
		if *wire.ResetsAt < 0 || *wire.ResetsAt > 253402300799 {
			return nil, ErrInvalidResponse
		}
		window.ResetsAt = new(time.Unix(*wire.ResetsAt, 0).UTC())
	}
	return window, nil
}

func (s Snapshot) ResetElapsed(asOf time.Time) bool {
	for _, bucket := range s.Buckets {
		for _, window := range []*Window{bucket.Primary, bucket.Secondary} {
			if window != nil && window.ResetsAt != nil && !asOf.Before(*window.ResetsAt) {
				return true
			}
		}
	}
	return false
}

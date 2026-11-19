package accountlimits

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"
)

// ReadKimi reads the quota attached to a global Kimi Coding Plan key.
func ReadKimi(ctx context.Context, key string) (Snapshot, error) {
	return readKimi(ctx, &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, key)
}
func readKimi(ctx context.Context, client *http.Client, key string) (Snapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.kimi.ai/coding/v1/usages", nil)
	if err != nil {
		return Snapshot{}, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Orpheus")
	r, err := client.Do(req)
	if err != nil {
		return Snapshot{}, err
	}
	defer func() { _ = r.Body.Close() }()
	switch r.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return Snapshot{}, ErrAuthenticationUnavailable
	default:
		return Snapshot{}, fmt.Errorf("quota endpoint returned HTTP %d", r.StatusCode)
	}
	const maxResponse = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxResponse+1))
	if err != nil {
		return Snapshot{}, err
	}
	if len(raw) > maxResponse {
		return Snapshot{}, ErrInvalidResponse
	}
	return parseKimi(raw)
}
func parseKimi(raw []byte) (Snapshot, error) {
	var response struct {
		Usages map[string]struct {
			UsedRatio *float64   `json:"used_ratio"`
			ResetTime *time.Time `json:"reset_time"`
		} `json:"usages"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return Snapshot{}, ErrInvalidResponse
	}
	if len(response.Usages) == 0 {
		return Snapshot{}, ErrNoData
	}
	labels := map[string]string{"limit_5h": "5 hours", "limit_week": "Weekly", "limit_weekly": "Weekly", "limit_month_code": "Monthly coding", "limit_month_total": "Monthly total"}
	result := Snapshot{Buckets: make([]Bucket, 0, len(response.Usages))}
	for id, w := range response.Usages {
		if id == "" || w.UsedRatio == nil || *w.UsedRatio < 0 || *w.UsedRatio > math.MaxFloat64/100 || w.ResetTime == nil {
			return Snapshot{}, ErrInvalidResponse
		}
		used := *w.UsedRatio * 100
		b := Bucket{LimitID: id, Primary: &Window{UsedPercent: used, RemainingPercent: max(0, 100-used), ResetsAt: w.ResetTime}}
		if s, ok := labels[id]; ok {
			b.LimitName = new(s)
		}
		switch id {
		case "limit_5h":
			b.Primary.WindowMinutes = new(int64(300))
		case "limit_week", "limit_weekly":
			b.Primary.WindowMinutes = new(int64(10080))
		}
		result.Buckets = append(result.Buckets, b)
	}
	slices.SortFunc(result.Buckets, func(a, b Bucket) int { return strings.Compare(a.LimitID, b.LimitID) })
	return result, nil
}

// ParseConsole maps the Go allowance from the native usage API. A valid workspace
// can have no Go subscription, even when its prepaid models are available.
func ParseConsole(raw []byte) (Snapshot, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return Snapshot{}, ErrNoData
	}
	var data struct {
		Workspace struct {
			ID string `json:"id"`
		} `json:"workspace"`
		Go map[string]struct {
			Percent  *float64 `json:"percent"`
			ResetsAt *int64   `json:"resetsAt"`
		} `json:"go"`
	}
	if err := json.Unmarshal(raw, &data); err != nil || data.Workspace.ID == "" {
		return Snapshot{}, ErrInvalidResponse
	}
	out := Snapshot{Buckets: []Bucket{}}
	if data.Go == nil {
		return out, nil
	}
	for _, spec := range []struct {
		id, label string
		minutes   *int64
	}{
		{"rolling", "OpenCode Go · 5 hours", new(int64(300))}, {"weekly", "OpenCode Go · Weekly", new(int64(10080))}, {"monthly", "OpenCode Go · Monthly", nil},
	} {
		w := data.Go[spec.id]
		if w.Percent == nil || *w.Percent < 0 || *w.Percent > 100 || w.ResetsAt == nil || *w.ResetsAt <= 0 {
			return Snapshot{}, ErrInvalidResponse
		}
		reset := time.UnixMilli(*w.ResetsAt).UTC()
		out.Buckets = append(out.Buckets, Bucket{LimitID: spec.id, LimitName: new(spec.label), Primary: &Window{UsedPercent: *w.Percent, RemainingPercent: 100 - *w.Percent, WindowMinutes: spec.minutes, ResetsAt: &reset}})
	}
	return out, nil
}

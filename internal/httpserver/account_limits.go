package httpserver

import (
	"github.com/orpheus-agents/orpheus/internal/accountlimits"
	"github.com/orpheus-agents/orpheus/internal/api"
	"github.com/orpheus-agents/orpheus/internal/store"
)

func accountLimitsResponse(report store.LimitReport) api.GetAccountLimits200JSONResponse {
	out := api.GetAccountLimits200JSONResponse{
		Enabled:           report.Enabled,
		AsOf:              report.AsOf,
		StaleAfterSeconds: report.StaleAfterSeconds,
		Items:             make([]api.AccountLimitItem, 0, len(report.Items)),
	}
	for _, item := range report.Items {
		mapped := api.AccountLimitItem{
			AccountID:     item.AccountID,
			Profiles:      item.Profiles,
			State:         api.AccountLimitItemState(item.State),
			ObservedAt:    item.ObservedAt,
			LastAttemptAt: item.LastAttemptAt,
			Buckets:       make([]api.AccountLimitBucket, 0, len(item.Buckets)),
		}
		if item.ErrorCode != nil {
			mapped.ErrorCode = new(api.AccountLimitItemErrorCode(*item.ErrorCode))
		}
		for _, bucket := range item.Buckets {
			mapped.Buckets = append(mapped.Buckets, api.AccountLimitBucket{
				LimitID:              bucket.LimitID,
				LimitName:            bucket.LimitName,
				PlanType:             bucket.PlanType,
				RateLimitReachedType: bucket.RateLimitReachedType,
				Primary:              accountLimitWindow(bucket.Primary),
				Secondary:            accountLimitWindow(bucket.Secondary),
			})
		}
		out.Items = append(out.Items, mapped)
	}
	return out
}

func accountLimitWindow(window *accountlimits.Window) *api.AccountLimitWindow {
	if window == nil {
		return nil
	}
	mapped := &api.AccountLimitWindow{
		UsedPercent:      window.UsedPercent,
		RemainingPercent: window.RemainingPercent,
		ResetsAt:         window.ResetsAt,
	}
	if window.WindowMinutes != nil {
		mapped.WindowMinutes = new(int(*window.WindowMinutes))
	}
	return mapped
}

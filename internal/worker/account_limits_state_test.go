package worker

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAdvanceLimitReadState(t *testing.T) {
	now := time.Date(2026, time.September, 25, 12, 0, 0, 0, time.UTC)
	first := uuid.UUID{15: 1}
	second := uuid.UUID{15: 2}
	tests := []struct {
		name         string
		state        limitReadState
		donor        uuid.UUID
		value        limitObservation
		hasAlternate bool
		want         limitReadState
		wantFailover bool
	}{
		{
			name:         "first timeout switches donor",
			donor:        first,
			value:        limitObservation{code: new("temporarily_unavailable"), donorFailure: true},
			hasAlternate: true,
			want:         limitReadState{lastDonor: first, backendFailures: 1, lastBackendDonor: first},
			wantFailover: true,
		},
		{
			name:         "second invalid response pauses account",
			state:        limitReadState{lastDonor: first, backendFailures: 1, lastBackendDonor: first},
			donor:        second,
			value:        limitObservation{code: new("invalid_response"), donorFailure: true},
			hasAlternate: true,
			want:         limitReadState{lastDonor: second, lastBackendDonor: second, providerFailures: 1, providerRetryUntil: now.Add(time.Minute)},
		},
		{
			name:         "local unsupported does not count",
			state:        limitReadState{lastDonor: first, backendFailures: 1, lastBackendDonor: first},
			donor:        second,
			value:        limitObservation{code: new("unsupported"), donorFailure: true},
			hasAlternate: true,
			want:         limitReadState{lastDonor: second, backendFailures: 1, lastBackendDonor: first},
			wantFailover: true,
		},
		{
			name:  "provider error pauses account",
			state: limitReadState{lastDonor: first, backendFailures: 1, lastBackendDonor: first, providerFailures: 1},
			donor: second,
			value: limitObservation{code: new("no_data")},
			want:  limitReadState{lastDonor: first, lastBackendDonor: first, providerFailures: 2, providerRetryUntil: now.Add(2 * time.Minute)},
		},
		{
			name:  "success resets account failures",
			state: limitReadState{preferred: first, lastDonor: first, backendFailures: 1, lastBackendDonor: first, providerFailures: 2, providerRetryUntil: now.Add(2 * time.Minute)},
			donor: second,
			value: limitObservation{},
			want:  limitReadState{preferred: second, lastDonor: first, lastBackendDonor: first},
		},
		{
			name:  "last donor failure ends chain",
			donor: first,
			value: limitObservation{code: new("invalid_response"), donorFailure: true},
			want:  limitReadState{lastDonor: first, lastBackendDonor: first},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, failover := advanceLimitReadState(tc.state, tc.donor, tc.value, now, tc.hasAlternate)
			if got != tc.want || failover != tc.wantFailover {
				t.Fatalf("state = %+v, failover = %t; want %+v, %t", got, failover, tc.want, tc.wantFailover)
			}
		})
	}
}

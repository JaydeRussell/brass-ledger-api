package bcp

import (
	"testing"
	"time"
)

func TestEventInfoTTL(t *testing.T) {
	inDays := func(d float64) string {
		return time.Now().Add(time.Duration(d * float64(24*time.Hour))).Format(time.RFC3339)
	}

	cases := []struct {
		name string
		info EventInfo
		want time.Duration
	}{
		{
			name: "a concluded event never changes again",
			info: EventInfo{Ended: true, StartDate: inDays(-30)},
			want: eventInfoEndedTTL,
		},
		{
			name: "an event being played right now changes round by round",
			info: EventInfo{Started: true, StartDate: inDays(0)},
			want: minRefetchInterval,
		},
		{
			name: "starting well over a day out — nothing to watch yet",
			info: EventInfo{StartDate: inDays(30)},
			want: eventInfoFarOffTTL,
		},
		{
			name: "starting just over the threshold still counts as far off",
			info: EventInfo{StartDate: inDays(1.5)},
			want: eventInfoFarOffTTL,
		},
		{
			name: "starting within the day — about to flip to started",
			info: EventInfo{StartDate: inDays(0.25)},
			want: minRefetchInterval,
		},
		{
			name: "start date already passed but not flagged started",
			info: EventInfo{StartDate: inDays(-1)},
			want: minRefetchInterval,
		},
		{
			name: "no usable start date is treated as imminent, not far off",
			info: EventInfo{StartDate: ""},
			want: minRefetchInterval,
		},
		{
			name: "an unparseable start date is treated as imminent too",
			info: EventInfo{StartDate: "sometime next spring"},
			want: minRefetchInterval,
		},
		{
			name: "ended wins over a far-off start date",
			info: EventInfo{Ended: true, StartDate: inDays(30)},
			want: eventInfoEndedTTL,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := eventInfoTTL(tc.info); got != tc.want {
				t.Errorf("eventInfoTTL() = %s, want %s", got, tc.want)
			}
		})
	}
}

// The safety property the whole policy rests on: a long TTL must never
// let us keep serving "hasn't started" past the moment it starts.
func TestEventInfoTTL_NeverOutlivesTheStartItIsWaitingFor(t *testing.T) {
	if eventInfoFarOffTTL >= eventInfoFarOffThreshold {
		t.Fatalf("eventInfoFarOffTTL (%s) must be shorter than eventInfoFarOffThreshold (%s) — "+
			"otherwise an event cached as 'not started' could stay that way past its own start time",
			eventInfoFarOffTTL, eventInfoFarOffThreshold)
	}
}

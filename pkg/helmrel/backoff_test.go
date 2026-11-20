package helmrel_test

import (
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/helmrel"
)

func TestDelayDoublesAndCaps(t *testing.T) {
	max := 30 * time.Minute
	for attempt, want := range map[int]time.Duration{
		1: time.Minute, 2: 2 * time.Minute, 3: 4 * time.Minute, 4: 8 * time.Minute, 5: 16 * time.Minute,
		6: 30 * time.Minute, 7: 30 * time.Minute, 100: 30 * time.Minute, 1 << 30: 30 * time.Minute,
	} {
		if got := helmrel.Delay(attempt, max, 0); got != want {
			t.Errorf("attempt %d: %s, want %s", attempt, got, want)
		}
	}
}

func TestDelayAtLeastOneMinuteForNonsense(t *testing.T) {
	for _, attempt := range []int{0, -3} {
		if got := helmrel.Delay(attempt, time.Hour, 0); got != time.Minute {
			t.Errorf("attempt %d: %s", attempt, got)
		}
	}
}

func TestDelayJitterStaysWithinABandAndUnderTheCap(t *testing.T) {
	max := 30 * time.Minute
	for _, j := range []float64{0, 0.25, 0.5, 0.999, 1} {
		got := helmrel.Delay(3, max, j)
		if got < 4*time.Minute || got > 5*time.Minute {
			t.Errorf("jitter %v: %s outside 4m..5m", j, got)
		}
	}
	if got := helmrel.Delay(50, max, 1); got != max {
		t.Errorf("jitter must not push past the cap: %s", got)
	}
	if got := helmrel.Delay(3, max, -1); got != 4*time.Minute {
		t.Errorf("negative jitter: %s", got)
	}
}

func TestDelayCapBelowOneMinute(t *testing.T) {
	if got := helmrel.Delay(1, 10*time.Second, 0.5); got != 10*time.Second {
		t.Errorf("%s", got)
	}
}

package maintenance_test

import (
	"testing"
	"time"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/maintenance"
)

const (
	mon = cloudv1.Weekday_WEEKDAY_MONDAY
	sat = cloudv1.Weekday_WEEKDAY_SATURDAY
	sun = cloudv1.Weekday_WEEKDAY_SUNDAY
)

// at is a time in the week of Monday 2026-11-16 (UTC): at(sat, "23:30").
func at(day cloudv1.Weekday, hhmm string) time.Time {
	clock, err := time.Parse("15:04", hhmm)
	if err != nil {
		panic(err)
	}
	monday := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	return monday.AddDate(0, 0, int(day)-1).Add(time.Duration(clock.Hour())*time.Hour + time.Duration(clock.Minute())*time.Minute)
}

func window(start, duration string, days ...cloudv1.Weekday) *cloudv1.MaintenanceWindow {
	return &cloudv1.MaintenanceWindow{Days: days, Start: start, Duration: duration}
}

func TestOpenAndNext(t *testing.T) {
	weekend := window("02:00", "4h", sat, sun)
	lateSat := window("23:00", "4h", sat)
	daily := window("22:00", "3h")
	for name, tt := range map[string]struct {
		w        *cloudv1.MaintenanceWindow
		now      time.Time
		open     bool
		nextOpen time.Time
	}{
		"no window: always open":         {w: nil, now: at(mon, "12:00"), open: true, nextOpen: at(mon, "12:00")},
		"inside":                         {w: weekend, now: at(sat, "03:00"), open: true, nextOpen: at(sat, "03:00")},
		"at the start":                   {w: weekend, now: at(sat, "02:00"), open: true, nextOpen: at(sat, "02:00")},
		"at the end: closed":             {w: weekend, now: at(sat, "06:00"), nextOpen: at(sun, "02:00")},
		"before, same day":               {w: weekend, now: at(sat, "01:59"), nextOpen: at(sat, "02:00")},
		"weekday: next is saturday":      {w: weekend, now: at(mon, "09:00"), nextOpen: at(sat, "02:00")},
		"after sunday: wraps the week":   {w: weekend, now: at(sun, "07:00"), nextOpen: at(sat, "02:00").AddDate(0, 0, 7)},
		"past midnight: open on sunday":  {w: lateSat, now: at(sun, "01:00"), open: true, nextOpen: at(sun, "01:00")},
		"past midnight: closes at 03:00": {w: lateSat, now: at(sun, "03:00"), nextOpen: at(sat, "23:00").AddDate(0, 0, 7)},
		"saturday's window, not friday":  {w: lateSat, now: at(sat, "01:00"), nextOpen: at(sat, "23:00")},
		"every day, before":              {w: daily, now: at(mon, "21:00"), nextOpen: at(mon, "22:00")},
		"every day, past midnight":       {w: daily, now: at(sun, "00:30"), open: true, nextOpen: at(sun, "00:30")},
		"every day, after":               {w: daily, now: at(mon, "01:00"), nextOpen: at(mon, "22:00")},
		"sunday night into monday":       {w: window("23:00", "2h", sun), now: at(mon, "00:15").AddDate(0, 0, 7), open: true, nextOpen: at(mon, "00:15").AddDate(0, 0, 7)},
		"whole day":                      {w: window("00:00", "24h", mon), now: at(mon, "23:59"), open: true, nextOpen: at(mon, "23:59")},
		"another zone is read as UTC":    {w: weekend, now: at(sat, "03:00").In(time.FixedZone("UTC+9", 9*3600)), open: true, nextOpen: at(sat, "03:00")},
	} {
		if got := maintenance.Open(tt.w, tt.now); got != tt.open {
			t.Errorf("%s: Open = %t, want %t", name, got, tt.open)
		}
		if got := maintenance.Next(tt.w, tt.now); !got.Equal(tt.nextOpen) {
			t.Errorf("%s: Next = %s, want %s", name, got.UTC().Format(time.RFC1123), tt.nextOpen.UTC().Format(time.RFC1123))
		}
	}
}

// A window the API would refuse (a stored row from before a rule, an
// object edited by hand) never opens: an upgrade waits rather than start
// at a time nobody chose.
func TestUnreadableWindowNeverOpens(t *testing.T) {
	for _, w := range []*cloudv1.MaintenanceWindow{window("2am", "4h"), window("02:00", "forever"), window("02:00", "")} {
		if maintenance.Open(w, at(sat, "03:00")) || !maintenance.Next(w, at(sat, "03:00")).IsZero() {
			t.Errorf("window %v opens", w)
		}
	}
}

func TestDescribe(t *testing.T) {
	for want, w := range map[string]*cloudv1.MaintenanceWindow{
		"any time":                    nil,
		"Sat,Sun 02:00 UTC for 4h":    window("02:00", "4h", sat, sun),
		"every day 22:00 UTC for 90m": window("22:00", "90m"),
		"Mon 00:00 UTC for 1h30m":     window("00:00", "1h30m", mon),
	} {
		if got := maintenance.Describe(w); got != want {
			t.Errorf("Describe(%v) = %q, want %q", w, got, want)
		}
	}
}

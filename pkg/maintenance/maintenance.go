// Package maintenance reads a cluster's maintenance window: the weekly
// time, in UTC, when an automatic upgrade of it may start. Only an
// upgrade's start must fall inside the window; a started upgrade runs to
// its end, which is the caller's to respect.
package maintenance

import (
	"strings"
	"time"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// week is how far ahead Next looks: every window opens within a week.
const week = 7 * 24 * time.Hour

// Open reports whether an upgrade may start at now: there is no window,
// or now is inside one of its openings. An opening that began the day
// before still counts (Saturday 23:00 for 4h is open on Sunday at 01:00).
// A window that cannot be read (see parse) is never open.
func Open(w *cloudv1.MaintenanceWindow, now time.Time) bool {
	if w == nil {
		return true
	}
	start, length, ok := parse(w)
	if !ok {
		return false
	}
	now = now.UTC()
	// The opening that may hold now began today or, at most 24h ago, yesterday.
	for _, day := range []time.Time{midnight(now), midnight(now).AddDate(0, 0, -1)} {
		if !opensOn(w, day) {
			continue
		}
		from := day.Add(start)
		if !now.Before(from) && now.Before(from.Add(length)) {
			return true
		}
	}
	return false
}

// Next is the first time from now on when an upgrade may start: now when
// the window is open (or there is none), else the start of its next
// opening. The zero time for a window that cannot be read: it never opens.
func Next(w *cloudv1.MaintenanceWindow, now time.Time) time.Time {
	if Open(w, now) {
		return now
	}
	start, _, ok := parse(w)
	if !ok {
		return time.Time{}
	}
	now = now.UTC()
	for day := midnight(now); day.Before(now.Add(week + 24*time.Hour)); day = day.AddDate(0, 0, 1) {
		if from := day.Add(start); opensOn(w, day) && from.After(now) {
			return from
		}
	}
	return time.Time{}
}

// Describe is the window as a person reads it: "Sat,Sun 02:00 UTC for
// 4h", "every day 22:00 UTC for 90m", or "any time" without one.
func Describe(w *cloudv1.MaintenanceWindow) string {
	if w == nil {
		return "any time"
	}
	days := "every day"
	if len(w.GetDays()) > 0 {
		names := make([]string, 0, len(w.GetDays()))
		for _, d := range w.GetDays() {
			names = append(names, weekday(d).String()[:3])
		}
		days = strings.Join(names, ",")
	}
	return days + " " + w.GetStart() + " UTC for " + w.GetDuration()
}

// parse is the window's start as an offset from midnight and its length.
// ok is false for a window the API refuses (start not HH:MM, a length
// outside 1h to 24h): one stored before a rule, or edited by hand.
func parse(w *cloudv1.MaintenanceWindow) (start, length time.Duration, ok bool) {
	clock, err := time.Parse("15:04", w.GetStart())
	if err != nil {
		return 0, 0, false
	}
	length, err = time.ParseDuration(w.GetDuration())
	if err != nil || length < time.Hour || length > 24*time.Hour {
		return 0, 0, false
	}
	return time.Duration(clock.Hour())*time.Hour + time.Duration(clock.Minute())*time.Minute, length, true
}

// opensOn reports whether the window has an opening that starts on day.
func opensOn(w *cloudv1.MaintenanceWindow, day time.Time) bool {
	if len(w.GetDays()) == 0 {
		return true
	}
	for _, d := range w.GetDays() {
		if weekday(d) == day.Weekday() {
			return true
		}
	}
	return false
}

// weekday is the contract's day as Go's: the contract counts from Monday
// (1) to Sunday (7), Go from Sunday (0).
func weekday(d cloudv1.Weekday) time.Weekday { return time.Weekday(int(d) % 7) }

func midnight(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

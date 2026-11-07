package ui

import (
	"fmt"
	"strings"
	"time"
)

// timeView is a moment as pages show it: relative text, with the absolute
// UTC time on hover.
type timeView struct {
	// Text is for example "2 min ago". It is empty for a zero time.
	Text string
	// UTC is the absolute time, to the second.
	UTC string
	// ISO is the machine-readable form for the time element.
	ISO string
}

// relativeTime describes at as a distance from now.
func relativeTime(now, at time.Time) timeView {
	if at.IsZero() {
		return timeView{}
	}

	view := timeView{UTC: at.UTC().Format("2006-01-02 15:04:05") + " UTC", ISO: at.UTC().Format(time.RFC3339)}
	age := now.Sub(at)

	switch {
	case age < 0:
		// A clock that differs between replicas can put a time in the future.
		view.Text = "just now"
	case age < time.Minute:
		view.Text = "just now"
	case age < time.Hour:
		view.Text = fmt.Sprintf("%d min ago", int(age/time.Minute))
	case age < 24*time.Hour:
		view.Text = fmt.Sprintf("%d h ago", int(age/time.Hour))
	default:
		view.Text = fmt.Sprintf("%d d ago", int(age/(24*time.Hour)))
	}

	return view
}

// timeSince is relativeTime against the clock, for templates.
func timeSince(at time.Time) timeView {
	return relativeTime(time.Now(), at)
}

// outcomeView is an outcome word with the colour class of its badge.
type outcomeView struct {
	Label string
	Class string
}

// outcomeBadge maps an outcome of a tool call or an approval to its badge.
// Words it doesn't know keep their text and get the neutral class.
func outcomeBadge(outcome string) outcomeView {
	label := strings.ToLower(strings.TrimSpace(outcome))

	switch label {
	case "allow":
		label = "ok"
	case "deny":
		label = "denied"
	}

	class := "badge-neutral"

	switch label {
	case "ok", "approved":
		class = "badge-ok"
	case "pending":
		class = "badge-warn"
	case "refused", "error", "denied":
		class = "badge-bad"
	}

	return outcomeView{Label: label, Class: class}
}

// relativeFuture describes at, a moment expected after now such as an
// expiry, as a distance from now: "in 5 min". A moment already past reads
// "now".
func relativeFuture(now, at time.Time) timeView {
	if at.IsZero() {
		return timeView{}
	}

	view := timeView{UTC: at.UTC().Format("2006-01-02 15:04:05") + " UTC", ISO: at.UTC().Format(time.RFC3339)}
	left := at.Sub(now)

	switch {
	case left < time.Minute:
		view.Text = "now"
	case left < time.Hour:
		view.Text = fmt.Sprintf("in %d min", int(left/time.Minute))
	case left < 24*time.Hour:
		view.Text = fmt.Sprintf("in %d h", int(left/time.Hour))
	default:
		view.Text = fmt.Sprintf("in %d d", int(left/(24*time.Hour)))
	}

	return view
}

// timeUntil is relativeFuture against the clock, for templates.
func timeUntil(at time.Time) timeView {
	return relativeFuture(time.Now(), at)
}

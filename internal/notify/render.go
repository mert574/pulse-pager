package notify

import (
	"fmt"
	"strings"
	"time"
)

// humanTimeFormat is the human-readable timestamp layout, always shown in UTC
// with the "UTC" suffix so there is no ambiguity (PRD 12.7 closing note).
const humanTimeLayout = "2006-01-02 15:04:05"

// humanTime renders t in UTC with the "UTC" suffix.
func humanTime(t time.Time) string {
	return t.UTC().Format(humanTimeLayout) + " UTC"
}

// humanDuration renders a duration like "10m 0s" or "1h 5m 0s". Negative or zero
// durations render as "0s".
func humanDuration(seconds int) string {
	if seconds <= 0 {
		return "0s"
	}
	d := time.Duration(seconds) * time.Second
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60

	var parts []string
	if h > 0 {
		parts = append(parts, fmt.Sprintf("%dh", h))
		parts = append(parts, fmt.Sprintf("%dm", m))
		parts = append(parts, fmt.Sprintf("%ds", s))
	} else if m > 0 {
		parts = append(parts, fmt.Sprintf("%dm", m))
		parts = append(parts, fmt.Sprintf("%ds", s))
	} else {
		parts = append(parts, fmt.Sprintf("%ds", s))
	}
	return strings.Join(parts, " ")
}

// reasonLine builds the "Reason: <reason> (HTTP <status>)" line. When the status
// code is nil (timeout, connection error) it drops the "(HTTP n)" part.
func reasonLine(ev Event) string {
	reason := ""
	if ev.Check.FailureReason != nil {
		reason = string(*ev.Check.FailureReason)
	}
	if ev.Check.StatusCode != nil {
		return fmt.Sprintf("Reason: %s (HTTP %d)", reason, *ev.Check.StatusCode)
	}
	return fmt.Sprintf("Reason: %s", reason)
}

// downText builds the body of a down message with the given bold-title markup.
// title is the already-formatted heading, for example ":red_circle: *DOWN*" or
// "**DOWN**".
func downText(title string, ev Event) string {
	return fmt.Sprintf("%s %s\n%s\n%s\nDown since %s",
		title,
		ev.Monitor.Name,
		ev.Monitor.URL,
		reasonLine(ev),
		humanTime(ev.Incident.StartedAt),
	)
}

// recoveryText builds the body of a recovery message.
func recoveryText(title string, ev Event) string {
	dur := 0
	if ev.DurationSeconds != nil {
		dur = *ev.DurationSeconds
	}
	return fmt.Sprintf("%s %s\n%s\nWas down for %s (since %s)",
		title,
		ev.Monitor.Name,
		ev.Monitor.URL,
		humanDuration(dur),
		humanTime(ev.Incident.StartedAt),
	)
}

// summaryTitle is a short one-line title for an event, used by the integration
// providers (PagerDuty/Opsgenie/Telegram/Teams/SMS) where a plain title reads
// better than the emoji-decorated chat title.
func summaryTitle(ev Event) string {
	if ev.EventType == EventRecovery {
		return fmt.Sprintf("RECOVERED: %s", ev.Monitor.Name)
	}
	return fmt.Sprintf("DOWN: %s", ev.Monitor.Name)
}

// plainBody builds a plain-text multi-line body for the integration providers. It
// reuses the same facts as the chat/email renderers (url, reason/status, when it
// went down, and the duration on recovery).
func plainBody(ev Event) string {
	var parts []string
	parts = append(parts, ev.Monitor.URL)
	if ev.EventType == EventRecovery {
		dur := 0
		if ev.DurationSeconds != nil {
			dur = *ev.DurationSeconds
		}
		parts = append(parts, fmt.Sprintf("Was down for %s (since %s)", humanDuration(dur), humanTime(ev.Incident.StartedAt)))
		return strings.Join(parts, "\n")
	}
	parts = append(parts, reasonLine(ev))
	if ev.Check.LatencyMs != nil {
		parts = append(parts, fmt.Sprintf("Latency: %dms", *ev.Check.LatencyMs))
	}
	parts = append(parts, fmt.Sprintf("Down since %s", humanTime(ev.Incident.StartedAt)))
	return strings.Join(parts, "\n")
}

// slackText renders the Slack message for an event.
func slackText(ev Event) string {
	if ev.EventType == EventRecovery {
		return recoveryText(":large_green_circle: *RECOVERED*", ev)
	}
	return downText(":red_circle: *DOWN*", ev)
}

// discordText renders the Discord message for an event.
func discordText(ev Event) string {
	if ev.EventType == EventRecovery {
		return recoveryText("**RECOVERED**", ev)
	}
	return downText("**DOWN**", ev)
}

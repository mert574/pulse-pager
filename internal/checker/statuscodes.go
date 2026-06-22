package checker

import (
	"fmt"
	"strconv"
	"strings"
)

// StatusMatcher decides whether an HTTP status code counts as healthy.
// It is parsed once from a monitor's expected_status_codes spec and reused.
type StatusMatcher interface {
	Matches(code int) bool
}

// statusMatcher holds the parsed spec: a set of explicit codes plus a set of
// hundreds ranges (2 for 2xx, 3 for 3xx, and so on).
type statusMatcher struct {
	explicit map[int]bool
	ranges   map[int]bool // keyed by the leading digit, e.g. 2 means 2xx
}

func (m *statusMatcher) Matches(code int) bool {
	if m.explicit[code] {
		return true
	}
	return m.ranges[code/100]
}

// ParseStatusCodes parses a comma separated spec of explicit codes (each in
// 100..599) and the shorthands 2xx, 3xx, 4xx, 5xx into a StatusMatcher.
// An empty or otherwise invalid spec is an error. This is also used by the api
// package to validate the field on monitor create and update, so it must be
// strict and not panic on bad input.
func ParseStatusCodes(spec string) (StatusMatcher, error) {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		return nil, fmt.Errorf("expected_status_codes must not be empty")
	}

	m := &statusMatcher{
		explicit: make(map[int]bool),
		ranges:   make(map[int]bool),
	}

	parts := strings.Split(trimmed, ",")
	for _, raw := range parts {
		part := strings.TrimSpace(raw)
		if part == "" {
			return nil, fmt.Errorf("empty entry in expected_status_codes %q", spec)
		}

		// Shorthand like 2xx, 3xx, 4xx, 5xx. Lowercase so 2XX is accepted too.
		lower := strings.ToLower(part)
		if len(lower) == 3 && lower[1] == 'x' && lower[2] == 'x' {
			d := int(lower[0] - '0')
			if d < 1 || d > 5 {
				return nil, fmt.Errorf("invalid status range %q", part)
			}
			m.ranges[d] = true
			continue
		}

		// Explicit code.
		code, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("invalid status code %q", part)
		}
		if code < 100 || code > 599 {
			return nil, fmt.Errorf("status code %d out of range 100..599", code)
		}
		m.explicit[code] = true
	}

	return m, nil
}

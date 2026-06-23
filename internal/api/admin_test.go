package api

import "testing"

// The admin allowlist is the only gate on the platform panel, so its matching has
// to be case-insensitive and closed by default. New lowercases the configured
// emails; isPlatformAdmin lowercases the candidate, so both sides normalize.
func TestIsPlatformAdmin(t *testing.T) {
	s := New(Config{PlatformAdmins: []string{"Owner@Example.com", "ops@example.com"}})

	cases := []struct {
		email string
		want  bool
	}{
		{"owner@example.com", true}, // exact, lowercased on both sides
		{"OWNER@EXAMPLE.COM", true}, // candidate upper-cased
		{" ops@example.com ", true}, // trimmed
		{"someone@example.com", false},
		{"", false},
	}
	for _, c := range cases {
		if got := s.isPlatformAdmin(c.email); got != c.want {
			t.Errorf("isPlatformAdmin(%q) = %v, want %v", c.email, got, c.want)
		}
	}
}

// An empty allowlist means nobody is an admin, so the panel is closed by default.
func TestIsPlatformAdmin_EmptyAllowlistClosed(t *testing.T) {
	s := New(Config{})
	if s.isPlatformAdmin("anyone@example.com") {
		t.Error("empty allowlist should admit no one")
	}
}

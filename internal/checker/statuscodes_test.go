package checker

import "testing"

func TestParseStatusCodes_Valid(t *testing.T) {
	cases := []struct {
		spec    string
		match   []int
		noMatch []int
	}{
		{"200", []int{200}, []int{201, 204, 500}},
		{"200,204", []int{200, 204}, []int{201, 500}},
		{"2xx", []int{200, 201, 250, 299}, []int{199, 300, 404}},
		{"2xx,301", []int{200, 299, 301}, []int{300, 302, 400}},
		{"4xx,5xx", []int{400, 404, 500, 599}, []int{200, 301, 399}},
		{" 200 , 301 ", []int{200, 301}, []int{302}},
		{"2XX", []int{200, 250}, []int{300}}, // uppercase shorthand accepted
		{"100,599", []int{100, 599}, []int{99, 600}},
	}

	for _, tc := range cases {
		m, err := ParseStatusCodes(tc.spec)
		if err != nil {
			t.Errorf("spec %q: unexpected error %v", tc.spec, err)
			continue
		}
		for _, code := range tc.match {
			if !m.Matches(code) {
				t.Errorf("spec %q: expected %d to match", tc.spec, code)
			}
		}
		for _, code := range tc.noMatch {
			if m.Matches(code) {
				t.Errorf("spec %q: expected %d not to match", tc.spec, code)
			}
		}
	}
}

func TestParseStatusCodes_Invalid(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"abc",
		"999999",
		"99",   // below 100
		"600",  // above 599
		"6xx",  // out of shorthand range
		"0xx",  // out of shorthand range
		"200,", // trailing empty entry
		",200", // leading empty entry
		"200,,204",
		"2x",   // malformed shorthand
		"2xxx", // too long
		"-1",
	}

	for _, spec := range bad {
		if _, err := ParseStatusCodes(spec); err == nil {
			t.Errorf("spec %q: expected error, got none", spec)
		}
	}
}

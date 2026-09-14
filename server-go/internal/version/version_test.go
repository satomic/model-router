package version

import "testing"

func TestIsNewer(t *testing.T) {
	cases := []struct {
		candidate, current string
		want               bool
	}{
		{"v1.6.0", "1.5.0", true},
		{"1.5.1", "1.5.0", true},
		{"1.5.0", "1.5.0", false},
		{"1.4.9", "1.5.0", false},
		// Shorter tuples are padded, so 1.5 and 1.5.0 compare equal rather than 1.5 reading as older.
		{"1.5", "1.5.0", false},
		{"1.6", "1.5.9", true},
		// A pre-release of a version we already run is not an upgrade.
		{"1.5.0-rc1", "1.5.0", false},
		{"1.6.0-rc1", "1.5.0", true},
		// An unparseable tag is never newer: offering an upgrade to something we cannot read
		// would send the user to a page that may not be a release at all.
		{"latest", "1.5.0", false},
		{"", "1.5.0", false},
	}
	for _, c := range cases {
		if got := IsNewer(c.candidate, c.current); got != c.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", c.candidate, c.current, got, c.want)
		}
	}
}

func TestParseStopsAtTheFirstSuffixedSegment(t *testing.T) {
	got := Parse("v2.3.4-beta.7")
	want := []int{2, 3, 4}
	if len(got) != len(want) {
		t.Fatalf("Parse = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Parse = %v, want %v", got, want)
		}
	}
	if Parse("nope") != nil {
		t.Errorf("an unparseable tag must read as nil, got %v", Parse("nope"))
	}
}

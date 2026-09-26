// Package version carries the build's own identity and the project links the console offers.
package version

import "strings"

const Version = "2.2.0"

const (
	RepoURL     = "https://github.com/satomic/model-router"
	IssuesURL   = "https://github.com/satomic/model-router/issues/new"
	ReleasesURL = "https://github.com/satomic/model-router/releases/latest"
	ReleasesAPI = "https://api.github.com/repos/satomic/model-router/releases/latest"
)

// Parse turns "v1.2.3" or "1.2" into a comparable slice, or nil when it is not a version.
//
// Only the leading numeric dotted run is read, so "1.2.3-rc1" compares as 1.2.3 and a
// pre-release of a version we already run does not read as an upgrade. Returning nil rather
// than a zero slice matters: an unparseable tag must be ignored, not treated as very old.
func Parse(text string) []int {
	if text == "" {
		return nil
	}
	body := strings.TrimLeft(strings.TrimSpace(text), "vV")
	var parts []int
	for _, chunk := range strings.Split(body, ".") {
		digits := ""
		for _, ch := range chunk {
			if ch < '0' || ch > '9' {
				break
			}
			digits += string(ch)
		}
		if digits == "" {
			break
		}
		value := 0
		for _, ch := range digits {
			value = value*10 + int(ch-'0')
		}
		parts = append(parts, value)
		if len(digits) != len(chunk) {
			// Stop at the first suffixed segment: "3-rc1" ends the numeric run.
			break
		}
	}
	return parts
}

// IsNewer reports whether candidate is a strictly higher version than current.
//
// Shorter slices are padded, so 1.2 and 1.2.0 compare equal rather than 1.2 reading as
// older. An unparseable candidate is never newer: offering an upgrade to a tag we cannot
// understand would send the user to a page that may not be a release at all.
func IsNewer(candidate, current string) bool {
	a, b := Parse(candidate), Parse(current)
	if a == nil || b == nil {
		return false
	}
	width := len(a)
	if len(b) > width {
		width = len(b)
	}
	at := func(v []int, i int) int {
		if i < len(v) {
			return v[i]
		}
		return 0
	}
	for i := 0; i < width; i++ {
		if at(a, i) != at(b, i) {
			return at(a, i) > at(b, i)
		}
	}
	return false
}

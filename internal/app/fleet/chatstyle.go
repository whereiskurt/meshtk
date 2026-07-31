package fleet

import (
	"regexp"
	"strings"
)

// dashRun matches an em or en dash together with any whitespace hugging it.
var dashRun = regexp.MustCompile(`\s*[\x{2014}\x{2013}]\s*`)

// normalizeDashes replaces em and en dashes with a comma and a space. The style
// preamble already forbids them, so this is the belt to that suspenders: a
// reply opening "Yo—Condor here" is the single most bot-looking thing we ship,
// and it costs nothing to make it unrepresentable.
func normalizeDashes(s string) string {
	s = strings.TrimSpace(dashRun.ReplaceAllString(s, ", "))
	s = strings.TrimPrefix(s, ", ")
	return strings.TrimRight(s, ", ")
}

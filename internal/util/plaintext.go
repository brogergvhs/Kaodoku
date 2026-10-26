package util

import (
	"html"
	"regexp"
	"strings"
)

var (
	tagPattern   = regexp.MustCompile(`<[^>]*>`)
	brPattern    = regexp.MustCompile(`(?i)<br\s*/?>`)
	manyNewlines = regexp.MustCompile(`\n{3,}`)
)

func PlainDescription(s string) string {
	s = brPattern.ReplaceAllString(s, "\n")
	s = html.UnescapeString(tagPattern.ReplaceAllString(s, ""))
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	s = manyNewlines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	return strings.TrimSpace(s)
}

func PlainSnippet(s string, max int) string {
	s = html.UnescapeString(tagPattern.ReplaceAllString(s, " "))
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	cut := string(runes[:max])
	if i := strings.LastIndexByte(cut, ' '); i > 0 {
		cut = cut[:i]
	}
	return cut + "..."
}

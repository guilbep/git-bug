package core

import (
	"regexp"
	"strings"
)

// a @mention, not preceded by what would make it part of an email, a URL or a
// path, and not ending with punctuation
var mentionRe = regexp.MustCompile(`(^|[^\w.\-/@` + "`" + `])(@[A-Za-z0-9_](?:[A-Za-z0-9_.\-]*[A-Za-z0-9_])?)`)

// a mention defused by DefuseMentions
var defusedMentionRe = regexp.MustCompile("`(@[A-Za-z0-9_](?:[A-Za-z0-9_.\\-]*[A-Za-z0-9_])?)`")

// DefuseMentions wraps @mentions in inline code, so that a text written on one
// forge doesn't notify whoever has the same username on another one. Code
// blocks and inline code are left alone.
func DefuseMentions(text string) string {
	return mapProse(text, func(prose string) string {
		return mentionRe.ReplaceAllString(prose, "$1`$2`")
	})
}

// RestoreMentions reverts DefuseMentions.
func RestoreMentions(text string) string {
	return mapFences(text, func(s string) string {
		return defusedMentionRe.ReplaceAllString(s, "$1")
	})
}

// mapProse applies f to the parts of a markdown text that are neither in a
// fenced code block nor in inline code.
// ponytail: single-backtick code spans only, enough for mentions.
func mapProse(text string, f func(string) string) string {
	return mapFences(text, func(s string) string {
		parts := strings.Split(s, "`")
		for i := 0; i < len(parts); i += 2 {
			parts[i] = f(parts[i])
		}
		return strings.Join(parts, "`")
	})
}

// mapFences applies f to each line of a markdown text that is not in a fenced
// code block.
func mapFences(text string, f func(string) string) string {
	lines := strings.Split(text, "\n")
	fenced := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
			continue
		}
		if !fenced {
			lines[i] = f(line)
		}
	}
	return strings.Join(lines, "\n")
}

package core

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/git-bug/git-bug/entity"
)

// markerRe matches the first line of a text the sync engine posted
var markerRe = regexp.MustCompile(`^<!-- git-bug:([0-9a-f]{64})( attributed)? -->$`)

// earlyMarker started the attribution header of the first GitLab
// export-foreign version, before the sync engine.
const earlyMarker = "<!-- git-bug attribution -->"

// Marker is the first line of every text the sync engine posts. Forges hide
// HTML comments. It carries the git-bug id of the bug or comment, so a pull
// recognizes what a push posted even if recording the push failed.
func Marker(id entity.Id) string {
	return "<!-- git-bug:" + id.String() + " -->"
}

// MarkerID returns the git-bug id carried by a text's marker.
func MarkerID(text string) (entity.Id, bool) {
	first, _, _ := strings.Cut(text, "\n")
	m := markerRe.FindStringSubmatch(strings.TrimSpace(first))
	if m == nil {
		return "", false
	}
	return entity.Id(m[1]), true
}

// Attribution credits the author of a text posted on their behalf.
type Attribution struct {
	Name string
	When time.Time
	// Source is the forge the text was written on, as people name it, and
	// URL where. An empty Source means the text was written in git-bug.
	Source string
	URL    string
}

func (a Attribution) header() string {
	date := a.When.UTC().Format(time.DateOnly)
	if a.Source == "" {
		return fmt.Sprintf("*Posted by %s on %s via git-bug*", a.Name, date)
	}
	h := fmt.Sprintf("*Originally posted by %s on %s", a.Name, date)
	if a.URL != "" {
		h += fmt.Sprintf(", [view on %s](%s)", a.Source, a.URL)
	}
	return h + "*"
}

// Render writes a text to post on a forge: the marker carrying id, the
// attribution header when attr is set, then the text. from is the dialect of
// the forge the text was written on, nil for git-bug: its mentions are
// defused, and ref rewrites its issue references from their number.
func Render(text string, id entity.Id, attr *Attribution, from *Dialect, ref func(n string) string) string {
	if from != nil {
		text = mapProse(text, func(prose string) string {
			prose = mentionRe.ReplaceAllString(prose, "$1`$2`")
			return replaceRefs(prose, from.Ref, ref)
		})
	}
	if attr == nil {
		return Marker(id) + "\n" + text
	}
	return strings.TrimSuffix(Marker(id), " -->") + " attributed -->\n" + attr.header() + "\n\n" + text
}

// replaceRefs replaces the first group of each match of re with ref applied
// to the second group.
func replaceRefs(s string, re *regexp.Regexp, ref func(n string) string) string {
	var b strings.Builder
	last := 0
	for _, m := range re.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(s[last:m[2]])
		b.WriteString(ref(s[m[4]:m[5]]))
		last = m[3]
	}
	b.WriteString(s[last:])
	return b.String()
}

// Unrender turns a text posted by Render back into the text, after it went
// through text.Cleanup. A text without marker is returned as is.
func Unrender(text string) string {
	first, rest, _ := strings.Cut(text, "\n")
	first = strings.TrimSpace(first)
	var attributed bool
	if m := markerRe.FindStringSubmatch(first); m != nil {
		attributed = m[2] != ""
	} else if first == earlyMarker {
		attributed = true
	} else {
		return text
	}
	if attributed {
		// no separator when the text is empty, as text.Cleanup trims it
		_, rest, _ = strings.Cut(rest, "\n\n")
	}
	return RestoreMentions(rest)
}

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
		// an unpaired backtick starts no code: what follows it is prose
		if last := len(parts) - 1; last%2 == 1 {
			parts[last] = f(parts[last])
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

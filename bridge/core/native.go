package core

import (
	"regexp"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

var slugNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// Slug is value as a label name: lower case, accents dropped, runs of other
// characters as a dash. It is git-board's, so labels match across both.
func Slug(value string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(strings.ToLower(strings.TrimSpace(value))) {
		if !unicode.Is(unicode.Mn, r) {
			b.WriteRune(r)
		}
	}
	return strings.Trim(slugNonAlnum.ReplaceAllString(b.String(), "-"), "-")
}

// OneNative applies label changes to a field the forge holds as one value
// (an issue type), given as labels by git-bug. current is the value held,
// native tells which labels are values of the field. It returns the value
// held next, and the labels left to add and to remove: the first native
// label added takes an empty field, the others stay labels, and a label
// that is the held value only comes off as its old-way label.
func OneNative(current string, added, removed []string, native func(label string) bool) (next string, add, remove []string) {
	next = current
	if slices.Contains(removed, current) {
		next = ""
	}
	remove = slices.Clone(removed)
	for _, l := range added {
		switch {
		case !native(l):
			add = append(add, l)
			continue
		case next == "":
			next = l
		case next != l:
			add = append(add, l)
			continue
		}
		if !slices.Contains(remove, l) {
			remove = append(remove, l)
		}
	}
	return next, add, remove
}

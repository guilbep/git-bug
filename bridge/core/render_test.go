package core

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/git-bug/git-bug/entity"
	"github.com/git-bug/git-bug/util/text"
)

func TestDefuseMentions(t *testing.T) {
	for in, out := range map[string]string{
		"hi @nico and @Quentin.":         "hi `@nico` and `@Quentin`.",
		"@guilbep, @pierre":              "`@guilbep`, `@pierre`",
		"see @ENAC-IT.md":                "see `@ENAC-IT.md`",
		"*@Quentin*":                     "*`@Quentin`*",
		"mail a.b@epfl.ch":               "mail a.b@epfl.ch",
		"https://x.org/@nico/post":       "https://x.org/@nico/post",
		"already `@nico` in code":        "already `@nico` in code",
		"```\n@nico in a block\n```\n@a": "```\n@nico in a block\n```\n`@a`",
		"no mention here, nor @":         "no mention here, nor @",
		"(@nicdub)":                      "(`@nicdub`)",
	} {
		require.Equal(t, out, DefuseMentions(in), in)
	}
}

func TestRestoreMentions(t *testing.T) {
	for _, s := range []string{
		"hi @nico and @Quentin.",
		"*@Quentin* see @ENAC-IT.md",
		"```\n`@nico` stays in a block\n```\n@a",
		"mail a.b@epfl.ch and https://x.org/@nico",
	} {
		require.Equal(t, s, RestoreMentions(DefuseMentions(s)), s)
	}
}

// a GitHub-like dialect: "#12", not "group/x#12" nor "page#12"
var testDialect = Dialect{
	Ref:       regexp.MustCompile(`(?:^|[^\w/#])(#(\d+))\b`),
	FormatRef: func(n string) string { return "#" + n },
}

var testId = entity.Id("1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")

func TestMarker(t *testing.T) {
	id, ok := MarkerID(Render("text", testId, nil, nil, nil))
	require.True(t, ok)
	require.Equal(t, testId, id)

	_, ok = MarkerID("text without marker")
	require.False(t, ok)
	_, ok = MarkerID("<!-- git-bug:not-an-id -->\ntext")
	require.False(t, ok)
}

func TestRenderAttribution(t *testing.T) {
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	out := Render("body", testId, &Attribution{Name: "Alice", When: when, Source: "GitHub", URL: "https://x/1"}, nil, nil)
	require.Contains(t, out, "\n*Originally posted by Alice on 2026-01-02, [view on GitHub](https://x/1)*\n\nbody")

	out = Render("body", testId, &Attribution{Name: "Alice", When: when}, nil, nil)
	require.Contains(t, out, "\n*Posted by Alice on 2026-01-02 via git-bug*\n\nbody")

	out = Render("body", testId, nil, nil, nil)
	require.Equal(t, Marker(testId)+"\nbody", out)
}

func TestRenderRefs(t *testing.T) {
	ref := func(n string) string {
		if n == "12" {
			return "#45"
		}
		return "[#" + n + "](https://src/issues/" + n + ")"
	}
	out := Render("see #12 and #13, not group/x#12, page#12, `#12` or\n```\n#12\n```", testId, nil, &testDialect, ref)
	require.Contains(t, out, "see #45 and [#13](https://src/issues/13), not group/x#12, page#12, `#12` or\n```\n#12\n```")

	// written in git-bug: refs and mentions mean nothing to translate
	out = Render("#12 @bob", testId, nil, nil, ref)
	require.Contains(t, out, "\n#12 @bob")
}

func TestRenderDefusesMentionsFromForge(t *testing.T) {
	out := Render("cc @bob", testId, nil, &testDialect, func(n string) string { return n })
	require.Contains(t, out, "cc `@bob`")
}

func TestUnrender(t *testing.T) {
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	attr := &Attribution{Name: "Alice", When: when, Source: "GitHub", URL: "https://x/1"}
	for _, body := range []string{"text", "", "first\n\nsecond", "cc @bob and @carol."} {
		for _, a := range []*Attribution{nil, attr} {
			posted := Render(body, testId, a, &testDialect, func(n string) string { return n })
			// as a forge returns it, maybe with windows new lines
			got := strings.ReplaceAll(posted, "\n", "\r\n")
			require.Equal(t, body, Unrender(text.Cleanup(got)), body)
		}
	}
}

func TestUnrenderLeavesOtherTextAlone(t *testing.T) {
	// a forge user's own text, never rendered by git-bug
	require.Equal(t, "keep `@bob` as is", Unrender("keep `@bob` as is"))
}

func TestUnrenderEarlyMarker(t *testing.T) {
	// the header written by the first export-foreign version, still on forges
	posted := "<!-- git-bug attribution -->\n*Originally posted by Alice on 2026-01-02, [view on github](https://x/1)*\n\ncc `@bob`"
	require.Equal(t, "cc @bob", Unrender(posted))
}

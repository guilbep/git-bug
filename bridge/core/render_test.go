package core

import (
	"testing"

	"github.com/stretchr/testify/require"
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

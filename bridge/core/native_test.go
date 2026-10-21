package core_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/git-bug/git-bug/bridge/core"
)

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"QA Validation": "qa-validation", "Comité Spécialisé": "comite-specialise",
		"S1 - Set up": "s1-set-up", "Preliminary work (S0)": "preliminary-work-s0",
		"Sprint 2": "sprint-2", "  Bug ": "bug",
	} {
		require.Equal(t, want, core.Slug(in), in)
	}
}

func TestOneNative(t *testing.T) {
	native := func(l string) bool { return l == "bug" || l == "feat" }
	for _, c := range []struct {
		name, current  string
		added, removed []string
		next           string
		add, remove    []string
	}{
		{"first takes it", "", []string{"bug", "a"}, nil, "bug", []string{"a"}, []string{"bug"}},
		{"held stays, another is a label", "bug", []string{"feat"}, nil, "bug", []string{"feat"}, nil},
		{"replaced", "bug", []string{"feat"}, []string{"bug"}, "feat", nil, []string{"bug", "feat"}},
		{"cleared", "bug", nil, []string{"bug"}, "", nil, []string{"bug"}},
		{"old way label sent again", "bug", []string{"bug"}, nil, "bug", nil, []string{"bug"}},
		{"not native", "", []string{"polish"}, nil, "", []string{"polish"}, nil},
	} {
		next, add, remove := core.OneNative(c.current, c.added, c.removed, native)
		require.Equal(t, c.next, next, c.name)
		require.Equal(t, c.add, add, c.name)
		require.Equal(t, c.remove, remove, c.name)
	}
}

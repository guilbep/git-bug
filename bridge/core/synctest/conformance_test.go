package synctest

import (
	"testing"

	"github.com/git-bug/git-bug/bridge/core"
)

func TestConformanceFake(t *testing.T) {
	for name, q := range map[string]Quirks{
		"plain":       {},
		"gitlab-like": {NoEditEvents: true, AuthorOnlyEdits: true},
	} {
		t.Run(name, func(t *testing.T) {
			Run(t, Harness{
				NewForge: func(t *testing.T, name string) Forge {
					bot := core.Account{User: core.User{ID: "bot-" + name, Login: "bot", Name: "Bot"}}
					return NewFake(name+".test/1", name+".test", q, bot)
				},
				LoginKey: "fake-login",
			})
		})
	}
}

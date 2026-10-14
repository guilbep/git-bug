package synctest

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/entities/common"
)

var bot = core.Account{User: core.User{ID: "1", Login: "bot", Name: "Bot"}}

func issues(t *testing.T, f *Fake) []*core.Issue {
	var res []*core.Issue
	for issue, err := range f.Issues(context.Background(), time.Time{}) {
		require.NoError(t, err)
		res = append(res, issue)
	}
	return res
}

func TestFakeActorActions(t *testing.T) {
	f := NewFake("fake.test/1", "fake.test", Quirks{}, bot)
	alice := f.UserAct("alice")

	issue := alice.CreateIssue("title", "body")
	comment := alice.Comment(issue, "hello")
	alice.EditText(issue, comment, "hello, edited")
	alice.SetTitle(issue, "new title")
	alice.SetLabels(issue, []string{"bug"})
	alice.SetStatus(issue, common.ClosedStatus)

	all := issues(t, f)
	require.Len(t, all, 1)
	got := all[0]
	require.Equal(t, "1", got.Number)
	require.Equal(t, "alice", got.Author.Login)
	require.Equal(t, "title", got.OriginalTitle)
	require.Equal(t, "new title", got.Title)
	require.Equal(t, common.ClosedStatus, got.Status)
	require.Equal(t, []string{"bug"}, got.Labels)
	require.Len(t, got.Comments, 1)
	require.Equal(t, "hello", got.Comments[0].Original)
	require.Equal(t, "hello, edited", got.Comments[0].Body)

	var kinds []core.EventKind
	for _, e := range got.Events {
		require.Equal(t, "alice", e.Author.Login)
		require.NotEmpty(t, e.ID)
		kinds = append(kinds, e.Kind)
	}
	require.Equal(t, []core.EventKind{core.EventTextEdit, core.EventTitle, core.EventLabels, core.EventStatus}, kinds)
	require.Zero(t, f.Writes(), "actors are not the engine")
}

func TestFakeEngineWrites(t *testing.T) {
	ctx := context.Background()
	f := NewFake("fake.test/1", "fake.test", Quirks{}, bot)

	issue, err := f.CreateIssue(ctx, bot, "title", "body")
	require.NoError(t, err)
	_, err = f.AddComment(ctx, bot, issue, "comment")
	require.NoError(t, err)
	require.NoError(t, f.SetStatus(ctx, bot, issue, common.ClosedStatus))
	// setting the current value changes nothing, as on forges
	require.NoError(t, f.SetStatus(ctx, bot, issue, common.ClosedStatus))

	got := f.Issue("1")
	require.Equal(t, "bot", got.Author.Login)
	require.Len(t, got.Events, 1)
	require.Equal(t, "bot", got.Events[0].Author.Login)
	require.Equal(t, 4, f.Writes())

	accounts, err := f.Accounts(ctx)
	require.NoError(t, err)
	require.Equal(t, []core.Account{bot}, accounts)
}

func TestFakeQuirks(t *testing.T) {
	ctx := context.Background()

	f := NewFake("fake.test/1", "fake.test", Quirks{NoEditEvents: true, AuthorOnlyEdits: true}, bot)
	alice := f.UserAct("alice")
	issue := alice.CreateIssue("title", "body")
	comment := alice.Comment(issue, "hello")
	alice.EditText(issue, comment, "edited")
	require.Empty(t, f.Issue("1").Events)
	require.Error(t, f.EditText(ctx, bot, issue, comment, "not mine"))

	f = NewFake("fake.test/1", "fake.test", Quirks{FailAfterWrite: true}, bot)
	_, err := f.CreateIssue(ctx, bot, "title", "body")
	require.Error(t, err)
	require.NotNil(t, f.Issue("1"), "the write happened")
	_, err = f.CreateIssue(ctx, bot, "title 2", "body")
	require.NoError(t, err, "only once")

	f = NewFake("fake.test/1", "fake.test", Quirks{RateLimitOnce: true}, bot)
	_, err = f.CreateIssue(ctx, bot, "title", "body")
	var limited core.ErrRateLimited
	require.True(t, errors.As(err, &limited))
	require.Nil(t, f.Issue("1"), "nothing written")
	_, err = f.CreateIssue(ctx, bot, "title", "body")
	require.NoError(t, err)
}

func TestFakeDeletedIssue(t *testing.T) {
	ctx := context.Background()
	f := NewFake("fake.test/1", "fake.test", Quirks{}, bot)
	alice := f.UserAct("alice")
	issue := alice.CreateIssue("title", "body")
	alice.Delete(issue)

	require.Empty(t, issues(t, f))
	require.ErrorIs(t, f.SetTitle(ctx, bot, issue, "x"), core.ErrGone)
}

func TestFakeIssuesSince(t *testing.T) {
	f := NewFake("fake.test/1", "fake.test", Quirks{}, bot)
	alice := f.UserAct("alice")
	alice.CreateIssue("old", "body")
	cut := time.Now().Add(time.Second)
	f.Clock = func() time.Time { return cut.Add(time.Minute) }
	alice.CreateIssue("new", "body")

	var titles []string
	for issue, err := range f.Issues(context.Background(), cut) {
		require.NoError(t, err)
		titles = append(titles, issue.Title)
	}
	require.Equal(t, []string{"new"}, titles)
	require.True(t, slices.Equal(f.Dialect().Ref.FindStringSubmatch("see #12")[1:], []string{"#12", "12"}))
}

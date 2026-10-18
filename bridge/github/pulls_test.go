package github

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shurcooL/githubv4"
	"github.com/stretchr/testify/require"

	"github.com/git-bug/git-bug/bridge/core"
	"github.com/git-bug/git-bug/cache"
	"github.com/git-bug/git-bug/entities/bug"
	"github.com/git-bug/git-bug/entity"
	"github.com/git-bug/git-bug/repository/repotest"
)

// ghPull is a pull request on the fake GitHub: it takes line comments on the
// files it changes, at its head commit.
type ghPull struct {
	head                     string
	paths                    []string
	lines, reviews, comments []restComment
	resolved                 map[string]bool // by thread id
}

func (f *fakeGithub) openPull(number int, head string, paths ...string) *ghPull {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pulls == nil {
		f.pulls = map[int]*ghPull{}
	}
	p := &ghPull{head: head, paths: paths, resolved: map[string]bool{}}
	f.pulls[number] = p
	return p
}

var restPath = regexp.MustCompile(`^/repos/[^/]+/[^/]+/(pulls|issues)/(\d+)/(comments|reviews)(?:\?per_page=100&page=(\d+))?$`)

// REST answers the REST calls of the adapter, about pull requests.
func (c fakeClient) REST(_ context.Context, method, path string, in, out any) error {
	f := c.f
	f.mu.Lock()
	defer f.mu.Unlock()
	m := restPath.FindStringSubmatch(path)
	if m == nil {
		return &restError{Status: http.StatusNotFound, Message: "Not Found"}
	}
	kind := m[1] + "/" + m[3]
	n, _ := strconv.Atoi(m[2])
	p := f.pulls[n]
	if p == nil {
		return &restError{Status: http.StatusNotFound, Message: "Not Found"}
	}
	if method == http.MethodGet {
		if m[4] == "1" {
			*out.(*[]restComment) = slices.Clone(*map[string]*[]restComment{
				"pulls/comments": &p.lines, "pulls/reviews": &p.reviews, "issues/comments": &p.comments,
			}[kind])
		}
		return nil
	}
	f.writes++
	rc, err := f.post(c.login, n, kind, in.(restPost))
	if err != nil {
		return err
	}
	*out.(*restComment) = rc
	return nil
}

// post posts on a pull request as GitHub does. Called with f.mu held.
func (f *fakeGithub) post(login string, n int, kind string, post restPost) (restComment, error) {
	p := f.pulls[n]
	f.lastID++
	id := int64(f.lastID)
	at := now().Time
	rc := restComment{ID: id, User: &restUser{Login: login}, Body: post.Body, CreatedAt: at, UpdatedAt: at}
	url := fmt.Sprintf("https://github.com/%s/project/pull/%d", f.owner, n)
	invalid := &restError{Status: http.StatusUnprocessableEntity, Message: "Validation Failed"}
	switch kind {
	case "pulls/reviews":
		rc.NodeID, rc.HTMLURL = fmt.Sprintf("PRR_%d", id), fmt.Sprintf("%s#pullrequestreview-%d", url, id)
		rc.State = map[string]string{"APPROVE": "APPROVED", "REQUEST_CHANGES": "CHANGES_REQUESTED", "COMMENT": "COMMENTED"}[post.Event]
		rc.CommitID, rc.SubmittedAt = post.CommitID, at
		p.reviews = append(p.reviews, rc)
	case "pulls/comments":
		rc.NodeID, rc.HTMLURL = fmt.Sprintf("PRRC_%d", id), fmt.Sprintf("%s#discussion_r%d", url, id)
		if post.InReplyTo != 0 {
			i := slices.IndexFunc(p.lines, func(o restComment) bool { return o.ID == post.InReplyTo })
			if i < 0 {
				return restComment{}, invalid
			}
			parent := p.lines[i]
			rc.Path, rc.Line, rc.OriginalLine, rc.Side = parent.Path, parent.Line, parent.OriginalLine, parent.Side
			rc.CommitID, rc.OriginalCommitID, rc.InReplyToID = parent.CommitID, parent.OriginalCommitID, parent.ID
		} else {
			if post.CommitID != p.head || !slices.Contains(p.paths, post.Path) {
				return restComment{}, invalid
			}
			line := post.Line
			rc.Path, rc.Line, rc.OriginalLine, rc.Side = post.Path, &line, &line, post.Side
			rc.CommitID, rc.OriginalCommitID = post.CommitID, post.CommitID
			// GitHub holds a line comment posted alone in a review without text
			f.lastID++
			p.reviews = append(p.reviews, restComment{
				ID: int64(f.lastID), NodeID: fmt.Sprintf("PRR_%d", f.lastID), User: rc.User,
				State: "COMMENTED", CommitID: post.CommitID, SubmittedAt: at,
			})
		}
		p.lines = append(p.lines, rc)
	case "issues/comments":
		rc.NodeID, rc.HTMLURL = fmt.Sprintf("IC_%d", id), fmt.Sprintf("%s#issuecomment-%d", url, id)
		p.comments = append(p.comments, rc)
	}
	return rc, nil
}

// pullAct posts on a pull request as login, directly on GitHub.
func (f *fakeGithub) pullAct(login string, n int, kind string, post restPost) restComment {
	f.mu.Lock()
	defer f.mu.Unlock()
	rc, err := f.post(login, n, kind, post)
	if err != nil {
		panic(err)
	}
	return rc
}

func (f *fakeGithub) reviewThreads(q *reviewThreadsQuery, vars map[string]interface{}) error {
	p := f.pulls[int(vars["number"].(githubv4.Int))]
	if p == nil {
		return fmt.Errorf("Could not resolve to a PullRequest with the number of %v", vars["number"])
	}
	for _, c := range p.lines {
		if c.InReplyToID == 0 {
			t := reviewThread{ID: "PRRT_" + c.NodeID}
			t.Comments.Nodes = []nodeRef{{ID: c.NodeID}}
			q.Repository.PullRequest.ReviewThreads.Nodes = append(q.Repository.PullRequest.ReviewThreads.Nodes, t)
		}
	}
	return nil
}

func (f *fakeGithub) resolve(thread githubv4.ID, resolved bool) error {
	for _, p := range f.pulls {
		for _, c := range p.lines {
			if "PRRT_"+c.NodeID == fmt.Sprint(thread) {
				p.resolved[fmt.Sprint(thread)] = resolved
				return nil
			}
		}
	}
	return fmt.Errorf("Could not resolve to a node with the global id of '%v'", thread)
}

var head = strings.Repeat("a", 40)

// pullSetup is a bug pulled from issue 1 of a fake GitHub, whose pull request
// 7 changes main.go at head; the bot has a token and an identity.
func pullSetup(t *testing.T) (githubForge, *cache.RepoCache, *cache.BugCache, *cache.IdentityCache) {
	g := newGithubForge(t, "owner")
	g.fake.openPull(7, head, "main.go")
	g.UserAct("alice").CreateIssue("title", "body")

	repo := repotest.NewGoGitRepo(t, false)
	backend, err := cache.NewRepoCacheNoEvents(repo, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })
	me, err := backend.Identities().New("Me", "me@example.com")
	require.NoError(t, err)
	require.NoError(t, backend.SetUserIdentity(me))
	bot, err := backend.Identities().NewRaw("Bot", "", "bot", "", nil, map[string]string{metaKeyGithubLogin: "bot"})
	require.NoError(t, err)

	syncPulls(t, backend, g)
	b, err := backend.Bugs().Resolve(backend.Bugs().AllIds()[0])
	require.NoError(t, err)
	return g, backend, b, bot
}

// syncPulls pulls and pushes, bugs' pull request being number 7.
func syncPulls(t *testing.T, repo *cache.RepoCache, g githubForge) {
	e, err := core.NewEngine(context.Background(), repo, g, core.SyncOptions{
		Foreign: true, DefaultLogin: "bot", LoginKey: metaKeyGithubLogin,
		PullsOf: func(entity.Id) []string { return []string{"7"} },
	})
	require.NoError(t, err)
	for r := range e.Pull(context.Background(), time.Time{}) {
		require.NotEqual(t, core.ImportEventError, r.Event, r.String())
	}
	for r := range e.Push(context.Background()) {
		require.NotEqual(t, core.ExportEventError, r.Event, r.String())
	}
}

func TestGithubPullsPullRequestComments(t *testing.T) {
	g, repo, b, _ := pullSetup(t)
	line := g.fake.pullAct("carol", 7, "pulls/comments", restPost{Body: "why?", CommitID: head, Path: "main.go", Line: 3, Side: "RIGHT"})
	g.fake.pullAct("Copilot", 7, "pulls/comments", restPost{Body: "I agree", InReplyTo: line.ID})
	g.fake.pullAct("carol", 7, "pulls/reviews", restPost{Body: "fix it", Event: "REQUEST_CHANGES", CommitID: head})
	g.fake.pullAct("github-actions[bot]", 7, "issues/comments", restPost{Body: "CI passed"})
	writes := g.Writes()

	syncPulls(t, repo, g)

	snap := b.Snapshot()
	require.Len(t, snap.Comments, 5, "the review holding the line comment isn't one")
	var ops []*bug.AddCommentOperation
	for _, op := range snap.Operations {
		if c, ok := op.(*bug.AddCommentOperation); ok {
			ops = append(ops, c)
		}
	}
	origin := "sync:github.com/owner/project:pr-comment"
	pr := "github.com/owner/project#7"

	require.Equal(t, "why?", ops[0].Message)
	require.Equal(t, "carol", ops[0].Author().Login())
	require.Equal(t, map[string]string{
		origin: line.NodeID, "sync:github.com/owner/project:url": line.HTMLURL,
		"sync:github.com/owner/project:text": core.Fingerprint("why?"),
		"review:pr":                          pr, "review:path": "main.go", "review:line": "3",
		"review:side": "new", "review:commit": head,
	}, ops[0].Metadata)

	require.Equal(t, "Copilot", ops[1].Author().Login())
	require.Equal(t, string(entity.CombineIds(b.Id(), ops[0].Id())), ops[1].Metadata["review:reply-to"])
	require.Equal(t, "main.go", ops[1].Metadata["review:path"])

	require.Equal(t, "fix it", ops[2].Message)
	require.Equal(t, "REQUEST_CHANGES", ops[2].Metadata["review:state"])
	require.Equal(t, head, ops[2].Metadata["review:commit"])
	require.Equal(t, pr, ops[2].Metadata["review:pr"])

	require.Equal(t, "CI passed", ops[3].Message)
	require.Equal(t, "github-actions[bot]", ops[3].Author().Login())
	require.Equal(t, pr, ops[3].Metadata["review:pr"])
	require.True(t, strings.HasPrefix(ops[3].Metadata[origin], "IC_"))

	// once, and nothing sent
	syncPulls(t, repo, g)
	require.Len(t, b.Snapshot().Comments, 5)
	require.Equal(t, writes, g.Writes())
	require.Len(t, g.Issue("1").Comments, 0)
}

func TestGithubSendsReviewComments(t *testing.T) {
	g, repo, b, bot := pullSetup(t)
	me, err := repo.GetUserIdentity()
	require.NoError(t, err)
	anchor := func(path string) map[string]string {
		return map[string]string{"review:pr": "github.com/owner/project#7", "review:path": path,
			"review:line": "3", "review:side": "new", "review:commit": head}
	}
	first, firstOp, err := b.AddCommentRaw(bot, time.Now().Unix(), "why?", nil, anchor("main.go"))
	require.NoError(t, err)
	reply := anchor("main.go")
	reply["review:reply-to"] = string(first)
	_, _, err = b.AddCommentRaw(me, time.Now().Unix(), "because", nil, reply)
	require.NoError(t, err)
	_, _, err = b.AddCommentRaw(bot, time.Now().Unix(), "elsewhere", nil, anchor("other.go"))
	require.NoError(t, err)
	_, _, err = b.AddCommentRaw(bot, time.Now().Unix(), "looks good", nil, map[string]string{
		"review:pr": "github.com/owner/project#7", "review:state": "APPROVE", "review:commit": head,
	})
	require.NoError(t, err)
	_, err = b.SetMetadataRaw(bot, time.Now().Unix(), firstOp.Id(), map[string]string{"review:resolved": "true"})
	require.NoError(t, err)
	require.NoError(t, b.Commit())

	syncPulls(t, repo, g)

	p := g.fake.pulls[7]
	require.Len(t, p.lines, 2)
	require.Equal(t, "main.go", p.lines[0].Path)
	require.Equal(t, 3, *p.lines[0].Line)
	require.Equal(t, "RIGHT", p.lines[0].Side)
	require.Equal(t, head, p.lines[0].CommitID)
	require.Equal(t, "bot", p.lines[0].User.Login)
	require.Equal(t, p.lines[0].ID, p.lines[1].InReplyToID)
	require.Contains(t, p.lines[1].Body, "*Posted by Me on")
	require.Len(t, p.comments, 1)
	require.Contains(t, p.comments[0].Body, "On `other.go` line 3 (new side) at aaaaaaa:")
	require.Equal(t, "APPROVED", p.reviews[len(p.reviews)-1].State)
	require.Equal(t, head, p.reviews[len(p.reviews)-1].CommitID)
	require.True(t, p.resolved["PRRT_"+p.lines[0].NodeID])
	require.Len(t, g.Issue("1").Comments, 0)

	writes, ops := g.Writes(), len(b.Snapshot().Operations)
	syncPulls(t, repo, g)
	require.Equal(t, writes, g.Writes(), "nothing sent twice")
	require.Len(t, b.Snapshot().Comments, 5, "nothing pulled back")
	require.Len(t, b.Snapshot().Operations, ops)
}

// The flow starts on GitHub: a reply in git-bug to a pulled line comment
// answers it there, and resolves its thread; an approval without text,
// GitHub's most common one, is pulled.
func TestGithubAnswersPulledLineComment(t *testing.T) {
	g, repo, b, bot := pullSetup(t)
	line := g.fake.pullAct("carol", 7, "pulls/comments", restPost{Body: "why?", CommitID: head, Path: "main.go", Line: 3, Side: "LEFT"})
	g.fake.pullAct("carol", 7, "pulls/reviews", restPost{Event: "APPROVE", CommitID: head})
	syncPulls(t, repo, g)

	snap := b.Snapshot()
	require.Len(t, snap.Comments, 3)
	approval := snap.Comments[2]
	require.Empty(t, approval.Message)
	var pulled *bug.AddCommentOperation
	for _, op := range snap.Operations {
		if c, ok := op.(*bug.AddCommentOperation); ok {
			if c.Message == "why?" {
				pulled = c
			}
			if c.Message == "" {
				require.Equal(t, "APPROVE", c.Metadata["review:state"])
			}
		}
	}
	require.Equal(t, "old", pulled.Metadata["review:side"])

	me, err := repo.GetUserIdentity()
	require.NoError(t, err)
	reply := maps.Clone(pulled.Metadata)
	for k := range reply {
		if strings.HasPrefix(k, "sync:") {
			delete(reply, k)
		}
	}
	reply["review:reply-to"] = string(entity.CombineIds(b.Id(), pulled.Id()))
	_, _, err = b.AddCommentRaw(me, time.Now().Unix(), "because", nil, reply)
	require.NoError(t, err)
	_, err = b.SetMetadataRaw(bot, time.Now().Unix(), pulled.Id(), map[string]string{"review:resolved": "true"})
	require.NoError(t, err)
	require.NoError(t, b.Commit())

	syncPulls(t, repo, g)

	p := g.fake.pulls[7]
	require.Len(t, p.lines, 2)
	require.Equal(t, line.ID, p.lines[1].InReplyToID)
	require.True(t, p.resolved["PRRT_"+line.NodeID])
	require.Len(t, b.Snapshot().Comments, 4)
}

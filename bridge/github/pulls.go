package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/shurcooL/githubv4"

	"github.com/git-bug/git-bug/bridge/core"
)

// Pull requests' line comments, reviews and comments go through GitHub's REST
// API: its GraphQL API adds line comments to a pending review, at the head
// commit only. Resolving a thread is GraphQL only.

var _ core.PullRemote = &remote{}

// restClient calls GitHub's REST API: path is under its root, in and out are
// the request's and the answer's JSON, if any.
type restClient interface {
	REST(ctx context.Context, method, path string, in, out any) error
}

// apiClient calls GitHub's GraphQL and REST APIs with one token.
type apiClient struct {
	*githubv4.Client
	http *http.Client
}

func (c apiClient) REST(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, githubV3Url+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var answer struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&answer)
		return &restError{Status: resp.StatusCode, Message: answer.Message}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// restError is an answer of GitHub's REST API other than a success.
type restError struct {
	Status  int
	Message string
}

func (e *restError) Error() string {
	return fmt.Sprintf("GitHub answered %d: %s", e.Status, e.Message)
}

// restComment is a pull request's line comment, review, or comment, as
// GitHub's REST API gives them.
type restComment struct {
	ID        int64     `json:"id"`
	NodeID    string    `json:"node_id"`
	User      *restUser `json:"user"`
	Body      string    `json:"body"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// line comments
	Path             string `json:"path"`
	Line             *int   `json:"line"`
	OriginalLine     *int   `json:"original_line"`
	Side             string `json:"side"`
	CommitID         string `json:"commit_id"`
	OriginalCommitID string `json:"original_commit_id"`
	InReplyToID      int64  `json:"in_reply_to_id"`

	// reviews
	State       string    `json:"state"`
	SubmittedAt time.Time `json:"submitted_at"`
}

type restUser struct {
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
}

func (u *restUser) user() core.User {
	if u == nil {
		return core.User{ID: "ghost", Login: "ghost", Name: "Deleted user"}
	}
	return core.User{ID: u.Login, Login: u.Login, AvatarURL: u.AvatarURL}
}

// restPost is what posts a line comment, a review or a comment.
type restPost struct {
	Body      string `json:"body"`
	CommitID  string `json:"commit_id,omitempty"`
	Path      string `json:"path,omitempty"`
	Line      int    `json:"line,omitempty"`
	Side      string `json:"side,omitempty"`
	InReplyTo int64  `json:"in_reply_to,omitempty"`
	Event     string `json:"event,omitempty"`
}

// GitHub's names of review states, and of diff sides.
var (
	reviewStates = map[string]string{"APPROVED": "APPROVE", "CHANGES_REQUESTED": "REQUEST_CHANGES", "COMMENTED": "COMMENT"}
	sides        = map[string]string{"LEFT": "old", "RIGHT": "new"}
)

func (r *remote) PullComments(ctx context.Context, pr string) ([]core.PullComment, error) {
	repo := r.restRepo()
	var lines, reviews, comments []restComment
	for path, list := range map[string]*[]restComment{
		repo + "/pulls/" + pr + "/comments":  &lines,
		repo + "/pulls/" + pr + "/reviews":   &reviews,
		repo + "/issues/" + pr + "/comments": &comments,
	} {
		if err := r.list(ctx, path, list); err != nil {
			return nil, err
		}
	}

	var res []core.PullComment
	nodeIDs := map[int64]string{}
	for _, c := range lines {
		nodeIDs[c.ID] = c.NodeID
	}
	for _, c := range lines {
		pc := c.comment()
		// where it was written, still there when outdated
		pc.Path, pc.Side = c.Path, sides[c.Side]
		pc.Commit = c.OriginalCommitID
		if pc.Commit == "" {
			pc.Commit = c.CommitID
		}
		for _, line := range []*int{c.OriginalLine, c.Line} {
			if line != nil {
				pc.Line = *line
				break
			}
		}
		if c.InReplyToID != 0 {
			pc.ReplyTo = core.Ref{ID: nodeIDs[c.InReplyToID]}
		}
		res = append(res, pc)
	}
	for _, c := range reviews {
		state, ok := reviewStates[c.State]
		// a review without text holding line comments, as GitHub makes for
		// each line comment posted alone; pending and dismissed ones
		if !ok || state == "COMMENT" && strings.TrimSpace(c.Body) == "" {
			continue
		}
		pc := c.comment()
		pc.Created, pc.Updated = c.SubmittedAt, c.SubmittedAt
		pc.State, pc.Commit = state, c.CommitID
		res = append(res, pc)
	}
	for _, c := range comments {
		res = append(res, c.comment())
	}
	slices.SortStableFunc(res, func(a, b core.PullComment) int { return a.Created.Compare(b.Created) })
	return res, nil
}

func (c restComment) comment() core.PullComment {
	return core.PullComment{
		Ref:     core.Ref{ID: c.NodeID, URL: c.HTMLURL},
		Author:  c.User.user(),
		Created: c.CreatedAt,
		Updated: c.UpdatedAt,
		Body:    c.Body,
	}
}

// discussionID is the id GitHub gives a line comment in its URL, which a
// reply names.
var discussionID = regexp.MustCompile(`#discussion_r(\d+)$`)

func (r *remote) AddPullComment(ctx context.Context, as core.Account, pr string, c core.PullComment) (core.Ref, error) {
	path := r.restRepo() + "/issues/" + pr + "/comments"
	post := restPost{Body: c.Body}
	switch {
	case c.State != "":
		path = r.restRepo() + "/pulls/" + pr + "/reviews"
		post.Event, post.CommitID = c.State, c.Commit
	case c.Path != "":
		path = r.restRepo() + "/pulls/" + pr + "/comments"
		if m := discussionID.FindStringSubmatch(c.ReplyTo.URL); m != nil {
			post.InReplyTo, _ = strconv.ParseInt(m[1], 10, 64)
			break
		}
		post.CommitID, post.Path, post.Line, post.Side = c.Commit, c.Path, c.Line, "RIGHT"
		if c.Side == "old" {
			post.Side = "LEFT"
		}
	}
	var answer restComment
	err := r.rest(ctx, as.Login, http.MethodPost, path, post, &answer)
	if re := (*restError)(nil); errors.As(err, &re) && re.Status == http.StatusUnprocessableEntity && c.State == "" && c.Path != "" {
		return core.Ref{}, fmt.Errorf("%w: %w", core.ErrOutsideDiff, err)
	}
	if err != nil {
		return core.Ref{}, err
	}
	return core.Ref{ID: answer.NodeID, URL: answer.HTMLURL}, nil
}

func (r *remote) ResolveThread(ctx context.Context, as core.Account, pr string, first core.Ref, resolved bool) error {
	number, err := strconv.Atoi(pr)
	if err != nil {
		return fmt.Errorf("pull request %q: %w", pr, err)
	}
	vars := varmap{
		"owner":  githubv4.String(r.owner),
		"name":   githubv4.String(r.project),
		"number": githubv4.Int(number),
		"after":  (*githubv4.String)(nil),
	}
	thread := ""
	for thread == "" {
		var q reviewThreadsQuery
		if err := r.reader.queryPrintMsgs(ctx, &q, vars); err != nil {
			return mapErr(err)
		}
		threads := q.Repository.PullRequest.ReviewThreads
		for _, t := range threads.Nodes {
			if len(t.Comments.Nodes) > 0 && t.Comments.Nodes[0].ID == first.ID {
				thread = t.ID
			}
		}
		if thread == "" && !threads.PageInfo.HasNextPage {
			return fmt.Errorf("%w: no thread of pull request #%s starts with %s", core.ErrGone, pr, first.URL)
		}
		vars["after"] = threads.PageInfo.EndCursor
	}
	if resolved {
		var m resolveReviewThreadMutation
		return r.mutate(ctx, as, &m, githubv4.ResolveReviewThreadInput{ThreadID: thread})
	}
	var m unresolveReviewThreadMutation
	return r.mutate(ctx, as, &m, githubv4.UnresolveReviewThreadInput{ThreadID: thread})
}

func (r *remote) restRepo() string {
	return "/repos/" + neturl.PathEscape(r.owner) + "/" + neturl.PathEscape(r.project)
}

// list reads every page of a REST listing.
func (r *remote) list(ctx context.Context, path string, out *[]restComment) error {
	for page := 1; ; page++ {
		var items []restComment
		err := r.rest(ctx, "", http.MethodGet,
			fmt.Sprintf("%s?per_page=100&page=%d", path, page), nil, &items)
		if err != nil {
			return err
		}
		*out = append(*out, items...)
		if len(items) < 100 {
			return nil
		}
	}
}

// rest calls the REST API as the account of a login, or as the reader for
// none.
func (r *remote) rest(ctx context.Context, login, method, path string, in, out any) error {
	c := r.reader
	if login != "" {
		var ok bool
		if c, ok = r.clients[login]; !ok {
			return fmt.Errorf("no GitHub token for %s", login)
		}
	}
	rc, ok := c.sc.(restClient)
	if !ok {
		return errors.New("no REST client")
	}
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	return mapRESTErr(rc.REST(ctx, method, path, in, out))
}

// mapRESTErr turns GitHub's REST answers into the engine's errors.
func mapRESTErr(err error) error {
	var re *restError
	if !errors.As(err, &re) {
		return err
	}
	switch {
	case re.Status == http.StatusTooManyRequests,
		re.Status == http.StatusForbidden && strings.Contains(strings.ToLower(re.Message), "rate limit"):
		return core.ErrRateLimited{Until: time.Now().Add(rateLimitWait)}
	case re.Status == http.StatusNotFound:
		return fmt.Errorf("%w: %w", core.ErrGone, err)
	case re.Status == http.StatusForbidden, re.Status == http.StatusUnprocessableEntity:
		return fmt.Errorf("%w: %w", core.ErrRefused, err)
	}
	return err
}

// reviewThreadsQuery finds the threads of a pull request by their first
// comment, with fields the repo scope reads.
type reviewThreadsQuery struct {
	Repository struct {
		PullRequest struct {
			ReviewThreads struct {
				Nodes    []reviewThread
				PageInfo pageInfo
			} `graphql:"reviewThreads(first: 100, after: $after)"`
		} `graphql:"pullRequest(number: $number)"`
	} `graphql:"repository(owner: $owner, name: $name)"`
}

type reviewThread struct {
	ID       string `graphql:"id"`
	Comments struct {
		Nodes []nodeRef
	} `graphql:"comments(first: 1)"`
}

type nodeRef struct {
	ID string `graphql:"id"`
}

type resolveReviewThreadMutation struct {
	ResolveReviewThread struct {
		Thread struct {
			ID string `graphql:"id"`
		}
	} `graphql:"resolveReviewThread(input:$input)"`
}

type unresolveReviewThreadMutation struct {
	UnresolveReviewThread struct {
		Thread struct {
			ID string `graphql:"id"`
		}
	} `graphql:"unresolveReviewThread(input:$input)"`
}

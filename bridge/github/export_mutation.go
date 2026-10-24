package github

import "github.com/shurcooL/githubv4"

type createIssueMutation struct {
	CreateIssue struct {
		Issue struct {
			ID  string `graphql:"id"`
			URL string `graphql:"url"`
		}
	} `graphql:"createIssue(input:$input)"`
}

type updateIssueMutation struct {
	UpdateIssue struct {
		Issue struct {
			ID  string `graphql:"id"`
			URL string `graphql:"url"`
		}
	} `graphql:"updateIssue(input:$input)"`
}

type addCommentToIssueMutation struct {
	AddComment struct {
		CommentEdge struct {
			Node struct {
				ID  string `graphql:"id"`
				URL string `graphql:"url"`
			}
		}
	} `graphql:"addComment(input:$input)"`
}

type updateIssueCommentMutation struct {
	UpdateIssueComment struct {
		IssueComment struct {
			ID  string `graphql:"id"`
			URL string `graphql:"url"`
		} `graphql:"issueComment"`
	} `graphql:"updateIssueComment(input:$input)"`
}

type removeLabelsFromLabelableMutation struct {
	RemoveLabels struct {
		Labelable struct {
			Typename string `graphql:"__typename"`
		}
	} `graphql:"removeLabelsFromLabelable(input:$input)"`
}

type addAssigneesToAssignableMutation struct {
	AddAssignees struct {
		Assignable assignable
	} `graphql:"addAssigneesToAssignable(input:$input)"`
}

type removeAssigneesFromAssignableMutation struct {
	RemoveAssignees struct {
		Assignable assignable
	} `graphql:"removeAssigneesFromAssignable(input:$input)"`
}

// assignable is an issue, with its assignees after a change: GitHub allows
// ten.
type assignable struct {
	Issue struct {
		Assignees struct {
			Nodes []struct {
				Login string `graphql:"login"`
			}
		} `graphql:"assignees(first: 20)"`
	} `graphql:"... on Issue"`
}

type addLabelsToLabelableMutation struct {
	AddLabels struct {
		Labelable struct {
			Typename string `graphql:"__typename"`
		}
	} `graphql:"addLabelsToLabelable(input:$input)"`
}

/**
type createLabelMutation struct {
	CreateLabel struct {
		Label struct {
			ID string `graphql:"id"`
		} `graphql:"label"`
	} `graphql:"createLabel(input: $input)"`
}

type createLabelInput struct {
	Color        githubv4.String  `json:"color"`
	Description  *githubv4.String `json:"description,omitempty"`
	Name         githubv4.String  `json:"name"`
	RepositoryID githubv4.ID      `json:"repositoryId"`

	ClientMutationID *githubv4.String `json:"clientMutationId,omitempty"`
}
*/

// UpdateIssueIssueTypeInput is updateIssueIssueType's input, which githubv4
// lacks. It is exported, as githubv4 names the variable's type after the Go
// type. A nil IssueTypeID goes as null, which clears the type.
type UpdateIssueIssueTypeInput struct {
	IssueID     githubv4.ID  `json:"issueId"`
	IssueTypeID *githubv4.ID `json:"issueTypeId"`
}

type updateIssueIssueTypeMutation struct {
	UpdateIssueIssueType struct {
		Issue struct {
			ID string `graphql:"id"`
		}
	} `graphql:"updateIssueIssueType(input:$input)"`
}

// issueTypeOfQuery reads the type of an issue.
type issueTypeOfQuery struct {
	Node struct {
		Issue struct {
			IssueType *struct {
				Name string `graphql:"name"`
			} `graphql:"issueType"`
		} `graphql:"... on Issue"`
	} `graphql:"node(id: $id)"`
}

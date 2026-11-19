package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// graphqlURL derives the GraphQL endpoint from the REST base:
// https://api.github.com -> https://api.github.com/graphql,
// https://ghe.example/api/v3 -> https://ghe.example/api/graphql.
func (c *Client) graphqlURL() string {
	if b, ok := strings.CutSuffix(c.Base, "/api/v3"); ok {
		return b + "/api/graphql"
	}
	return c.Base + "/graphql"
}

type gqlError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// GraphQLError is a GraphQL response that carried errors.
type GraphQLError struct{ Errors []gqlError }

func (e *GraphQLError) Error() string {
	var msgs []string
	for _, x := range e.Errors {
		msgs = append(msgs, x.Message)
	}
	return "graphql: " + strings.Join(msgs, "; ")
}

// ErrDiscussionsForbidden is returned when the token cannot read a
// repository's discussions.
var ErrDiscussionsForbidden = errors.New("the token cannot read discussions (grant Discussions read, or pass --discussions=false)")

// gone reports a discussion, comment or repository that does not exist
// (any more), which the REST surfaces treat as nothing to scan.
func gone(err error) bool {
	if isStatus(err, http.StatusNotFound, http.StatusGone) {
		return true
	}
	var ge *GraphQLError
	if !errors.As(err, &ge) || len(ge.Errors) == 0 {
		return false
	}
	for _, x := range ge.Errors {
		if x.Type != "NOT_FOUND" {
			return false
		}
	}
	return true
}

func rateLimited(body []byte) bool {
	var env struct {
		Errors []gqlError `json:"errors"`
	}
	if json.Unmarshal(body, &env) != nil {
		return false
	}
	for _, x := range env.Errors {
		if x.Type == "RATE_LIMITED" {
			return true
		}
	}
	return false
}

func (c *Client) graphql(ctx context.Context, query string, vars map[string]any, out any) error {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	u := c.graphqlURL()
	body, _, err := c.do(ctx, http.MethodPost, u, payload, rateLimited)
	if err != nil {
		return err
	}
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []gqlError      `json:"errors"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("%s: %w", redactURL(u), err)
	}
	if len(env.Errors) > 0 {
		for _, x := range env.Errors {
			if x.Type == "FORBIDDEN" {
				return ErrDiscussionsForbidden
			}
		}
		return &GraphQLError{Errors: env.Errors}
	}
	return json.Unmarshal(env.Data, out)
}

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type gqlReply struct {
	DatabaseID int64  `json:"databaseId"`
	Body       string `json:"body"`
}

type gqlComment struct {
	ID         string `json:"id"`
	DatabaseID int64  `json:"databaseId"`
	Body       string `json:"body"`
	Replies    struct {
		PageInfo pageInfo   `json:"pageInfo"`
		Nodes    []gqlReply `json:"nodes"`
	} `json:"replies"`
}

type gqlComments struct {
	PageInfo pageInfo     `json:"pageInfo"`
	Nodes    []gqlComment `json:"nodes"`
}

type gqlDiscussion struct {
	Number   int         `json:"number"`
	Title    string      `json:"title"`
	Body     string      `json:"body"`
	Comments gqlComments `json:"comments"`
}

const replyFields = `replies(first: 50) { pageInfo { hasNextPage endCursor } nodes { databaseId body } }`

const commentFields = `pageInfo { hasNextPage endCursor } nodes { id databaseId body ` + replyFields + ` }`

const discussionsQuery = `query($owner: String!, $name: String!, $after: String) {
  repository(owner: $owner, name: $name) {
    discussions(first: 25, after: $after) {
      pageInfo { hasNextPage endCursor }
      nodes { number title body comments(first: 20) { ` + commentFields + ` } }
    }
  }
}`

const commentsQuery = `query($owner: String!, $name: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    discussion(number: $number) { comments(first: 50, after: $after) { ` + commentFields + ` } }
  }
}`

const repliesQuery = `query($id: ID!, $after: String) {
  node(id: $id) { ... on DiscussionComment { replies(first: 50, after: $after) { pageInfo { hasNextPage endCursor } nodes { databaseId body } } } }
}`

// discussions scans discussion titles, bodies, comments and replies.
func (s *Scanner) discussions(ctx context.Context, fullName string) error {
	owner, name, _ := strings.Cut(fullName, "/")
	var after any
	for {
		var d struct {
			Repository *struct {
				Discussions struct {
					PageInfo pageInfo        `json:"pageInfo"`
					Nodes    []gqlDiscussion `json:"nodes"`
				} `json:"discussions"`
			} `json:"repository"`
		}
		if err := s.C.graphql(ctx, discussionsQuery, map[string]any{"owner": owner, "name": name, "after": after}, &d); err != nil {
			return err
		}
		if d.Repository == nil {
			return nil
		}
		for _, disc := range d.Repository.Discussions.Nodes {
			loc := fmt.Sprintf("%s discussion %d", fullName, disc.Number)
			s.text(loc, "discussion.title", disc.Title)
			s.text(loc, "discussion.body", disc.Body)
			if err := s.discussionComments(ctx, owner, name, loc, disc.Number, disc.Comments); err != nil && !gone(err) {
				return err
			}
		}
		pi := d.Repository.Discussions.PageInfo
		if !pi.HasNextPage {
			return nil
		}
		after = pi.EndCursor
	}
}

func (s *Scanner) discussionComments(ctx context.Context, owner, name, loc string, number int, page gqlComments) error {
	for {
		for _, c := range page.Nodes {
			cloc := fmt.Sprintf("%s comment %d", loc, c.DatabaseID)
			s.text(cloc, "discussion-comment.body", c.Body)
			if err := s.discussionReplies(ctx, cloc, c); err != nil && !gone(err) {
				return err
			}
		}
		if !page.PageInfo.HasNextPage {
			return nil
		}
		var d struct {
			Repository *struct {
				Discussion *struct {
					Comments gqlComments `json:"comments"`
				} `json:"discussion"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": owner, "name": name, "number": number, "after": page.PageInfo.EndCursor}
		if err := s.C.graphql(ctx, commentsQuery, vars, &d); err != nil {
			return err
		}
		if d.Repository == nil || d.Repository.Discussion == nil {
			return nil
		}
		page = d.Repository.Discussion.Comments
	}
}

func (s *Scanner) discussionReplies(ctx context.Context, loc string, c gqlComment) error {
	replies, pi := c.Replies.Nodes, c.Replies.PageInfo
	for {
		for _, r := range replies {
			s.text(fmt.Sprintf("%s reply %d", loc, r.DatabaseID), "discussion-reply.body", r.Body)
		}
		if !pi.HasNextPage {
			return nil
		}
		var d struct {
			Node *struct {
				Replies struct {
					PageInfo pageInfo   `json:"pageInfo"`
					Nodes    []gqlReply `json:"nodes"`
				} `json:"replies"`
			} `json:"node"`
		}
		if err := s.C.graphql(ctx, repliesQuery, map[string]any{"id": c.ID, "after": pi.EndCursor}, &d); err != nil {
			return err
		}
		if d.Node == nil {
			return nil
		}
		replies, pi = d.Node.Replies.Nodes, d.Node.Replies.PageInfo
	}
}

package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kanywst/kwangmyong/internal/identity"
	"github.com/kanywst/kwangmyong/internal/rules"
)

func TestGraphQLURL(t *testing.T) {
	for base, want := range map[string]string{
		"https://api.github.com":     "https://api.github.com/graphql",
		"https://ghe.example/api/v3": "https://ghe.example/api/graphql",
	} {
		if got := (&Client{Base: base}).graphqlURL(); got != want {
			t.Errorf("%s: got %s, want %s", base, got, want)
		}
	}
}

func TestScanDiscussions(t *testing.T) {
	page := func(next bool, cursor string) map[string]any {
		return map[string]any{"hasNextPage": next, "endCursor": cursor}
	}
	empty := map[string]any{"pageInfo": page(false, ""), "nodes": []any{}}
	mux := http.NewServeMux()
	j := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) { j(w, map[string]string{"login": "x"}) })
	mux.HandleFunc("/orgs/acme/repos", func(w http.ResponseWriter, _ *http.Request) {
		j(w, []map[string]any{
			{"full_name": "acme/app", "has_discussions": true},
			{"full_name": "acme/locked", "has_discussions": true},
			{"full_name": "acme/off"},
		})
	})
	for _, p := range []string{"issues", "issues/comments", "pulls/comments", "releases"} {
		for _, r := range []string{"app", "locked", "off"} {
			mux.HandleFunc("/repos/acme/"+r+"/"+p, func(w http.ResponseWriter, _ *http.Request) { j(w, []any{}) })
		}
	}
	var calls []string
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		v := req.Variables
		switch {
		case v["name"] == "off":
			t.Error("repository without discussions was queried")
		case v["name"] == "locked":
			calls = append(calls, "locked")
			j(w, map[string]any{"data": map[string]any{"repository": nil}, "errors": []any{map[string]any{"type": "NOT_FOUND", "message": "no"}}})
		case strings.Contains(req.Query, "node(id:"):
			calls = append(calls, "replies")
			j(w, map[string]any{"data": map[string]any{"node": map[string]any{"replies": map[string]any{
				"pageInfo": page(false, ""), "nodes": []any{map[string]any{"databaseId": 31, "body": "from /home/tyamada/x"}},
			}}}})
		case strings.Contains(req.Query, "discussion(number:"):
			calls = append(calls, "comments")
			j(w, map[string]any{"data": map[string]any{"repository": map[string]any{"discussion": map[string]any{"comments": map[string]any{
				"pageInfo": page(false, ""), "nodes": []any{map[string]any{"id": "C2", "databaseId": 22, "body": "cc taro@example.com", "replies": empty}},
			}}}}})
		case v["after"] == nil:
			calls = append(calls, "discussions")
			j(w, map[string]any{"data": map[string]any{"repository": map[string]any{"discussions": map[string]any{
				"pageInfo": page(true, "d1"),
				"nodes": []any{map[string]any{"number": 1, "title": "help", "body": "by Taro Yamada", "comments": map[string]any{
					"pageInfo": page(true, "c1"),
					"nodes": []any{map[string]any{"id": "C1", "databaseId": 21, "body": "clean", "replies": map[string]any{
						"pageInfo": page(true, "r1"), "nodes": []any{map[string]any{"databaseId": 30, "body": "clean"}},
					}}},
				}}},
			}}}})
		default:
			calls = append(calls, "discussions")
			j(w, map[string]any{"data": map[string]any{"repository": map[string]any{"discussions": map[string]any{
				"pageInfo": page(false, ""),
				"nodes":    []any{map[string]any{"number": 2, "title": "tyamada was here", "body": "", "comments": empty}},
			}}}})
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	m, err := rules.New(identity.Expand(identity.Identity{
		Names: []string{"Taro Yamada"}, Logins: []string{"tyamada"}, Emails: []string{"taro@example.com"},
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	s := &Scanner{C: &Client{HTTP: srv.Client(), Base: srv.URL, token: "tok"}, M: m}
	got, err := s.Run(context.Background(), Options{Owner: "acme", Discussions: true, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, f := range got {
		seen = append(seen, f.Field+" "+f.Location)
	}
	joined := strings.Join(seen, "\n")
	for _, want := range []string{
		"discussion.body acme/app discussion 1",
		"discussion-comment.body acme/app discussion 1 comment 22",
		"discussion-reply.body acme/app discussion 1 comment 21 reply 31",
		"discussion.title acme/app discussion 2",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in\n%s", want, joined)
		}
	}
	if c := strings.Join(calls, ","); strings.Count(c, "locked") != 1 || strings.Count(c, "discussions") != 2 {
		t.Errorf("unexpected calls: %s", c)
	}

	s = &Scanner{C: &Client{HTTP: srv.Client(), Base: srv.URL, token: "tok"}, M: m}
	calls = nil
	if _, err := s.Run(context.Background(), Options{Owner: "acme", Concurrency: 1}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Errorf("discussions off but GraphQL called: %v", calls)
	}
}

// gqlServer answers /graphql with each response in turn, then the last one.
func gqlServer(t *testing.T, resps ...func(w http.ResponseWriter)) (*Client, *int) {
	t.Helper()
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		i := min(n, len(resps)-1)
		n++
		resps[i](w)
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Base: srv.URL}, &n
}

func gqlBody(v string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) { _, _ = w.Write([]byte(v)) }
}

func TestGraphQLErrors(t *testing.T) {
	limited := func(w http.ResponseWriter) {
		w.Header().Set("Retry-After", "1")
		_, _ = w.Write([]byte(`{"errors":[{"type":"RATE_LIMITED","message":"slow down"}]}`))
	}
	secondary := func(w http.ResponseWriter) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "abuse", http.StatusForbidden)
	}
	ok := gqlBody(`{"data":{"x":1}}`)
	for name, tc := range map[string]struct {
		resps []func(http.ResponseWriter)
		calls int
		check func(error) bool
	}{
		"primary limit is retried":   {[]func(http.ResponseWriter){limited, ok}, 2, func(e error) bool { return e == nil }},
		"secondary limit is retried": {[]func(http.ResponseWriter){secondary, ok}, 2, func(e error) bool { return e == nil }},
		"forbidden is fatal": {[]func(http.ResponseWriter){gqlBody(`{"errors":[{"type":"FORBIDDEN","message":"no"}]}`)}, 1,
			func(e error) bool { return errors.Is(e, ErrDiscussionsForbidden) && !gone(e) }},
		"not found is gone": {[]func(http.ResponseWriter){gqlBody(`{"errors":[{"type":"NOT_FOUND","message":"no"}]}`)}, 1, gone},
		"other errors are fatal": {[]func(http.ResponseWriter){gqlBody(`{"errors":[{"type":"INTERNAL","message":"boom"}]}`)}, 1,
			func(e error) bool { return e != nil && !gone(e) }},
		"http errors name the method": {[]func(http.ResponseWriter){func(w http.ResponseWriter) { http.Error(w, "x", http.StatusBadGateway) }}, 1,
			func(e error) bool { return e != nil && strings.HasPrefix(e.Error(), "POST /graphql") }},
	} {
		t.Run(name, func(t *testing.T) {
			c, n := gqlServer(t, tc.resps...)
			var out map[string]any
			err := c.graphql(context.Background(), "q", nil, &out)
			if !tc.check(err) || *n != tc.calls {
				t.Errorf("err=%v calls=%d", err, *n)
			}
		})
	}
}

func TestDiscussionVanishesMidScan(t *testing.T) {
	m, err := rules.New(identity.Expand(identity.Identity{Logins: []string{"tyamada"}}), nil)
	if err != nil {
		t.Fatal(err)
	}
	first := `{"data":{"repository":{"discussions":{"pageInfo":{"hasNextPage":false},"nodes":[
		{"number":1,"title":"a","body":"","comments":{"pageInfo":{"hasNextPage":true,"endCursor":"c"},"nodes":[]}},
		{"number":2,"title":"tyamada leaked","body":"","comments":{"pageInfo":{"hasNextPage":false},"nodes":[]}}]}}}}`
	c, _ := gqlServer(t, gqlBody(first), gqlBody(`{"data":{"repository":{"discussion":null}},"errors":[{"type":"NOT_FOUND","message":"gone"}]}`))
	s := &Scanner{C: c, M: m}
	if err := s.discussions(context.Background(), "acme/app"); err != nil {
		t.Fatal(err)
	}
	if len(s.out) != 1 || s.out[0].Location != "acme/app discussion 2" {
		t.Errorf("discussion 2 should still be scanned: %+v", s.out)
	}
}

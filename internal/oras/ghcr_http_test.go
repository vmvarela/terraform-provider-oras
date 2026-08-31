// HTTP-level tests for the GHCR Packages REST API fallback (ghcr.go).
//
// deleteGitHubPackageVersionByTag and friends accept an injected *http.Client
// and base URL, so the GitHub API is faked with httptest and exercised over
// real HTTP: headers, pagination, status codes, and payload shapes.
package oras

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// recordedRequest is a captured incoming request.
type recordedRequest struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
}

// newGHCRAPITestServer starts an httptest server that records every request
// and delegates the response to h. Returns the server and a function that
// snapshots the requests seen so far.
func newGHCRAPITestServer(t *testing.T, h func(w http.ResponseWriter, r recordedRequest)) (*httptest.Server, func() []recordedRequest) {
	t.Helper()
	var mu sync.Mutex
	var reqs []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.Query(),
			Header: r.Header.Clone(),
		}
		mu.Lock()
		reqs = append(reqs, rec)
		mu.Unlock()
		h(w, rec)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedRequest(nil), reqs...)
	}
}

// roundTripFunc adapts a function into an http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fakeGitHubResponse builds a canned HTTP response for roundTripFunc.
func fakeGitHubResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     strconv.Itoa(status) + " " + http.StatusText(status),
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

// ghcrVersionsBody marshals versions (id -> tags) as the API's version list.
func ghcrVersionsBody(versions map[int64][]string) string {
	out := make([]githubPackageVersion, 0, len(versions))
	for id, tags := range versions {
		var v githubPackageVersion
		v.ID = id
		v.Metadata.Container.Tags = tags
		out = append(out, v)
	}
	b, _ := json.Marshal(out) //nolint:errcheck // marshaling this struct cannot fail
	return string(b)
}

// checkGitHubHeaders asserts the headers githubRequest must send.
func checkGitHubHeaders(t *testing.T, h http.Header, token string) {
	t.Helper()
	if got := h.Get("Authorization"); got != "Bearer "+token {
		t.Errorf("Authorization = %q, want %q", got, "Bearer "+token)
	}
	if got := h.Get("Accept"); got != "application/vnd.github+json" {
		t.Errorf("Accept = %q, want %q", got, "application/vnd.github+json")
	}
	if got := h.Get("X-GitHub-Api-Version"); got != githubAPIVersion {
		t.Errorf("X-GitHub-Api-Version = %q, want %q", got, githubAPIVersion)
	}
	if h.Get("User-Agent") == "" {
		t.Error("User-Agent header is empty")
	}
}

// serveVersionsAndDelete returns a handler that answers GET .../versions with
// the given page bodies (keyed by page number) and DELETE with delStatus.
func serveVersionsAndDelete(pages map[string]string, delStatus int) func(http.ResponseWriter, recordedRequest) {
	return func(w http.ResponseWriter, r recordedRequest) {
		if r.Method == http.MethodGet {
			body, ok := pages[r.Query.Get("page")]
			if !ok {
				http.Error(w, "unexpected page", http.StatusBadRequest)
				return
			}
			fmt.Fprint(w, body) //nolint:errcheck // test response write
			return
		}
		w.WriteHeader(delStatus)
	}
}

func TestDeleteGitHubPackageVersionByTag_Success(t *testing.T) {
	ctx := context.Background()
	srv, reqs := newGHCRAPITestServer(t, func(w http.ResponseWriter, r recordedRequest) {
		checkGitHubHeaders(t, r.Header, "tok-123")
		switch {
		case r.Method == http.MethodGet && r.Path == "/orgs/myorg/packages/container/myrepo/versions":
			if got := r.Query.Get("per_page"); got != strconv.Itoa(githubVersionsPerPage) {
				t.Errorf("per_page = %q, want %d", got, githubVersionsPerPage)
			}
			if got := r.Query.Get("page"); got != "1" {
				t.Errorf("page = %q, want 1", got)
			}
			fmt.Fprint(w, ghcrVersionsBody(map[int64][]string{42: {"state-default"}})) //nolint:errcheck // test response write
		case r.Method == http.MethodDelete && r.Path == "/orgs/myorg/packages/container/myrepo/versions/42":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request: "+r.Method+" "+r.Path, http.StatusBadRequest)
		}
	})

	err := deleteGitHubPackageVersionByTag(ctx, srv.Client(), srv.URL, "myorg", "myrepo", "state-default", "tok-123")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	seen := reqs()
	if len(seen) != 2 {
		t.Fatalf("got %d requests, want 2", len(seen))
	}
	if seen[0].Method != http.MethodGet || seen[1].Method != http.MethodDelete {
		t.Errorf("methods = [%s %s], want [GET DELETE]", seen[0].Method, seen[1].Method)
	}
}

func TestDeleteGitHubPackageVersionByTag_Org404FallsBackToUser(t *testing.T) {
	ctx := context.Background()
	srv, reqs := newGHCRAPITestServer(t, func(w http.ResponseWriter, r recordedRequest) {
		checkGitHubHeaders(t, r.Header, "tok")
		if strings.HasPrefix(r.Path, "/orgs/") {
			// Org-scoped package does not exist.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// User-scoped package exists.
		if r.Method == http.MethodGet {
			fmt.Fprint(w, ghcrVersionsBody(map[int64][]string{7: {"state-default"}})) //nolint:errcheck // test response write
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	err := deleteGitHubPackageVersionByTag(ctx, srv.Client(), srv.URL, "myorg", "myrepo", "state-default", "tok")
	if err != nil {
		t.Fatalf("expected user-endpoint fallback to succeed, got %v", err)
	}

	seen := reqs()
	// Org GET 404s, then the user endpoint does the full lookup + delete.
	if len(seen) != 3 {
		t.Fatalf("got %d requests, want 3 (org GET, user GET, user DELETE)", len(seen))
	}
	last := seen[len(seen)-1]
	if last.Method != http.MethodDelete || !strings.HasPrefix(last.Path, "/users/") {
		t.Errorf("last request = %s %s, want DELETE /users/...", last.Method, last.Path)
	}
}

func TestDeleteGitHubPackageVersionByTag_TagNotFound(t *testing.T) {
	t.Run("empty version list", func(t *testing.T) {
		ctx := context.Background()
		srv, reqs := newGHCRAPITestServer(t, serveVersionsAndDelete(map[string]string{"1": "[]"}, 0))

		err := deleteGitHubPackageVersionByTag(ctx, srv.Client(), srv.URL, "myorg", "myrepo", "missing-tag", "tok")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !isHTTPStatus(err, http.StatusNotFound) {
			t.Errorf("error = %v, want httpStatusErr 404", err)
		}
		if !strings.Contains(err.Error(), "tag not found") {
			t.Errorf("error = %v, want it to mention 'tag not found'", err)
		}
		// A 404 from the org endpoint triggers the user-endpoint fallback,
		// which repeats the lookup: one GET per endpoint.
		if n := len(reqs()); n != 2 {
			t.Errorf("requests = %d, want 2 (org GET, user GET)", n)
		}
	})

	t.Run("versions with non-matching tags", func(t *testing.T) {
		ctx := context.Background()
		srv, reqs := newGHCRAPITestServer(t, serveVersionsAndDelete(map[string]string{
			"1": ghcrVersionsBody(map[int64][]string{1: {"other"}, 2: {"another"}}),
			"2": "[]",
		}, 0))

		err := deleteGitHubPackageVersionByTag(ctx, srv.Client(), srv.URL, "myorg", "myrepo", "missing-tag", "tok")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !isHTTPStatus(err, http.StatusNotFound) {
			t.Errorf("error = %v, want httpStatusErr 404", err)
		}
		// Page 2 (empty) is consulted per endpoint, and the user endpoint is
		// retried after the org endpoint reports the tag missing.
		if n := len(reqs()); n != 4 {
			t.Errorf("requests = %d, want 4 (org p1, org p2, user p1, user p2)", n)
		}
	})
}

func TestDeleteGitHubPackageVersionByTag_ListErrors(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantStatus int // expected httpStatusErr code; 0 means expect a non-status (decode) error
	}{
		{name: "401 unauthorized", status: http.StatusUnauthorized, body: "denied", wantStatus: http.StatusUnauthorized},
		{name: "403 forbidden", status: http.StatusForbidden, body: "forbidden", wantStatus: http.StatusForbidden},
		{name: "404 package not found", status: http.StatusNotFound, body: "no package", wantStatus: http.StatusNotFound},
		{name: "500 server error", status: http.StatusInternalServerError, body: "boom", wantStatus: http.StatusInternalServerError},
		{name: "503 service unavailable", status: http.StatusServiceUnavailable, body: "later", wantStatus: http.StatusServiceUnavailable},
		{name: "malformed json body", status: http.StatusOK, body: "not-json-at-all", wantStatus: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			srv, _ := newGHCRAPITestServer(t, func(w http.ResponseWriter, r recordedRequest) {
				w.WriteHeader(tt.status)
				fmt.Fprint(w, tt.body) //nolint:errcheck // test response write
			})

			err := deleteGitHubPackageVersionByTag(ctx, srv.Client(), srv.URL, "myorg", "myrepo", "state-default", "tok")
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if tt.wantStatus == 0 {
				if isHTTPStatus(err, tt.status) {
					t.Errorf("decode failure must not masquerade as status error: %v", err)
				}
				return
			}
			if !isHTTPStatus(err, tt.wantStatus) {
				t.Errorf("error = %v, want httpStatusErr %d", err, tt.wantStatus)
			}
		})
	}
}

func TestDeleteGitHubPackageVersionByTag_DeleteFailureModes(t *testing.T) {
	tests := []struct {
		name       string
		delStatus  int
		wantStatus int
	}{
		{name: "403 forbidden on delete", delStatus: http.StatusForbidden, wantStatus: http.StatusForbidden},
		{name: "422 unprocessable on delete", delStatus: http.StatusUnprocessableEntity, wantStatus: http.StatusUnprocessableEntity},
		{name: "200 instead of expected 204", delStatus: http.StatusOK, wantStatus: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			srv, _ := newGHCRAPITestServer(t, serveVersionsAndDelete(
				map[string]string{"1": ghcrVersionsBody(map[int64][]string{42: {"state-default"}})},
				tt.delStatus,
			))

			err := deleteGitHubPackageVersionByTag(ctx, srv.Client(), srv.URL, "myorg", "myrepo", "state-default", "tok")
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !isHTTPStatus(err, tt.wantStatus) {
				t.Errorf("error = %v, want httpStatusErr %d", err, tt.wantStatus)
			}
		})
	}
}

func TestFindGitHubVersionIDByTag_PaginatesUntilTagFound(t *testing.T) {
	ctx := context.Background()
	pages := map[string]string{
		"1": ghcrVersionsBody(map[int64][]string{1: {"state-a"}}),
		"2": ghcrVersionsBody(map[int64][]string{2: {"state-b"}}),
		"3": ghcrVersionsBody(map[int64][]string{3: {"state-ws-default"}}),
	}
	srv, reqs := newGHCRAPITestServer(t, serveVersionsAndDelete(pages, 0))

	base := srv.URL + "/orgs/myorg/packages/container/myrepo"
	id, err := findGitHubVersionIDByTag(ctx, srv.Client(), base, "state-ws-default", "tok")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if id != 3 {
		t.Errorf("id = %d, want 3", id)
	}

	seen := reqs()
	if len(seen) != 3 {
		t.Fatalf("requests = %d, want 3", len(seen))
	}
	for i, r := range seen {
		if got := r.Query.Get("page"); got != strconv.Itoa(i+1) {
			t.Errorf("request %d page = %q, want %d", i, got, i+1)
		}
	}
}

func TestFindGitHubVersionIDByTag_StopsAtMaxPages(t *testing.T) {
	// A tag that never appears must stop after githubMaxVersionPages pages,
	// not loop forever against a server that always returns versions.
	ctx := context.Background()
	body := ghcrVersionsBody(map[int64][]string{1: {"other"}})
	srv, reqs := newGHCRAPITestServer(t, func(w http.ResponseWriter, r recordedRequest) {
		fmt.Fprint(w, body) //nolint:errcheck // test response write
	})

	id, err := findGitHubVersionIDByTag(ctx, srv.Client(), srv.URL, "never-present", "tok")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if id != 0 {
		t.Errorf("id = %d, want 0", id)
	}
	if n := len(reqs()); n != githubMaxVersionPages {
		t.Errorf("requests = %d, want %d", n, githubMaxVersionPages)
	}
}

func TestGitHubRequest(t *testing.T) {
	ctx := context.Background()

	t.Run("decodes body on expected status", func(t *testing.T) {
		srv, _ := newGHCRAPITestServer(t, func(w http.ResponseWriter, r recordedRequest) {
			checkGitHubHeaders(t, r.Header, "tok")
			fmt.Fprint(w, `[{"id":5}]`) //nolint:errcheck // test response write
		})

		var got []githubPackageVersion
		err := githubRequest(ctx, srv.Client(), http.MethodGet, srv.URL+"/x", "tok", "op", http.StatusOK, &got)
		if err != nil {
			t.Fatalf("githubRequest: %v", err)
		}
		if len(got) != 1 || got[0].ID != 5 {
			t.Errorf("decoded = %+v, want one version with ID 5", got)
		}
	})

	t.Run("nil decode with 204", func(t *testing.T) {
		srv, _ := newGHCRAPITestServer(t, func(w http.ResponseWriter, r recordedRequest) {
			w.WriteHeader(http.StatusNoContent)
		})

		err := githubRequest(ctx, srv.Client(), http.MethodDelete, srv.URL+"/x", "tok", "op", http.StatusNoContent, nil)
		if err != nil {
			t.Fatalf("githubRequest: %v", err)
		}
	})

	t.Run("status mismatch yields httpStatusErr", func(t *testing.T) {
		srv, _ := newGHCRAPITestServer(t, func(w http.ResponseWriter, r recordedRequest) {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "boom") //nolint:errcheck // test response write
		})

		err := githubRequest(ctx, srv.Client(), http.MethodGet, srv.URL+"/x", "tok", "list package versions", http.StatusNoContent, nil)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !isHTTPStatus(err, http.StatusInternalServerError) {
			t.Errorf("error = %v, want httpStatusErr 500", err)
		}
		if !strings.Contains(err.Error(), "list package versions") || !strings.Contains(err.Error(), "500") {
			t.Errorf("error = %q, want it to mention the operation and status", err)
		}
	})
}

func TestFindVersionIDWithTag(t *testing.T) {
	var v1, v2 githubPackageVersion
	v1.ID = 1
	v1.Metadata.Container.Tags = []string{"a", "b"}
	v2.ID = 2
	v2.Metadata.Container.Tags = []string{"c"}

	tests := []struct {
		name     string
		versions []githubPackageVersion
		tag      string
		want     int64
	}{
		{name: "found in first version", versions: []githubPackageVersion{v1, v2}, tag: "b", want: 1},
		{name: "found in later version", versions: []githubPackageVersion{v1, v2}, tag: "c", want: 2},
		{name: "not found", versions: []githubPackageVersion{v1, v2}, tag: "z", want: 0},
		{name: "empty versions", versions: nil, tag: "a", want: 0},
		{name: "version with no tags", versions: func() []githubPackageVersion { var v githubPackageVersion; v.ID = 9; return []githubPackageVersion{v} }(), tag: "a", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := findVersionIDWithTag(tt.versions, tt.tag); got != tt.want {
				t.Errorf("findVersionIDWithTag = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestTryDeleteGHCRTag_PassesTokenAndClientToGitHubAPI(t *testing.T) {
	// tryDeleteGHCRTag hardcodes the api.github.com base URL, so the URL can't
	// be pointed at a test server. Instead, intercept at the transport level:
	// this proves repo.httpClient is used and the resolved token authenticates
	// the GitHub API request.
	var gotReqs []*http.Request
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotReqs = append(gotReqs, r)
		if r.Method == http.MethodGet {
			return fakeGitHubResponse(http.StatusOK,
				ghcrVersionsBody(map[int64][]string{7: {"state-default"}})), nil
		}
		return fakeGitHubResponse(http.StatusNoContent, ""), nil
	})}

	repo := &orasRepositoryClient{
		inner:      newFakeORASRepo(),
		repository: "ghcr.io/myorg/myrepo",
		token:      "tok-abc",
		httpClient: client,
	}

	if err := tryDeleteGHCRTag(context.Background(), repo, "state-default"); err != nil {
		t.Fatalf("tryDeleteGHCRTag: %v", err)
	}

	if len(gotReqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(gotReqs))
	}
	if got := gotReqs[0].Header.Get("Authorization"); got != "Bearer tok-abc" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer tok-abc")
	}
	if got := gotReqs[1].URL.Path; got != "/orgs/myorg/packages/container/myrepo/versions/7" {
		t.Errorf("delete path = %q", got)
	}
}

func TestTryDeleteGHCRTag_PropagatesGitHubAPIError(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return fakeGitHubResponse(http.StatusUnauthorized, "denied"), nil
	})}

	repo := &orasRepositoryClient{
		inner:      newFakeORASRepo(),
		repository: "ghcr.io/myorg/myrepo",
		token:      "bad-token",
		httpClient: client,
	}

	err := tryDeleteGHCRTag(context.Background(), repo, "state-default")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !isHTTPStatus(err, http.StatusUnauthorized) {
		t.Errorf("error = %v, want httpStatusErr 401", err)
	}
}

func TestTryDeleteGHCRTag_NoToken(t *testing.T) {
	repo := &orasRepositoryClient{
		inner:      newFakeORASRepo(),
		repository: "ghcr.io/myorg/myrepo",
	}

	err := tryDeleteGHCRTag(context.Background(), repo, "state-default")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no credentials available") {
		t.Errorf("error = %v, want it to mention missing credentials", err)
	}
}

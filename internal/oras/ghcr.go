// GHCR tag deletion support.
//
// GitHub Container Registry (ghcr.io) does not support the standard OCI
// manifest deletion endpoint (DELETE /v2/.../manifests/<digest>); it returns
// HTTP 405. As a fallback, this file implements tag deletion via the GitHub
// Packages REST API (https://docs.github.com/en/rest/packages).
package oras

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

var errNotGHCR = errors.New("not a ghcr.io repository")

const (
	ghcrHost              = "ghcr.io"
	githubAPIBaseURL      = "https://api.github.com"
	githubAPIVersion      = "2022-11-28"
	githubVersionsPerPage = 100
)

type githubPackageVersion struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"` // full digest of the manifest, e.g. "sha256:..."
	Metadata struct {
		Container struct {
			Tags []string `json:"tags"`
		} `json:"container"`
	} `json:"metadata"`
}

func tryDeleteGHCRTag(ctx context.Context, repo *orasRepositoryClient, tag, expectedDigest string) error {
	if repo == nil {
		return fmt.Errorf("nil repository client")
	}

	host, owner, packageName, err := parseGHCRRepository(repo.repository)
	if err != nil {
		return err
	}
	if host != ghcrHost {
		return errNotGHCR
	}

	token, err := repo.accessToken(ctx)
	if err != nil {
		return err
	}
	if token == "" {
		return fmt.Errorf("no credentials available for %s (need a token with delete:packages)", host)
	}

	client := repo.httpClient
	if client == nil {
		client = &http.Client{}
	}

	return deleteGitHubPackageVersionByTag(ctx, client, githubAPIBaseURL, owner, packageName, tag, token, expectedDigest)
}

func parseGHCRRepository(repository string) (host, owner, packageName string, err error) {
	trimmed := strings.TrimSpace(repository)
	parts := strings.Split(trimmed, "/")
	if len(parts) < 3 {
		return "", "", "", fmt.Errorf("invalid repository %q: expected <host>/<owner>/<name>", repository)
	}

	host = parts[0]
	owner = parts[1]
	packageName = strings.Join(parts[2:], "/")

	if host == "" || owner == "" || packageName == "" {
		return "", "", "", fmt.Errorf("invalid repository %q: empty segment", repository)
	}
	return host, owner, packageName, nil
}

func deleteGitHubPackageVersionByTag(ctx context.Context, client *http.Client, baseURL, owner, packageName, tag, token, expectedDigest string) error {
	baseURL = strings.TrimRight(baseURL, "/")
	pkgEscaped := url.PathEscape(packageName)
	ownerEscaped := url.PathEscape(owner)

	orgBase := fmt.Sprintf("%s/orgs/%s/packages/container/%s", baseURL, ownerEscaped, pkgEscaped)
	if err := deleteFromGitHubPackagesEndpoint(ctx, client, orgBase, tag, token, expectedDigest); err == nil {
		return nil
	} else if !isHTTPStatus(err, http.StatusNotFound) {
		return err
	}

	userBase := fmt.Sprintf("%s/users/%s/packages/container/%s", baseURL, ownerEscaped, pkgEscaped)
	return deleteFromGitHubPackagesEndpoint(ctx, client, userBase, tag, token, expectedDigest)
}

func deleteFromGitHubPackagesEndpoint(ctx context.Context, client *http.Client, baseURL, tag, token, expectedDigest string) error {
	versionID, err := findGitHubVersionIDByTag(ctx, client, baseURL, tag, token, expectedDigest)
	if err != nil {
		return err
	}
	if versionID == 0 {
		return newHTTPStatusError(http.StatusNotFound, "no package version matching tag and digest")
	}

	deleteURL := fmt.Sprintf("%s/versions/%d", baseURL, versionID)
	return githubRequest(ctx, client, http.MethodDelete, deleteURL, token, "delete package version", http.StatusNoContent, nil)
}

// findGitHubVersionIDByTag pages through the package's versions looking for a
// version whose tags contain tag AND whose top-level "name" digest equals
// expectedDigest. A tag-only match is not enough: the DELETE by version ID
// removes the digest and ALL its tags, so a digest mismatch would destroy
// another manifest sharing the tag. Paging stops at an empty or short page
// (fewer than githubVersionsPerPage entries means the API returned
// everything); context cancellation surfaces as an error.
func findGitHubVersionIDByTag(ctx context.Context, client *http.Client, baseURL, tag, token, expectedDigest string) (int64, error) {
	for page := 1; ; page++ {
		versions, err := listGitHubPackageVersions(ctx, client, baseURL, page, token)
		if err != nil {
			return 0, err
		}
		if len(versions) == 0 {
			break
		}

		if id := findVersionIDWithTag(versions, tag, expectedDigest); id != 0 {
			return id, nil
		}
		if len(versions) < githubVersionsPerPage {
			break
		}
	}
	return 0, nil
}

func listGitHubPackageVersions(ctx context.Context, client *http.Client, baseURL string, page int, token string) ([]githubPackageVersion, error) {
	versionsURL, err := url.Parse(baseURL + "/versions")
	if err != nil {
		return nil, err
	}
	query := versionsURL.Query()
	query.Set("per_page", strconv.Itoa(githubVersionsPerPage))
	query.Set("page", strconv.Itoa(page))
	versionsURL.RawQuery = query.Encode()

	var versions []githubPackageVersion
	if err := githubRequest(ctx, client, http.MethodGet, versionsURL.String(), token, "list package versions", http.StatusOK, &versions); err != nil {
		return nil, err
	}
	return versions, nil
}

// findVersionIDWithTag returns the ID of the first version matching BOTH the
// tag and the expected digest ("name" field). Anything else — tag hit on a
// different digest, digest hit without the tag — is not a match.
func findVersionIDWithTag(versions []githubPackageVersion, tag, expectedDigest string) int64 {
	for _, v := range versions {
		if v.Name != expectedDigest {
			continue
		}
		for _, t := range v.Metadata.Container.Tags {
			if t == tag {
				return v.ID
			}
		}
	}
	return 0
}

func githubRequest(ctx context.Context, client *http.Client, method, urlStr, token, operation string, expectedStatus int, decode any) error {
	req, err := http.NewRequestWithContext(ctx, method, urlStr, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", userAgent())

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != expectedStatus {
		// Drain the body so the underlying TCP connection can be reused (HTTP/1.1 keep-alive).
		_, _ = io.Copy(io.Discard, resp.Body)
		return newHTTPStatusError(resp.StatusCode, operation)
	}
	if decode == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(decode)
}

type httpStatusErr struct {
	code int
	op   string
}

func (e httpStatusErr) Error() string {
	return fmt.Sprintf("github api error (%s): status %d", e.op, e.code)
}

func newHTTPStatusError(code int, op string) error {
	return httpStatusErr{code: code, op: op}
}

func isHTTPStatus(err error, code int) bool {
	var e httpStatusErr
	if errors.As(err, &e) {
		return e.code == code
	}
	return false
}

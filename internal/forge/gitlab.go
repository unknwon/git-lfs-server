package forge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/cockroachdb/errors"

	"unknwon.dev/git-lfs-server/internal/logx"
)

var _ Provider = (*GitLabProvider)(nil)

type GitLabProvider struct {
	host      string
	apiBase   string
	client    *http.Client
	allowlist *RepoAllowlist
}

func (*GitLabProvider) Type() Type { return TypeGitLab }

func (p *GitLabProvider) Allow(repo string) bool { return p.allowlist.Allow(repo) }

func NewGitLabProvider(host string, allowlist *RepoAllowlist) *GitLabProvider {
	return &GitLabProvider{
		host:      host,
		apiBase:   gitlabAPIBase(host),
		client:    &http.Client{Timeout: 30 * time.Second},
		allowlist: allowlist,
	}
}

// gitlabAPIBase resolves the REST API root for a forge host. gitlab.com and
// self-managed GitLab both serve the same v4 API on the same hostname.
func gitlabAPIBase(host string) string {
	return "https://" + host + "/api/v4"
}

// GitLab numeric access levels. Mirrors the values in the upstream API
// reference; only the levels relevant to LFS access are named here.
//
// Reference: https://docs.gitlab.com/api/access_requests/#valid-access-levels
const (
	gitlabAccessReporter  = 20
	gitlabAccessDeveloper = 30
)

type gitlabProjectResponse struct {
	Permissions struct {
		ProjectAccess *struct {
			AccessLevel int `json:"access_level"`
		} `json:"project_access"`
		GroupAccess *struct {
			AccessLevel int `json:"access_level"`
		} `json:"group_access"`
	} `json:"permissions"`
}

func (p *GitLabProvider) Authorize(ctx context.Context, logger *logx.Logger, repo, token string) (Permission, time.Duration, error) {
	// GitLab supports nested groups, so the repo path may contain multiple
	// "/" separators. The full path must be percent-encoded as a single
	// path segment, e.g., "group/sub/project" -> "group%2Fsub%2Fproject".
	endpoint := p.apiBase + "/projects/" + url.PathEscape(repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", -1, errors.Wrap(err, "create request")
	}
	// Bearer accepts both personal access tokens and OAuth tokens, so a
	// single header form works for the credential types Git LFS clients
	// realistically pass through.
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", -1, errors.Wrap(err, "call GitLab API")
	}
	defer func() {
		// Drain before close so the underlying connection is eligible for
		// keep-alive reuse on the early-return error paths below.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		// GitLab returns 404 both for nonexistent projects and for projects
		// the token can't see, so we can't distinguish "wrong repo" from
		// "wrong token" here. Treat all three as token invalid.
		return "", -1, errors.WithStack(ErrTokenInvalid)
	default:
		return "", -1, errors.Newf("unexpected status %d from GitLab API", resp.StatusCode)
	}

	var body gitlabProjectResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", -1, errors.Wrap(err, "decode GitLab API response")
	}

	perm, ok := gitlabPermissionFromResponse(&body)
	if !ok {
		return "", -1, errors.WithStack(ErrTokenInvalid)
	}
	// GitLab does not advertise token expiry on regular API calls; learning
	// it would require a separate /personal_access_tokens/self round-trip on
	// every request. Return zero so the caller applies its conservative
	// default TTL instead.
	return perm, 0, nil
}

// gitlabPermissionFromResponse derives the effective Permission from a
// /projects/:id response. The user's access on a project is the max of
// project_access and group_access (either may be null). ok is false when
// neither field grants at least Reporter.
func gitlabPermissionFromResponse(body *gitlabProjectResponse) (Permission, bool) {
	level := 0
	if a := body.Permissions.ProjectAccess; a != nil && a.AccessLevel > level {
		level = a.AccessLevel
	}
	if a := body.Permissions.GroupAccess; a != nil && a.AccessLevel > level {
		level = a.AccessLevel
	}
	switch {
	case level >= gitlabAccessDeveloper:
		return PermissionWrite, true
	case level >= gitlabAccessReporter:
		return PermissionRead, true
	default:
		return "", false
	}
}

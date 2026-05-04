package forge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/cockroachdb/errors"

	"unknwon.dev/git-lfs-server/internal/logx"
)

var _ Provider = (*GitHubProvider)(nil)

type GitHubProvider struct {
	host      string
	apiBase   string
	client    *http.Client
	allowlist *RepoAllowlist
}

func (*GitHubProvider) Type() Type { return TypeGitHub }

func (p *GitHubProvider) Allow(repo string) bool { return p.allowlist.Allow(repo) }

func NewGitHubProvider(host string, allowlist *RepoAllowlist) *GitHubProvider {
	return &GitHubProvider{
		host:      host,
		apiBase:   githubAPIBase(host),
		client:    &http.Client{Timeout: 30 * time.Second},
		allowlist: allowlist,
	}
}

// githubAPIBase resolves the REST API root for a forge host. github.com is
// served from a dedicated subdomain. GitHub Enterprise Server exposes the
// same API under "/api/v3" on the appliance host.
func githubAPIBase(host string) string {
	if host == "github.com" {
		return "https://api.github.com"
	}
	return "https://" + host + "/api/v3"
}

type githubRepoResponse struct {
	Permissions struct {
		Pull bool `json:"pull"`
		Push bool `json:"push"`
	} `json:"permissions"`
}

// githubExpirationHeader is the response header GitHub sets on token-bearing
// API calls to advertise when the token expires.
const githubExpirationHeader = "github-authentication-token-expiration"

// githubExpirationLayouts holds the wire formats observed for
// githubExpirationHeader. Classic PATs emit the zone-abbreviation form
// (e.g. "2024-04-27 20:14:21 UTC"). Fine-grained PATs and GitHub App user
// tokens emit the numeric-offset form reflecting the token owner's timezone
// (e.g. "2025-09-10 02:30:13 +0200"). GitHub does not document the format,
// so we mirror the dual-layout strategy used by google/go-github (issue #2649).
var githubExpirationLayouts = []string{
	"2006-01-02 15:04:05 MST",
	"2006-01-02 15:04:05 -0700",
}

func (p *GitHubProvider) Authorize(ctx context.Context, logger *logx.Logger, repo, username, token string) (Permission, time.Duration, error) {
	url := p.apiBase + "/repos/" + repo
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", -1, errors.Wrap(err, "create request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", -1, errors.Wrap(err, "call GitHub API")
	}
	defer func() {
		// Drain before close so the underlying connection is eligible for
		// keep-alive reuse on the early-return error paths below.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusNotFound:
		return "", -1, errors.WithStack(ErrTokenInvalid)
	default:
		return "", -1, errors.Newf("unexpected status %d from GitHub API", resp.StatusCode)
	}

	var body githubRepoResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", -1, errors.Wrap(err, "decode GitHub API response")
	}

	var perm Permission
	switch {
	case body.Permissions.Push:
		perm = PermissionWrite
	case body.Permissions.Pull:
		perm = PermissionRead
	default:
		return "", -1, errors.WithStack(ErrTokenInvalid)
	}
	return perm, githubTokenTTL(ctx, logger, resp.Header.Get(githubExpirationHeader)), nil
}

// githubTokenTTL returns the raw time until the token expires based on the
// GitHub-Authentication-Token-Expiration response header. The return value
// follows the forge.Provider contract:
//   - Zero: the header was absent (classic PATs and OAuth tokens do not emit
//     it). The caller may apply a conservative default TTL.
//   - Negative: the header was present but could not be parsed against any
//     known layout. This signals an undocumented format change on GitHub's
//     side, so the caller must fail closed and skip caching rather than fall
//     back to a default TTL on an expiry signal we no longer trust.
//   - Positive: the time until the parsed expiry. The caller applies any
//     safety margin and maximum cap before caching.
func githubTokenTTL(ctx context.Context, logger *logx.Logger, header string) time.Duration {
	if header == "" {
		return 0
	}
	var expiry time.Time
	var parseErr error
	for _, layout := range githubExpirationLayouts {
		expiry, parseErr = time.Parse(layout, header)
		if parseErr == nil {
			break
		}
	}
	if parseErr != nil {
		logger.WarnContext(ctx, "Failed to parse GitHub token expiration header",
			"header", githubExpirationHeader, "value", header, "error", parseErr)
		return -1
	}
	return time.Until(expiry)
}

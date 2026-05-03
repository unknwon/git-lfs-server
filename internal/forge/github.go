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
	client    *http.Client
	allowlist *RepoAllowlist
}

func (*GitHubProvider) Type() Type { return TypeGitHub }

func (p *GitHubProvider) Allow(repo string) bool { return p.allowlist.Allow(repo) }

func NewGitHubProvider(host string, allowlist *RepoAllowlist) *GitHubProvider {
	return &GitHubProvider{
		host:      host,
		client:    &http.Client{Timeout: 30 * time.Second},
		allowlist: allowlist,
	}
}

type githubRepoResponse struct {
	Permissions struct {
		Pull bool `json:"pull"`
		Push bool `json:"push"`
	} `json:"permissions"`
}

// Cache TTL policy for permission decisions returned from the GitHub API.
//
// GitHub returns the github-authentication-token-expiration response header for
// fine-grained PATs and GitHub App user tokens. When present, we cache the
// decision until margin before the token expires (so the cache never serves a
// decision tied to an already-invalid token). When absent — classic PATs and
// OAuth tokens — we fall back to defaultTTL. maxTTL caps both paths so a
// long-lived token never sits in the cache for an unbounded window.
const (
	githubDefaultTTL = 5 * time.Minute
	githubMaxTTL     = 1 * time.Hour
	githubMargin     = 5 * time.Minute
)

const githubExpirationHeader = "github-authentication-token-expiration"

// Wire formats observed for the github-authentication-token-expiration header.
// Classic PATs emit the zone-abbreviation form (e.g. "2024-04-27 20:14:21 UTC");
// fine-grained PATs and GitHub App user tokens emit the numeric-offset form
// reflecting the token owner's timezone (e.g. "2025-09-10 02:30:13 +0200").
// GitHub does not document the format, so we mirror the dual-layout strategy
// used by google/go-github (issue #2649).
var githubExpirationLayouts = []string{
	"2006-01-02 15:04:05 MST",
	"2006-01-02 15:04:05 -0700",
}

func (p *GitHubProvider) Authorize(ctx context.Context, logger *logx.Logger, repo, token string) (Permission, time.Duration, error) {
	url := "https://api.github.com/repos/" + repo
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, errors.Wrap(err, "create request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", 0, errors.Wrap(err, "call GitHub API")
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
		return "", 0, errors.WithStack(ErrTokenInvalid)
	default:
		return "", 0, errors.Newf("unexpected status %d from GitHub API", resp.StatusCode)
	}

	var body githubRepoResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", 0, errors.Wrap(err, "decode GitHub API response")
	}

	var perm Permission
	switch {
	case body.Permissions.Push:
		perm = PermissionWrite
	case body.Permissions.Pull:
		perm = PermissionRead
	default:
		return "", 0, errors.WithStack(ErrTokenInvalid)
	}
	return perm, githubCacheTTL(ctx, logger, resp.Header.Get(githubExpirationHeader)), nil
}

func githubCacheTTL(ctx context.Context, logger *logx.Logger, header string) time.Duration {
	if header == "" {
		return githubDefaultTTL
	}
	var expiry time.Time
	var parseErr error
	for _, layout := range githubExpirationLayouts {
		t, err := time.Parse(layout, header)
		if err == nil {
			expiry = t
			parseErr = nil
			break
		}
		parseErr = err
	}
	if expiry.IsZero() {
		logger.WarnContext(ctx, "Failed to parse GitHub token expiration header, falling back to default TTL",
			"header", githubExpirationHeader, "value", header, "error", parseErr)
		return githubDefaultTTL
	}
	ttl := time.Until(expiry) - githubMargin
	if ttl <= 0 {
		return -1
	}
	if ttl > githubMaxTTL {
		return githubMaxTTL
	}
	return ttl
}

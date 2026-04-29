package forge

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/cockroachdb/errors"

	"unknwon.dev/git-lfs-server/internal/logx"
)

var _ Provider = (*GitHubProvider)(nil)

type GitHubProvider struct {
	host   string
	client *http.Client
}

func NewGitHubProvider(host string) *GitHubProvider {
	return &GitHubProvider{
		host:   host,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

type githubRepoResponse struct {
	Permissions struct {
		Pull bool `json:"pull"`
		Push bool `json:"push"`
	} `json:"permissions"`
}

func (p *GitHubProvider) Authorize(ctx context.Context, logger *logx.Logger, repo, token string) (Permission, error) {
	url := "https://api.github.com/repos/" + repo
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", errors.Wrap(err, "create request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", errors.Wrap(err, "call GitHub API")
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusNotFound:
		return "", errors.WithStack(ErrTokenInvalid)
	default:
		return "", errors.Newf("unexpected status %d from GitHub API", resp.StatusCode)
	}

	var body githubRepoResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", errors.Wrap(err, "decode GitHub API response")
	}

	if body.Permissions.Push {
		return PermissionWrite, nil
	}
	if body.Permissions.Pull {
		return PermissionRead, nil
	}
	return "", errors.WithStack(ErrTokenInvalid)
}

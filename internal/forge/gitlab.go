package forge

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cockroachdb/errors"

	"unknwon.dev/git-lfs-server/internal/logx"
)

var _ Provider = (*GitLabProvider)(nil)

type GitLabProvider struct {
	baseURL   string
	client    *http.Client
	allowlist *RepoAllowlist
}

func (*GitLabProvider) Type() Type { return TypeGitLab }

func (p *GitLabProvider) Allow(repo string) bool { return p.allowlist.Allow(repo) }

func NewGitLabProvider(host string, allowlist *RepoAllowlist) *GitLabProvider {
	return &GitLabProvider{
		baseURL:   "https://" + host,
		client:    &http.Client{Timeout: 30 * time.Second},
		allowlist: allowlist,
	}
}

func (p *GitLabProvider) Authorize(ctx context.Context, logger *logx.Logger, repo, username, token string) (Permission, time.Duration, error) {
	allowed, err := p.allowGitService(ctx, repo, username, token, "git-receive-pack")
	if err != nil {
		return "", -1, err
	}
	if allowed {
		return PermissionWrite, 0, nil
	}

	allowed, err = p.allowGitService(ctx, repo, username, token, "git-upload-pack")
	if err != nil {
		return "", -1, err
	}
	if allowed {
		return PermissionRead, 0, nil
	}
	return "", -1, errors.WithStack(ErrTokenInvalid)
}

func (p *GitLabProvider) allowGitService(ctx context.Context, repo, username, token, service string) (bool, error) {
	endpoint := gitlabSmartHTTPURL(p.baseURL, repo, service)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, errors.Wrap(err, "create request")
	}
	if username == "" {
		username = "oauth2"
	}
	req.SetBasicAuth(username, token)

	resp, err := p.client.Do(req)
	if err != nil {
		return false, errors.Wrapf(err, "call GitLab %s", service)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return false, nil
	default:
		return false, errors.Newf("unexpected status %d from GitLab %s", resp.StatusCode, service)
	}
}

func gitlabSmartHTTPURL(base, repo, service string) string {
	parts := strings.Split(repo, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	query := url.Values{"service": []string{service}}
	return base + "/" + strings.Join(parts, "/") + ".git/info/refs?" + query.Encode()
}

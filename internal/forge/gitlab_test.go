package forge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"unknwon.dev/git-lfs-server/internal/logx"
)

func TestGitLabGitBase(t *testing.T) {
	t.Run("gitlab.com uses the public host", func(t *testing.T) {
		assert.Equal(t, "https://gitlab.com", gitlabGitBase("gitlab.com"))
	})

	t.Run("self-managed uses the appliance host", func(t *testing.T) {
		assert.Equal(t, "https://gitlab.example.com", gitlabGitBase("gitlab.example.com"))
	})
}

func TestGitLabSmartHTTPURL(t *testing.T) {
	t.Run("nested repo path remains path segments", func(t *testing.T) {
		got := gitlabSmartHTTPURL("https://gitlab.example.com", "group/sub/project", "git-upload-pack")
		assert.Equal(t, "https://gitlab.example.com/group/sub/project.git/info/refs?service=git-upload-pack", got)
	})

	t.Run("repo path segments are escaped", func(t *testing.T) {
		got := gitlabSmartHTTPURL("https://gitlab.example.com", "group/sub project/repo#1", "git-receive-pack")
		assert.Equal(t, "https://gitlab.example.com/group/sub%20project/repo%231.git/info/refs?service=git-receive-pack", got)
	})
}

func TestGitLabProviderAuthorize(t *testing.T) {
	tests := []struct {
		name             string
		receiveStatus    int
		uploadStatus     int
		wantPerm         Permission
		wantErrIsInvalid bool
		wantErrContains  string
		wantServices     []string
	}{
		{
			name:          "receive pack grants write",
			receiveStatus: http.StatusOK,
			wantPerm:      PermissionWrite,
			wantServices:  []string{"git-receive-pack"},
		},
		{
			name:          "upload pack grants read after write denied",
			receiveStatus: http.StatusForbidden,
			uploadStatus:  http.StatusOK,
			wantPerm:      PermissionRead,
			wantServices:  []string{"git-receive-pack", "git-upload-pack"},
		},
		{
			name:             "both services denied rejects token",
			receiveStatus:    http.StatusForbidden,
			uploadStatus:     http.StatusNotFound,
			wantErrIsInvalid: true,
			wantServices:     []string{"git-receive-pack", "git-upload-pack"},
		},
		{
			name:            "unexpected receive status returns error",
			receiveStatus:   http.StatusInternalServerError,
			wantErrContains: "unexpected status 500 from GitLab git-receive-pack",
			wantServices:    []string{"git-receive-pack"},
		},
		{
			name:            "unexpected upload status returns error",
			receiveStatus:   http.StatusUnauthorized,
			uploadStatus:    http.StatusInternalServerError,
			wantErrContains: "unexpected status 500 from GitLab git-upload-pack",
			wantServices:    []string{"git-receive-pack", "git-upload-pack"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotServices []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodHead, r.Method)
				username, password, ok := r.BasicAuth()
				assert.True(t, ok)
				assert.Equal(t, "oauth2", username)
				assert.Equal(t, "token", password)
				assert.Equal(t, "/group/sub/project.git/info/refs", r.URL.Path)

				service := r.URL.Query().Get("service")
				gotServices = append(gotServices, service)
				switch service {
				case "git-receive-pack":
					w.WriteHeader(tt.receiveStatus)
				case "git-upload-pack":
					if tt.uploadStatus == 0 {
						t.Error("unexpected upload-pack request")
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					w.WriteHeader(tt.uploadStatus)
				default:
					t.Errorf("unexpected service %q", service)
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			defer server.Close()

			provider := NewGitLabProvider("gitlab.example.com", nil)
			provider.gitBase = server.URL
			provider.client = server.Client()

			perm, ttl, err := provider.Authorize(context.Background(), logx.NewNoopLogger(), "group/sub/project", "token")
			if tt.wantErrIsInvalid || tt.wantErrContains != "" {
				require.Error(t, err)
				assert.Equal(t, Permission(""), perm)
				assert.Equal(t, time.Duration(-1), ttl)
				if tt.wantErrIsInvalid {
					assert.True(t, errors.Is(err, ErrTokenInvalid))
				}
				if tt.wantErrContains != "" {
					assert.Contains(t, err.Error(), tt.wantErrContains)
				}
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.wantPerm, perm)
				assert.Equal(t, time.Duration(0), ttl)
			}
			assert.Equal(t, tt.wantServices, gotServices)
		})
	}
}

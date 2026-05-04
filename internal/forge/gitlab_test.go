package forge

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGitLabAPIBase(t *testing.T) {
	t.Run("gitlab.com uses the public host", func(t *testing.T) {
		assert.Equal(t, "https://gitlab.com/api/v4", gitlabAPIBase("gitlab.com"))
	})

	t.Run("self-managed uses the appliance host", func(t *testing.T) {
		assert.Equal(t, "https://gitlab.example.com/api/v4", gitlabAPIBase("gitlab.example.com"))
	})
}

func TestGitLabPermissionFromResponse(t *testing.T) {
	withAccess := func(project, group int) *gitlabProjectResponse {
		body := &gitlabProjectResponse{}
		if project >= 0 {
			body.Permissions.ProjectAccess = &struct {
				AccessLevel int `json:"access_level"`
			}{AccessLevel: project}
		}
		if group >= 0 {
			body.Permissions.GroupAccess = &struct {
				AccessLevel int `json:"access_level"`
			}{AccessLevel: group}
		}
		return body
	}

	t.Run("both null is rejected", func(t *testing.T) {
		_, ok := gitlabPermissionFromResponse(withAccess(-1, -1))
		assert.False(t, ok)
	})

	t.Run("guest access is rejected", func(t *testing.T) {
		// Access level 10 (Guest) is below Reporter (20) and grants no LFS
		// read or write. Mapping it to PermissionRead would let a guest
		// download objects the upstream forge would block.
		_, ok := gitlabPermissionFromResponse(withAccess(10, -1))
		assert.False(t, ok)
	})

	t.Run("reporter is read-only", func(t *testing.T) {
		perm, ok := gitlabPermissionFromResponse(withAccess(20, -1))
		assert.True(t, ok)
		assert.Equal(t, PermissionRead, perm)
	})

	t.Run("developer is read-write", func(t *testing.T) {
		perm, ok := gitlabPermissionFromResponse(withAccess(30, -1))
		assert.True(t, ok)
		assert.Equal(t, PermissionWrite, perm)
	})

	t.Run("maintainer is read-write", func(t *testing.T) {
		perm, ok := gitlabPermissionFromResponse(withAccess(40, -1))
		assert.True(t, ok)
		assert.Equal(t, PermissionWrite, perm)
	})

	t.Run("owner is read-write", func(t *testing.T) {
		perm, ok := gitlabPermissionFromResponse(withAccess(50, -1))
		assert.True(t, ok)
		assert.Equal(t, PermissionWrite, perm)
	})

	t.Run("group access raises effective level above project access", func(t *testing.T) {
		// User is a Reporter on the project but Developer on the parent
		// group. The inherited group access should win.
		perm, ok := gitlabPermissionFromResponse(withAccess(20, 30))
		assert.True(t, ok)
		assert.Equal(t, PermissionWrite, perm)
	})

	t.Run("project access raises effective level above group access", func(t *testing.T) {
		perm, ok := gitlabPermissionFromResponse(withAccess(40, 20))
		assert.True(t, ok)
		assert.Equal(t, PermissionWrite, perm)
	})
}

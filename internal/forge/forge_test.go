package forge

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRepoAllowlist(t *testing.T) {
	t.Run("nil for empty input", func(t *testing.T) {
		got, err := NewRepoAllowlist(nil)
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("nil when only empty entries", func(t *testing.T) {
		got, err := NewRepoAllowlist([]string{"", "  ", "\t"})
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("trims and lowercases entries", func(t *testing.T) {
		a, err := NewRepoAllowlist([]string{"  MyOrg/Repo  ", "Other/**"})
		require.NoError(t, err)
		require.NotNil(t, a)
		assert.True(t, a.Allow("myorg/repo"))
		assert.True(t, a.Allow("other/anything"))
		assert.False(t, a.Allow("myorg/Repo"), "callers always pass lowercased input; matcher stores lowercase")
	})

	t.Run("drops empty entries mixed with valid ones", func(t *testing.T) {
		a, err := NewRepoAllowlist([]string{"", "myorg/**", " "})
		require.NoError(t, err)
		require.NotNil(t, a)
		assert.True(t, a.Allow("myorg/repo"))
	})

	t.Run("rejects invalid entries", func(t *testing.T) {
		invalid := []string{
			"/",
			"/foo",
			"foo//bar",
			"**",
			"*",
			"*/repo",
			"foo/*",
			"foo/**/bar",
			"foo*",
			"**/repo",
		}
		for _, entry := range invalid {
			t.Run(entry, func(t *testing.T) {
				_, err := NewRepoAllowlist([]string{entry})
				require.Error(t, err)
			})
		}
	})
}

func TestRepoAllowlist_Allow(t *testing.T) {
	t.Run("nil receiver allows everything", func(t *testing.T) {
		var a *RepoAllowlist
		assert.True(t, a.Allow("anything/at/all"))
		assert.True(t, a.Allow(""))
	})

	t.Run("exact match", func(t *testing.T) {
		a, err := NewRepoAllowlist([]string{"myorg/repo"})
		require.NoError(t, err)
		assert.True(t, a.Allow("myorg/repo"))
		assert.False(t, a.Allow("myorg/other"))
		assert.False(t, a.Allow("myorg/repo/extra"))
		assert.False(t, a.Allow("myorg"))
	})

	t.Run("prefix wildcard", func(t *testing.T) {
		a, err := NewRepoAllowlist([]string{"myorg/**"})
		require.NoError(t, err)
		assert.True(t, a.Allow("myorg/anything"))
		assert.True(t, a.Allow("myorg/sub/repo"))
		assert.False(t, a.Allow("myorg"))
		assert.False(t, a.Allow("other/anything"))
		assert.False(t, a.Allow("myorganization/repo"), "prefix requires the trailing slash boundary")
	})

	t.Run("multi-segment prefix wildcard", func(t *testing.T) {
		a, err := NewRepoAllowlist([]string{"mygroup/sub/**"})
		require.NoError(t, err)
		assert.True(t, a.Allow("mygroup/sub/repo"))
		assert.True(t, a.Allow("mygroup/sub/deeper/repo"))
		assert.False(t, a.Allow("mygroup/sub"))
		assert.False(t, a.Allow("mygroup/other/repo"))
	})

	t.Run("exact and prefix coexist", func(t *testing.T) {
		a, err := NewRepoAllowlist([]string{"myorg/special", "myorg/**", "otherorg/specific"})
		require.NoError(t, err)
		assert.True(t, a.Allow("myorg/special"))
		assert.True(t, a.Allow("myorg/anything"))
		assert.True(t, a.Allow("otherorg/specific"))
		assert.False(t, a.Allow("otherorg/anything"))
	})
}

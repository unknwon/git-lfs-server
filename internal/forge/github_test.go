package forge

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"unknwon.dev/git-lfs-server/internal/logx"
)

func TestGitHubCacheTTL(t *testing.T) {
	ctx := context.Background()
	logger := logx.NewNoopLogger()

	t.Run("header absent returns default TTL", func(t *testing.T) {
		assert.Equal(t, githubDefaultTTL, githubCacheTTL(ctx, logger, ""))
	})

	t.Run("header malformed returns default TTL", func(t *testing.T) {
		assert.Equal(t, githubDefaultTTL, githubCacheTTL(ctx, logger, "not a date"))
	})

	t.Run("header far in the future is clamped to max TTL", func(t *testing.T) {
		expiry := time.Now().Add(8 * time.Hour).UTC().Format(time.RFC3339)
		assert.Equal(t, githubMaxTTL, githubCacheTTL(ctx, logger, expiry))
	})

	t.Run("header 30 minutes out returns expiry minus margin", func(t *testing.T) {
		expiry := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
		ttl := githubCacheTTL(ctx, logger, expiry)
		// Allow a 2-second window for test scheduling jitter.
		want := 30*time.Minute - githubMargin
		assert.InDelta(t, want, ttl, float64(2*time.Second))
	})

	t.Run("header at or before now disables caching", func(t *testing.T) {
		expiry := time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339)
		assert.Equal(t, time.Duration(-1), githubCacheTTL(ctx, logger, expiry))
	})

	t.Run("header within margin disables caching", func(t *testing.T) {
		expiry := time.Now().Add(githubMargin / 2).UTC().Format(time.RFC3339)
		assert.Equal(t, time.Duration(-1), githubCacheTTL(ctx, logger, expiry))
	})
}

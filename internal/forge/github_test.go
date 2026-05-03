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

	// Wire formats GitHub uses, matching empirically observed behavior:
	//   - classic PATs: "2006-01-02 15:04:05 UTC"
	//   - fine-grained PATs / GitHub App user tokens: "2006-01-02 15:04:05 -0700"
	const (
		layoutClassic     = "2006-01-02 15:04:05 UTC"
		layoutFineGrained = "2006-01-02 15:04:05 -0700"
	)

	t.Run("header absent returns default TTL", func(t *testing.T) {
		assert.Equal(t, githubDefaultTTL, githubCacheTTL(ctx, logger, ""))
	})

	t.Run("header malformed returns default TTL", func(t *testing.T) {
		assert.Equal(t, githubDefaultTTL, githubCacheTTL(ctx, logger, "not a date"))
	})

	t.Run("classic PAT UTC header far in the future is clamped to max TTL", func(t *testing.T) {
		expiry := time.Now().UTC().Add(8 * time.Hour).Format(layoutClassic)
		assert.Equal(t, githubMaxTTL, githubCacheTTL(ctx, logger, expiry))
	})

	t.Run("classic PAT UTC header 30 minutes out returns expiry minus margin", func(t *testing.T) {
		expiry := time.Now().UTC().Add(30 * time.Minute).Format(layoutClassic)
		ttl := githubCacheTTL(ctx, logger, expiry)
		want := 30*time.Minute - githubMargin
		assert.InDelta(t, want, ttl, float64(2*time.Second))
	})

	t.Run("fine-grained PAT offset header in non-UTC zone parses correctly", func(t *testing.T) {
		// Real-world capture from GitHub Community Discussion #172213 has the
		// form "2025-09-10 02:30:13 +0200". Format using a +0200 zone so the
		// wall-clock differs from UTC, proving we don't naively assume UTC.
		berlin := time.FixedZone("test", 2*60*60)
		expiry := time.Now().Add(30 * time.Minute).In(berlin).Format(layoutFineGrained)
		ttl := githubCacheTTL(ctx, logger, expiry)
		want := 30*time.Minute - githubMargin
		assert.InDelta(t, want, ttl, float64(2*time.Second))
	})

	t.Run("classic PAT UTC header at or before now disables caching", func(t *testing.T) {
		expiry := time.Now().UTC().Add(-1 * time.Minute).Format(layoutClassic)
		assert.Equal(t, time.Duration(-1), githubCacheTTL(ctx, logger, expiry))
	})

	t.Run("classic PAT UTC header within margin disables caching", func(t *testing.T) {
		expiry := time.Now().UTC().Add(githubMargin / 2).Format(layoutClassic)
		assert.Equal(t, time.Duration(-1), githubCacheTTL(ctx, logger, expiry))
	})
}

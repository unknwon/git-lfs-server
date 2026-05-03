package forge

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"unknwon.dev/git-lfs-server/internal/logx"
)

func TestGitHubTokenTTL(t *testing.T) {
	ctx := context.Background()
	logger := logx.NewNoopLogger()

	// Wire formats GitHub uses, matching empirically observed behavior:
	//   - classic PATs: "2006-01-02 15:04:05 UTC"
	//   - fine-grained PATs / GitHub App user tokens: "2006-01-02 15:04:05 -0700"
	const (
		layoutClassic     = "2006-01-02 15:04:05 UTC"
		layoutFineGrained = "2006-01-02 15:04:05 -0700"
	)

	t.Run("header absent returns zero", func(t *testing.T) {
		assert.Equal(t, time.Duration(0), githubTokenTTL(ctx, logger, ""))
	})

	t.Run("header malformed returns negative to fail closed", func(t *testing.T) {
		assert.Negative(t, githubTokenTTL(ctx, logger, "not a date"))
	})

	t.Run("classic PAT UTC header far in the future returns large positive duration", func(t *testing.T) {
		expiry := time.Now().UTC().Add(8 * time.Hour).Format(layoutClassic)
		ttl := githubTokenTTL(ctx, logger, expiry)
		assert.InDelta(t, (8 * time.Hour).Seconds(), ttl.Seconds(), 2)
	})

	t.Run("classic PAT UTC header 30 minutes out returns ~30 minutes", func(t *testing.T) {
		expiry := time.Now().UTC().Add(30 * time.Minute).Format(layoutClassic)
		ttl := githubTokenTTL(ctx, logger, expiry)
		assert.InDelta(t, (30 * time.Minute).Seconds(), ttl.Seconds(), 2)
	})

	t.Run("fine-grained PAT offset header in non-UTC zone parses correctly", func(t *testing.T) {
		// Real-world capture from GitHub Community Discussion #172213 has the
		// form "2025-09-10 02:30:13 +0200". Format using a +0200 zone so the
		// wall-clock differs from UTC, proving we don't naively assume UTC.
		berlin := time.FixedZone("test", 2*60*60)
		expiry := time.Now().Add(30 * time.Minute).In(berlin).Format(layoutFineGrained)
		ttl := githubTokenTTL(ctx, logger, expiry)
		assert.InDelta(t, (30 * time.Minute).Seconds(), ttl.Seconds(), 2)
	})

	t.Run("classic PAT UTC header in the past returns negative duration", func(t *testing.T) {
		expiry := time.Now().UTC().Add(-1 * time.Minute).Format(layoutClassic)
		assert.Negative(t, githubTokenTTL(ctx, logger, expiry))
	})
}

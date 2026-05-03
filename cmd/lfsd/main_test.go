package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestAuthCacheTTL(t *testing.T) {
	t.Run("negative rawTTL opts out of caching", func(t *testing.T) {
		assert.Equal(t, time.Duration(-1), authCacheTTL(-1))
		assert.Equal(t, time.Duration(-1), authCacheTTL(-1*time.Minute))
	})

	t.Run("zero rawTTL uses default TTL", func(t *testing.T) {
		assert.Equal(t, authCacheTTLDefault, authCacheTTL(0))
	})

	t.Run("positive rawTTL far in the future is clamped to max TTL", func(t *testing.T) {
		assert.Equal(t, authCacheTTLMax, authCacheTTL(8*time.Hour))
	})

	t.Run("positive rawTTL 30 minutes out returns expiry minus margin", func(t *testing.T) {
		assert.Equal(t, 30*time.Minute-authCacheTTLMargin, authCacheTTL(30*time.Minute))
	})

	t.Run("positive rawTTL within margin disables caching", func(t *testing.T) {
		assert.Equal(t, time.Duration(-1), authCacheTTL(authCacheTTLMargin/2))
	})

	t.Run("positive rawTTL exactly equal to margin disables caching", func(t *testing.T) {
		assert.Equal(t, time.Duration(-1), authCacheTTL(authCacheTTLMargin))
	})
}

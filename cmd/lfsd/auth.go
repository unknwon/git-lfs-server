package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/flamego/cache"
	"github.com/flamego/flamego"

	"unknwon.dev/git-lfs-server/internal/forge"
	"unknwon.dev/git-lfs-server/internal/logx"
)

func authorize(forges map[string]forge.Provider) flamego.Handler {
	return func(c flamego.Context, logger *logx.Logger, cache cache.Cache) {
		logger = logger.Scoped("authorize")
		ctx := c.Request().Context()

		host := hostFromContext(c)
		provider, ok := forges[host]
		if !ok {
			http.Error(c.ResponseWriter(), "unsupported forge host", http.StatusBadRequest)
			return
		}

		_, token, ok := c.Request().BasicAuth()
		if !ok || token == "" {
			c.ResponseWriter().Header().Set("WWW-Authenticate", `Basic realm="git-lfs"`)
			http.Error(c.ResponseWriter(), "basic auth credentials are required", http.StatusUnauthorized)
			return
		}

		repo := strings.ToLower(c.Param("**"))
		if repo == "" {
			http.Error(c.ResponseWriter(), "repository path is missing", http.StatusBadRequest)
			return
		}

		if !provider.Allow(repo) {
			logger.InfoContext(ctx, "Repository rejected by allowlist", "host", host, "repo", repo)
			http.Error(c.ResponseWriter(), "repository is not in the allowlist", http.StatusForbidden)
			return
		}

		key := authCacheKey(host, repo, token)
		if v, err := cache.Get(ctx, key); err == nil {
			if perm, ok := v.(forge.Permission); ok {
				c.Map(perm)
				return
			}
			logger.DebugContext(ctx, "Discarding auth cache entry of unexpected type")
		} else if !errors.Is(err, os.ErrNotExist) {
			logger.WarnContext(ctx, "Auth cache lookup failed", "error", err)
		}

		perm, rawTTL, err := provider.Authorize(ctx, logger.Scoped("forge"), repo, token)
		if err != nil {
			if errors.Is(err, forge.ErrTokenInvalid) {
				http.Error(c.ResponseWriter(), "token is invalid or lacks repository access", http.StatusForbidden)
			} else {
				logger.ErrorContext(ctx, "Failed to verify repository permissions", "repo", repo, "error", err)
				http.Error(c.ResponseWriter(), http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			}
			return
		}

		if ttl := authCacheTTL(rawTTL); ttl >= 0 {
			// Decouple the cache write from the request context so a client
			// disconnect between the upstream call and the Set doesn't drop
			// the entry on backends that honor cancellation.
			setCtx := context.WithoutCancel(ctx)
			if err := cache.Set(setCtx, key, perm, ttl); err != nil {
				logger.WarnContext(ctx, "Failed to cache authorization result", "error", err)
			}
		}

		c.Map(perm)
	}
}

// authCacheKey hashes the host, repo, and token together so the cache never
// retains plaintext tokens in its keyspace.
func authCacheKey(host, repo, token string) string {
	sum := sha256.Sum256([]byte(host + ":" + repo + ":" + token))
	return hex.EncodeToString(sum[:])
}

// Cache TTL policy for permission decisions. The forge provider returns the raw
// time until the token expires (zero if unknown). This function applies the
// default TTL, safety margin, and maximum cap so the cached entry is always
// invalidated before the underlying token becomes stale.
const (
	authCacheTTLDefault = 5 * time.Minute
	authCacheTTLMax     = 1 * time.Hour
	authCacheTTLMargin  = 5 * time.Minute
)

// authCacheTTL converts a provider-reported raw token TTL into an effective
// cache TTL. A negative rawTTL means the provider explicitly opts out of
// caching, and the returned value is also negative in that case. A zero
// rawTTL means the provider has no expiry signal and authCacheTTLDefault
// is used. A positive rawTTL has authCacheTTLMargin subtracted and is
// capped at authCacheTTLMax. If the result is non-positive, caching is
// also skipped.
func authCacheTTL(rawTTL time.Duration) time.Duration {
	if rawTTL < 0 {
		return -1
	}
	var ttl time.Duration
	if rawTTL == 0 {
		ttl = authCacheTTLDefault
	} else {
		ttl = rawTTL - authCacheTTLMargin
	}
	if ttl <= 0 {
		return -1
	}
	if ttl > authCacheTTLMax {
		return authCacheTTLMax
	}
	return ttl
}

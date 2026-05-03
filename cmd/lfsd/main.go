package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/cockroachdb/errors"
	"github.com/flamego/cache"
	"github.com/flamego/flamego"
	"github.com/sourcegraph/conc"

	"unknwon.dev/git-lfs-server/internal/database"
	"unknwon.dev/git-lfs-server/internal/forge"
	"unknwon.dev/git-lfs-server/internal/logx"
	"unknwon.dev/git-lfs-server/internal/strx"
)

// Set via ldflags.
var (
	buildDate   string
	buildCommit string
)

func main() {
	configPath := strx.Coalesce(os.Getenv("LFSD_CONFIG_PATH"), "config.ini")
	config, err := loadConfig(configPath)
	if err != nil {
		// Logger is not yet available; write to stderr and exit.
		_, _ = os.Stderr.WriteString("Failed to load config: " + err.Error() + "\n")
		os.Exit(1)
	}

	ctx := context.Background()
	logger := setupLogging(config.Log)

	for host, provider := range config.Forges {
		logger.InfoContext(ctx, "Forge initialized", "host", host, "type", provider.Type())
		if provider.Type() == forge.TypeSkipAuth {
			logger.WarnContext(ctx, "Forge auth is disabled — every request grants write access", "host", host)
		}
	}
	for host, backend := range config.Storages {
		logger.InfoContext(ctx, "Storage initialized", "host", host, "name", backend.Name(), "type", backend.Type())
	}

	db, err := database.New(ctx, logger.Scoped("database"), config.Database)
	if err != nil {
		logger.FatalContext(ctx, "Failed to connect to database", "error", err)
	}

	f := flamego.New()
	f.Map(logger)
	f.Use(flamego.Recovery())
	f.Use(cache.Cacher())

	f.Get("/healthz", serveHealthz(db))

	f.Group("/{host}/{**}/info/lfs/objects", func() {
		f.Post("/batch", serveBatch(db, config.Server.ExternalURL, config.Server.MaxObjectSize))
		f.Get("/{oid}", serveDownload(db, config.Storages))
		f.Put("/{oid}", serveUpload(db, config.Storages, config.Server.MaxObjectSize))
		f.Post("/verify", serveVerify(db, config.Storages))
	}, authorize(config.Forges))

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:    config.Server.ListenAddress,
		Handler: f,
	}

	var routines conc.WaitGroup
	routines.Go(func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.ErrorContext(ctx, "HTTP server exited unexpectedly", "error", err)
		}
		stop()
	})
	routines.Go(func() {
		<-ctx.Done()
		logger.WarnContext(ctx, "Server shutdown requested")
		if err := srv.Shutdown(context.Background()); err != nil {
			logger.ErrorContext(ctx, "Failed to shut down HTTP server gracefully", "error", err)
		}
	})

	logger.InfoContext(ctx, "Server ready",
		"buildDate", buildDate, "buildCommit", buildCommit,
		"listenAddr", config.Server.ListenAddress,
	)
	if r := routines.WaitAndRecover(); r != nil {
		logger.FatalContext(ctx, "Server panicked", "panic", r.Value, "stack", string(r.Stack))
	}
}

func hostFromContext(c flamego.Context) string {
	return strings.ToLower(c.Param("host"))
}

func repoNameFromContext(c flamego.Context) string {
	return strings.ToLower(c.Param("host") + "/" + c.Param("**"))
}

func serveHealthz(db *database.DB) flamego.Handler {
	return func(c flamego.Context, logger *logx.Logger) {
		if err := db.Ping(c.Request().Context()); err != nil {
			logger.ErrorContext(c.Request().Context(), "Health check failed", "error", err)
			http.Error(c.ResponseWriter(), http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
		c.ResponseWriter().WriteHeader(http.StatusOK)
		_, _ = c.ResponseWriter().Write([]byte("ok"))
	}
}

func authorize(forges map[string]forge.Provider) flamego.Handler {
	return func(c flamego.Context, logger *logx.Logger, store cache.Cache) {
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
		if v, err := store.Get(ctx, key); err == nil {
			if perm, ok := v.(forge.Permission); ok {
				c.Map(perm)
				return
			}
			logger.DebugContext(ctx, "Discarding auth cache entry of unexpected type")
		} else if !errors.Is(err, os.ErrNotExist) {
			logger.DebugContext(ctx, "Auth cache lookup failed", "error", err)
		}

		perm, ttl, err := provider.Authorize(ctx, logger.Scoped("forge"), repo, token)
		if err != nil {
			if errors.Is(err, forge.ErrTokenInvalid) {
				http.Error(c.ResponseWriter(), "token is invalid or lacks repository access", http.StatusForbidden)
			} else {
				logger.ErrorContext(ctx, "Failed to verify repository permissions", "repo", repo, "error", err)
				http.Error(c.ResponseWriter(), http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			}
			return
		}

		if ttl >= 0 {
			// Decouple the cache write from the request context so a client
			// disconnect between the upstream call and the Set doesn't drop
			// the entry on backends that honor cancellation.
			setCtx := context.WithoutCancel(ctx)
			if err := store.Set(setCtx, key, perm, ttl); err != nil {
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

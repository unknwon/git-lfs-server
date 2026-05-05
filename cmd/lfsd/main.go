package main

import (
	"context"
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
	"unknwon.dev/git-lfs-server/internal/janitor"
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
		// Logger is not yet available, write to stderr and exit.
		_, _ = os.Stderr.WriteString("Failed to load config: " + err.Error() + "\n")
		os.Exit(1)
	}

	ctx := context.Background()
	logger := setupLogging(config.Log)

	for host, provider := range config.Forges {
		logger.InfoContext(ctx, "Forge initialized", "host", host, "type", provider.Type())
		if provider.Type() == forge.TypeSkipAuth {
			logger.WarnContext(ctx, "Forge auth is disabled, every request grants write access", "host", host)
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
	routines.Go(func() {
		janitor.New(db, config.Storages, config.Janitor).Run(ctx, logger.Scoped("janitor"))
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

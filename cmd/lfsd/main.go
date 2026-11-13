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
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"unknwon.dev/git-lfs-server/internal/database"
	"unknwon.dev/git-lfs-server/internal/forge"
	"unknwon.dev/git-lfs-server/internal/janitor"
	"unknwon.dev/git-lfs-server/internal/logx"
	"unknwon.dev/git-lfs-server/internal/strx"
	"unknwon.dev/git-lfs-server/internal/tracing"
	"unknwon.dev/git-lfs-server/lfs"
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

	// Set up before anything that creates spans, e.g., the database plugin.
	shutdownTracing, err := tracing.Setup(ctx, config.Tracing, buildCommit)
	if err != nil {
		logger.FatalContext(ctx, "Failed to set up tracing", "error", err)
	}
	if config.Tracing.Enabled {
		logger.InfoContext(ctx, "Tracing enabled", "sampleRatio", config.Tracing.SampleRatio)
	}

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

	services := make(map[string]*lfs.Service, len(config.Storages))
	for host, backend := range config.Storages {
		services[host] = &lfs.Service{
			Store:          db,
			Storage:        backend,
			MaxObjectSize:  config.Server.MaxObjectSize,
			CrossRepoDedup: true,
		}
	}
	serve := serveLFS(config.Server.ExternalURL, services)
	auth := authorize(config.Forges)
	const objectsRoute = "/{host}/{**}/info/lfs/objects"
	f.Group(objectsRoute, func() {
		f.Post("/batch", traceRoute(objectsRoute+"/batch"), auth, serve)
		f.Get("/{oid}", traceRoute(objectsRoute+"/{oid}"), auth, serve)
		f.Put("/{oid}", traceRoute(objectsRoute+"/{oid}"), auth, serve)
		f.Post("/verify", traceRoute(objectsRoute+"/verify"), auth, serve)
	})

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr: config.Server.ListenAddress,
		Handler: otelhttp.NewHandler(f, "lfsd",
			otelhttp.WithFilter(func(r *http.Request) bool { return r.URL.Path != "/healthz" }),
		),
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
		janitor.New(db, config.Storages).Run(ctx, logger.Scoped("janitor"))
	})

	logger.InfoContext(ctx, "Server ready",
		"buildDate", buildDate, "buildCommit", buildCommit,
		"listenAddr", config.Server.ListenAddress,
	)
	r := routines.WaitAndRecover()
	// Flush the spans of the requests that finished during shutdown. The
	// signal context is already canceled, so the flush gets its own.
	if err := shutdownTracing(context.Background()); err != nil {
		logger.ErrorContext(ctx, "Failed to shut down tracing", "error", err)
	}
	if r != nil {
		logger.FatalContext(ctx, "Server panicked", "panic", r.Value, "stack", string(r.Stack))
	}
}

// traceRoute names the server span of the request after route, because the
// otelhttp handler wraps the router and does not know which route matched.
func traceRoute(route string) flamego.Handler {
	return func(c flamego.Context) {
		span := trace.SpanFromContext(c.Request().Context())
		span.SetName(c.Request().Method + " " + route)
		span.SetAttributes(
			attribute.String("http.route", route),
			attribute.String("lfs.forge.host", hostFromContext(c)),
			attribute.String("lfs.repo", repoNameFromContext(c)),
		)
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

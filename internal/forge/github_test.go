package forge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

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

func TestGitHubProvider_Tracing(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	// The forge client uses the global provider and must ignore the global
	// propagator, so install both to prove that nothing is propagated.
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	const token = "ghp_secret"
	var gotHeader http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		_, _ = w.Write([]byte(`{"permissions":{"pull":true,"push":true}}`))
	}))
	t.Cleanup(server.Close)

	provider := NewGitHubProvider("github.example.com", nil)
	provider.apiBase = server.URL

	ctx, parent := tp.Tracer("test").Start(t.Context(), "parent")
	perm, _, err := provider.Authorize(ctx, logx.NewNoopLogger(), "org/repo", "x-access-token", token)
	parent.End()
	require.NoError(t, err)
	assert.Equal(t, PermissionWrite, perm)

	assert.Empty(t, gotHeader.Get("traceparent"), "trace context must not be sent to the forge")

	var client sdktrace.ReadOnlySpan
	for _, span := range recorder.Ended() {
		if span.SpanKind() == trace.SpanKindClient {
			client = span
		}
	}
	require.NotNil(t, client, "forge call must be traced")
	assert.Equal(t, parent.SpanContext().SpanID(), client.Parent().SpanID())
	for _, a := range client.Attributes() {
		assert.NotContains(t, a.Value.String(), token, "attribute %s", a.Key)
	}
}

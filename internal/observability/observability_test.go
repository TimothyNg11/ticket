package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
)

func TestLogLinesCarryRequestAndTraceIDs(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(contextHandler{slog.NewJSONHandler(&buf, nil)})

	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(WithRequestID(context.Background(), "req-123"),
		trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid}))

	log.With("job", "x").InfoContext(ctx, "hello", "k", 1)
	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	assert.Equal(t, "req-123", line["request_id"])
	assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", line["trace_id"])
	assert.Equal(t, "x", line["job"], "attributes added with With survive")

	buf.Reset()
	log.Info("no context")
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	assert.NotContains(t, buf.String(), "request_id")
}

func TestQuerySpanName(t *testing.T) {
	assert.Equal(t, "db GetHoldForUpdate", QuerySpanName("-- name: GetHoldForUpdate :one\nSELECT * FROM holds WHERE id = $1 FOR UPDATE"))
	assert.Equal(t, "db SELECT", QuerySpanName("  select count(*) from holds"))
	assert.Equal(t, "db", QuerySpanName(""))
}

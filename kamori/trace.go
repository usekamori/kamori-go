package kamori

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// traceCtxKey is the private context key under which ContextWithTrace stores an
// explicit correlation id.
type traceCtxKey struct{}

// ContextWithTrace returns a copy of ctx carrying the given trace id. LogCtx
// attaches it to events that do not already set "trace_id".
func ContextWithTrace(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceCtxKey{}, traceID)
}

// TraceFromContext returns the trace id stored by ContextWithTrace, or "".
func TraceFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(traceCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// TraceExtractor derives a trace id (and optional span id) from a context.
// The optional kamori/otel subpackage registers one to bridge OpenTelemetry,
// which keeps OpenTelemetry out of the core package's dependency graph.
type TraceExtractor func(ctx context.Context) (traceID, spanID string)

var traceExtractor TraceExtractor

// RegisterTraceExtractor installs a hook consulted by LogCtx when the context
// carries no explicit ContextWithTrace id. Called by kamori/otel on import.
func RegisterTraceExtractor(fn TraceExtractor) { traceExtractor = fn }

// resolveTrace returns the trace/span ids for ctx. An explicit ContextWithTrace
// id wins; otherwise the registered extractor (e.g. OpenTelemetry) is consulted.
func resolveTrace(ctx context.Context) (string, string) {
	if id := TraceFromContext(ctx); id != "" {
		return id, ""
	}
	if traceExtractor != nil {
		return traceExtractor(ctx)
	}
	return "", ""
}

// LogCtx logs an event, attaching trace_id (and span_id when available) derived
// from ctx unless the event already sets them. The caller's fields always win.
func (c *Client) LogCtx(ctx context.Context, event Event) {
	c.Log(withTraceFields(ctx, event))
}

// LogCtx logs through the scoped client, attaching trace context from ctx.
func (s *ScopedClient) LogCtx(ctx context.Context, event Event) {
	s.Log(withTraceFields(ctx, event))
}

// withTraceFields returns event augmented with trace_id/span_id from ctx, or
// event unchanged when there is no context id or the fields are already set.
func withTraceFields(ctx context.Context, event Event) Event {
	if ctx == nil {
		return event
	}
	if _, ok := event["trace_id"]; ok {
		return event
	}
	traceID, spanID := resolveTrace(ctx)
	if traceID == "" {
		return event
	}
	merged := make(Event, len(event)+2)
	for k, v := range event {
		merged[k] = v
	}
	merged["trace_id"] = traceID
	if spanID != "" {
		if _, ok := event["span_id"]; !ok {
			merged["span_id"] = spanID
		}
	}
	return merged
}

// GenerateTraceID returns a random 32-hex-char trace id, suitable for seeding a
// correlation id when no inbound trace is present.
func GenerateTraceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ParseTraceparent extracts the trace id from a W3C `traceparent` header value,
// or "" if it is malformed or all-zero. Use it in HTTP middleware to reuse an
// inbound trace across services.
func ParseTraceparent(header string) string {
	parts := strings.Split(strings.TrimSpace(header), "-")
	if len(parts) < 4 {
		return ""
	}
	traceID := parts[1]
	if len(traceID) != 32 || traceID == "00000000000000000000000000000000" {
		return ""
	}
	for _, r := range traceID {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return ""
		}
	}
	return traceID
}

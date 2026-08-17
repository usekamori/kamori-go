// Package otel bridges OpenTelemetry trace context into the Kamori Go SDK.
//
// Import it for its side effect (or call Enable) so that kamori.LogCtx attaches
// the active span's trace_id/span_id automatically:
//
//	import (
//	    "github.com/usekamori/kamori-go/kamori"
//	    _ "github.com/usekamori/kamori-go/kamori/otel" // enable OTel bridge
//	)
//
//	client.LogCtx(ctx, kamori.Event{"level": "info", "message": "handled"})
//	// → trace_id/span_id copied from the active OpenTelemetry span in ctx
//
// Keeping this in a separate package means the core kamori package has no
// OpenTelemetry dependency: projects that don't import it never pull OTel.
package otel

import (
	"context"

	"github.com/usekamori/kamori-go/kamori"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func init() { Enable() }

// Enable registers the OpenTelemetry trace extractor with the core SDK. It is
// called automatically on import; exposed for explicit or testable use.
func Enable() {
	kamori.RegisterTraceExtractor(func(ctx context.Context) (string, string) {
		sc := oteltrace.SpanContextFromContext(ctx)
		if !sc.IsValid() {
			return "", ""
		}
		return sc.TraceID().String(), sc.SpanID().String()
	})
}

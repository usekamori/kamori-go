# kamori-go

Go SDK for [Kamori](https://github.com/usekamori/kamori) — self-hosted log ingestion with MCP support.

Events are buffered in memory and sent to your Kamori server in the background. `Flush()` is safe to call concurrently from any goroutine.

---

## Installation

```bash
go get github.com/usekamori/kamori-go
```

Requires Go 1.22+.

---

## Quick start

```go
package main

import (
    "github.com/usekamori/kamori-go/kamori"
)

func main() {
    client := kamori.New(kamori.Options{
        URL:   "https://your-kamori-server.com",
        Token: "your-log-token",   // matches INGEST_TOKEN on the server
    })
    defer client.Shutdown(context.Background())   // flush and wait for in-flight sends

    client.Log(kamori.Event{
        "level":   "info",
        "service": "api",
        "message": "Server started",
    })

    client.Log(kamori.Event{
        "level":       "error",
        "service":     "payments",
        "message":     "Stripe timeout",
        "duration_ms": 5001,
    })
}
```

---

## Options

```go
client := kamori.New(kamori.Options{
    URL:           "https://your-kamori-server.com",
    Token:         "your-log-token",

    // Default service name merged into every event (optional).
    // Overridden per-event if the event already has a "service" key.
    Service:       "api",

    // Flush when the buffer reaches this many events (default: 50).
    BatchSize:     50,

    // Max time to wait before flushing (default: 2s).
    FlushInterval: 2 * time.Second,

    // Max concurrent in-flight HTTP sends (default: 4).
    // If this limit is reached when Flush is called, the batch is dropped
    // (OnDrop is called) rather than blocking the caller.
    MaxConcurrent: 4,

    // Max events in the in-memory buffer (default: 5 * BatchSize = 250).
    // Log() drops new events (calling OnDrop) once this limit is reached.
    // Set to -1 to disable the limit.
    MaxBuffer: 250,

    // Called with the batch when all retries are exhausted (optional).
    OnDrop: func(events []kamori.Event) {
        log.Printf("kamori: dropped %d events", len(events))
    },
})
```

---

## Log / Flush

```go
// Queue an event (non-blocking). Dropped silently if MaxBuffer is reached.
client.Log(kamori.Event{"level": "warn", "message": "Cache miss"})

// Flush all buffered events now (non-blocking send via goroutine).
// Blocks only to drain the local buffer — the HTTP request is async.
client.Flush()

// Shutdown flushes the buffer and waits for all in-flight sends to complete.
// Pass a context with a deadline to bound the wait time.
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
if err := client.Shutdown(ctx); err != nil {
    log.Printf("kamori: shutdown: %v", err)
}
```

---

## Scoped clients

`Scoped` creates a lightweight view over the same underlying buffer that merges default fields into every event. It shares the parent's timer and HTTP client — no extra goroutines.

```go
// Create a scoped client with per-request defaults
requestLog := client.Scoped(kamori.Event{
    "service":    "api",
    "request_id": "abc-123",
    "user_id":    42,
})

requestLog.Log(kamori.Event{"level": "info", "message": "Request started"})
requestLog.Log(kamori.Event{"level": "error", "message": "Validation failed", "field": "email"})
// Both events include service, request_id, and user_id automatically.

// Flush the parent (scoped clients share the parent buffer)
requestLog.Flush()
```

---

## slog integration

```go
import (
    "log/slog"
    "github.com/usekamori/kamori-go/kamori"
)

client := kamori.New(kamori.Options{URL: "https://your-kamori-server.com", Token: "tok"})

// Custom slog handler that forwards to Kamori
type kamoriHandler struct {
    client *kamori.Client
    attrs  []slog.Attr
}

func (h *kamoriHandler) Handle(_ context.Context, r slog.Record) error {
    event := kamori.Event{
        "level":   r.Level.String(),
        "message": r.Message,
        "time":    r.Time.Format(time.RFC3339),
    }
    r.Attrs(func(a slog.Attr) bool {
        event[a.Key] = a.Value.Any()
        return true
    })
    for _, a := range h.attrs {
        event[a.Key] = a.Value.Any()
    }
    h.client.Log(event)
    return nil
}

func (h *kamoriHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *kamoriHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
    return &kamoriHandler{client: h.client, attrs: append(h.attrs, attrs...)}
}
func (h *kamoriHandler) WithGroup(_ string) slog.Handler { return h }

logger := slog.New(&kamoriHandler{client: client})
logger.Info("Server started", "service", "api", "port", 8080)
```

---

## Context-aware logging

Pass request-scoped metadata through `context.Context`:

```go
type ctxKeyKamori struct{}

// Attach a scoped client to a context
func WithKamori(ctx context.Context, defaults kamori.Event) context.Context {
    scoped := globalClient.Scoped(defaults)
    return context.WithValue(ctx, ctxKeyKamori{}, scoped)
}

// Retrieve from context (nil-safe)
func FromContext(ctx context.Context) *kamori.ScopedClient {
    if sc, ok := ctx.Value(ctxKeyKamori{}).(*kamori.ScopedClient); ok {
        return sc
    }
    return nil
}

// In HTTP middleware
func loggingMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        ctx := WithKamori(r.Context(), kamori.Event{
            "service":    "api",
            "request_id": r.Header.Get("X-Request-Id"),
            "method":     r.Method,
            "path":       r.URL.Path,
        })
        next.ServeHTTP(w, r.WithContext(ctx))
    })
}

// In handlers
func myHandler(w http.ResponseWriter, r *http.Request) {
    log := FromContext(r.Context())
    if log != nil {
        log.Log(kamori.Event{"level": "info", "message": "Handling request"})
    }
}
```

---

## OnDrop handler

```go
client := kamori.New(kamori.Options{
    URL:   "https://your-kamori-server.com",
    Token: "your-log-token",
    OnDrop: func(events []kamori.Event) {
        // Write dropped events to stderr as a fallback
        for _, e := range events {
            fmt.Fprintf(os.Stderr, "kamori dropped: %v\n", e)
        }
    },
})
```

---

## Retry behaviour

Failed requests (5xx or network error) are retried up to three times with exponential back-off. Retries run in the same goroutine as the initial send so the concurrency slot is held across all attempts:

| Attempt   | Delay  |
| --------- | ------ |
| 1st retry | 250 ms |
| 2nd retry | 1 s    |
| 3rd retry | 4 s    |

`4xx` responses are **not** retried (client error). After all retries, `OnDrop` is called.

---

## Types

```go
// Event is an arbitrary log event (JSON object).
type Event map[string]any

// Options configures a Client.
type Options struct {
    URL           string
    Token         string
    BatchSize     int
    FlushInterval time.Duration
    MaxBuffer     int
    Service       string
    MaxConcurrent int
    OnDrop        func(events []Event)
}

// Client buffers log events and sends them to a Kamori ingest server.
type Client struct { ... }
func New(opts Options) *Client
func (c *Client) Log(event Event)
func (c *Client) Flush()
func (c *Client) Shutdown(ctx context.Context) error
func (c *Client) Scoped(defaults Event) *ScopedClient

// ScopedClient wraps a Client and merges default fields into every event.
type ScopedClient struct { ... }
func (s *ScopedClient) Log(event Event)
func (s *ScopedClient) Flush()
func (s *ScopedClient) Shutdown(ctx context.Context) error
```

---

## License

MIT

# `kamori-go` Technical Specification

**Module:** `github.com/usekamori/kamori-go`  
**Source root:** `sdks/go`  
**Package:** `kamori`  
**Primary role:** concurrent-safe buffered Go client for Kamori ingest (`POST /v1/ingest`).

## 1. Purpose

`kamori-go` provides a lightweight logging client that:

- buffers events in-memory,
- flushes automatically by batch size or flush interval,
- sends batches asynchronously over HTTP,
- retries transient failures with exponential backoff,
- exposes drop hooks for failed or capacity-rejected batches.

The client is designed to avoid blocking caller goroutines in normal logging paths.

## 2. Module and Runtime Contract

- Go version: `1.22+` (`go.mod`).
- Public package path: `github.com/usekamori/kamori-go/kamori`.
- JSON event model: `type Event map[string]any`.

## 3. Public API

## 3.1 Types

- `type Options struct`
  - `URL string` (required)
  - `Token string`
  - `BatchSize int` (default `50`)
  - `FlushInterval time.Duration` (default `2s`)
  - `MaxBuffer int` (default `5*BatchSize`; `-1` disables limit)
  - `Service string` (default service key merged if event missing `service`)
  - `MaxConcurrent int` (default `4`)
  - `OnDrop func(events []Event)` (optional callback)

- `type Client struct`
- `type ScopedClient struct`

## 3.2 Constructors / Methods

- `New(opts Options) *Client`
- `(*Client) Log(event Event)`
- `(*Client) Flush()`
- `(*Client) Shutdown(ctx context.Context) error`
- `(*Client) Scoped(defaults Event) *ScopedClient`

- `(*ScopedClient) Log(event Event)`
- `(*ScopedClient) Flush()`
- `(*ScopedClient) Shutdown(ctx context.Context) error`

## 4. Core Behavior

## 4.1 Event buffering (`Log`)

- Thread-safe (`sync.Mutex` around mutable state).
- If `Service` option is set and event has no `service`, client injects it.
- If client has been shut down (`stopped=true`), `Log` is a no-op.
- If `MaxBuffer > 0` and buffer is full:
  - incoming event is dropped immediately,
  - `OnDrop` called with a single-event batch.
- Auto-flush triggers:
  - buffer length reaches `BatchSize`, or
  - one-shot timer fires after `FlushInterval`.

## 4.2 Flush semantics (`Flush`)

- Safe for concurrent calls.
- If buffer empty: no-op.
- Drains buffer atomically and clears timer.
- Send path is asynchronous:
  - tries to acquire semaphore slot (`MaxConcurrent`) non-blocking.
  - if slot acquired: launches goroutine and sends batch with retries.
  - if slot unavailable: drops whole drained batch (`OnDrop`).

This guarantees `Flush()` does not block callers waiting for send capacity.

## 4.3 Shutdown semantics (`Shutdown`)

- Marks client stopped (future `Log` calls ignored).
- Stops flush timer and drains remaining buffered events.
- Attempts to schedule final drained batch:
  - if semaphore slot unavailable, batch is dropped.
- Waits for all in-flight send goroutines via `WaitGroup`.
- Wait is context-bounded:
  - returns `nil` when all sends complete,
  - returns `ctx.Err()` on timeout/cancel.

## 5. HTTP Transport Contract

Single batch send (`sendWithRetry` + `http.Client`):

- endpoint: `POST <URL>/v1/ingest`
- headers:
  - `Content-Type: application/json`
  - `Authorization: Bearer <Token>` (only when token non-empty)
- body: JSON array of events (`[]Event`)
- client timeout: `10s`

Status handling:

- `2xx`: success
- `4xx`: drop immediately (no retry)
- `5xx`: retry with backoff
- network/request construction/marshal errors: retry (where applicable) then drop

## 6. Retry and Drop Policy

Backoff schedule (`retryDelays`):

1. `250ms`
2. `1s`
3. `4s`

Total attempts for retriable failures:

- initial attempt + 3 retries = 4 attempts max.

Retry execution model:

- retries run in the same send goroutine (`time.Sleep` + recursive call),
- semaphore slot is held for the whole retry lifecycle.

Drop callback behavior:

- `OnDrop` invoked for:
  - permanent send failure after retries,
  - buffer overflow rejection in `Log`,
  - flush/shutdown rejection when concurrency semaphore is saturated.

## 7. Concurrency and Safety

- `Log`, `Flush`, and `Shutdown` are intended for concurrent use.
- Internal synchronization primitives:
  - `sync.Mutex` for buffer/timer/stopped state
  - buffered channel semaphore for in-flight sends
  - `sync.WaitGroup` for shutdown coordination
- No caller-visible panics are intentionally raised in normal flow.

## 8. Scoped Client Contract

`Scoped(defaults)` returns a lightweight wrapper that:

- merges defaults into each event,
- event keys override scoped defaults on conflicts,
- delegates to parent client for buffer/timer/send behavior.

No separate goroutines, buffers, or HTTP clients are created per scope.

## 9. Tested Guarantees (from `client_test.go`)

Validated behavior includes:

- correct ingest path `/v1/ingest`
- auth header presence/absence based on token
- batch JSON array payload encoding
- manual flush and auto-flush by batch size
- auto-flush by flush interval timer
- default `Service` injection
- scoped merge behavior and override precedence
- `4xx` immediate drop without retry
- `5xx` retries (4 total attempts) then drop
- retry-success path without drop callback
- no-op flush on empty buffer
- concurrency safety under parallel logging (`-race` target)
- `MaxConcurrent` saturation causes immediate batch drop

## 10. Non-Goals / Out of Scope

- durable local disk queueing for offline mode
- response body contract handling beyond HTTP status classes
- strict event schema validation on client side
- guaranteed delivery when caller overload exceeds `MaxBuffer`/`MaxConcurrent`

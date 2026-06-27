// Package kamori provides a Go client for the Kamori self-hosted log ingest server.
//
// Usage:
//
//	client := kamori.New(kamori.Options{
//	    URL:   "https://kamori.example.com",
//	    Token: "your-log-token",
//	})
//	defer client.Shutdown(context.Background())
//
//	client.Log(kamori.Event{"level": "info", "message": "hello"})
package kamori

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Event is an arbitrary log event (JSON object). Any JSON-serialisable key/value
// pairs are accepted. The keys "level", "service", and "message" are indexed by
// the Kamori server for fast filtering.
type Event map[string]any

// Options configures a Client.
type Options struct {
	// URL is the base URL of the Kamori ingest server (required).
	// E.g. "https://kamori.example.com"
	URL string

	// Token is the auth token (INGEST_TOKEN) set on the server. Optional.
	Token string

	// BatchSize is the max number of events to buffer before flushing.
	// Default: 50.
	BatchSize int

	// FlushInterval is the max time to wait before flushing the buffer.
	// Default: 2s.
	FlushInterval time.Duration

	// MaxBuffer is the maximum number of events to hold in the in-memory
	// buffer. Log() drops new events (calling OnDrop) once this limit is
	// reached so the caller is never blocked and memory is bounded.
	// Default: 5 * BatchSize (i.e. 250 by default).
	// Set to -1 to disable the limit.
	MaxBuffer int

	// Service is a default service name merged into every event. Optional.
	// Overridden per-event if the event already contains a "service" key.
	Service string

	// MaxConcurrent caps the number of goroutines performing HTTP sends at
	// any one time. Under sustained server unavailability retries accumulate;
	// this semaphore prevents unbounded goroutine growth.
	// Default: 4.
	MaxConcurrent int

	// OnDrop is called when a batch is permanently dropped after all retries,
	// or when the buffer is full and a new event cannot be accepted.
	// Optional. Useful for writing dropped events to stderr or a fallback sink.
	OnDrop func(events []Event)
}

// retryDelays defines the backoff schedule between send attempts.
var retryDelays = []time.Duration{250 * time.Millisecond, time.Second, 4 * time.Second}

// Client buffers log events and sends them to a Kamori ingest server.
// All methods are safe for concurrent use from multiple goroutines.
type Client struct {
	opts    Options
	mu      sync.Mutex
	buffer  []Event
	timer   *time.Timer
	httpCli *http.Client
	// sem is a counting semaphore that caps the number of in-flight HTTP sends.
	sem chan struct{}
	// wg tracks all in-flight send goroutines so Shutdown can wait for them.
	wg sync.WaitGroup
	// stopped is set by Shutdown; Log() becomes a no-op once set.
	stopped bool
}

// New creates a new Client with the given options.
func New(opts Options) *Client {
	if opts.BatchSize <= 0 {
		opts.BatchSize = 50
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = 2 * time.Second
	}
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = 4
	}
	if opts.MaxBuffer == 0 {
		opts.MaxBuffer = 5 * opts.BatchSize
	}

	return &Client{
		opts:    opts,
		httpCli: &http.Client{Timeout: 10 * time.Second},
		sem:     make(chan struct{}, opts.MaxConcurrent),
	}
}

// Log queues an event. Flushes automatically when the buffer is full or the
// flush interval fires. Drops the event (calling OnDrop) when the buffer has
// reached MaxBuffer to keep memory bounded without blocking the caller.
func (c *Client) Log(event Event) {
	// Merge default service if set and not already present in the event.
	if c.opts.Service != "" {
		if _, ok := event["service"]; !ok {
			merged := make(Event, len(event)+1)
			for k, v := range event {
				merged[k] = v
			}
			merged["service"] = c.opts.Service
			event = merged
		}
	}

	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	// Drop new events when the buffer is full to keep memory bounded.
	if c.opts.MaxBuffer > 0 && len(c.buffer) >= c.opts.MaxBuffer {
		c.mu.Unlock()
		c.drop([]Event{event})
		return
	}
	c.buffer = append(c.buffer, event)
	full := len(c.buffer) >= c.opts.BatchSize
	if !full && c.timer == nil {
		c.timer = time.AfterFunc(c.opts.FlushInterval, c.Flush)
	}
	c.mu.Unlock()

	if full {
		c.Flush()
	}
}

// Flush sends all buffered events immediately. Safe to call concurrently.
// The send itself is async (goroutine); Flush returns as soon as the local
// buffer is drained.
func (c *Client) Flush() {
	c.mu.Lock()
	if len(c.buffer) == 0 {
		c.mu.Unlock()
		return
	}
	events := c.buffer
	c.buffer = nil
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.mu.Unlock()

	// Acquire the semaphore before launching the goroutine so the slot is
	// held for the lifetime of the send (including retries).
	// Non-blocking: if at capacity, drop this batch rather than block the caller.
	// A logging client must never stall the application.
	select {
	case c.sem <- struct{}{}:
		c.wg.Add(1)
		go func() {
			defer func() {
				<-c.sem
				c.wg.Done()
			}()
			c.sendWithRetry(events, 0)
		}()
	default:
		c.drop(events)
	}
}

// Shutdown flushes all buffered events, waits for in-flight sends to finish,
// and stops the client. After Shutdown returns, Log() is a no-op.
//
// The context deadline/cancel controls the maximum wait time for in-flight
// goroutines. Returns ctx.Err() if the context expires before all goroutines
// finish (events may be lost), nil on clean shutdown.
func (c *Client) Shutdown(ctx context.Context) error {
	// Mark stopped so no new events are accepted, then flush remaining buffer.
	c.mu.Lock()
	c.stopped = true
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	events := c.buffer
	c.buffer = nil
	c.mu.Unlock()

	if len(events) > 0 {
		select {
		case c.sem <- struct{}{}:
			c.wg.Add(1)
			go func() {
				defer func() {
					<-c.sem
					c.wg.Done()
				}()
				c.sendWithRetry(events, 0)
			}()
		default:
			c.drop(events)
		}
	}

	// Wait for all in-flight sends to complete, honouring the context deadline.
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) sendWithRetry(events []Event, attempt int) {
	body, err := json.Marshal(events)
	if err != nil {
		c.drop(events)
		return
	}

	req, err := http.NewRequest(http.MethodPost, c.opts.URL+"/v1/ingest", bytes.NewReader(body))
	if err != nil {
		c.drop(events)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if c.opts.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.opts.Token)
	}

	resp, err := c.httpCli.Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode < 500 {
			// 2xx: success. 4xx: client error — don't retry.
			if resp.StatusCode >= 400 {
				c.drop(events)
			}
			return
		}
	}

	// 5xx or network error — schedule retry with backoff.
	// The retry runs in the same goroutine (via time.Sleep) so the semaphore
	// slot is held across all attempts for this batch, preventing the slot
	// count from growing with each retry.
	if attempt < len(retryDelays) {
		time.Sleep(retryDelays[attempt])
		c.sendWithRetry(events, attempt+1)
		return
	}

	c.drop(events)
}

func (c *Client) drop(events []Event) {
	if c.opts.OnDrop != nil {
		c.opts.OnDrop(events)
	}
}

// Scoped returns a ScopedClient that merges defaults into every logged event.
// The scoped client shares the parent's buffer, flush timer, and HTTP client —
// it is a lightweight view over the same underlying Client. No extra goroutines
// are created.
func (c *Client) Scoped(defaults Event) *ScopedClient {
	return &ScopedClient{parent: c, defaults: defaults}
}

// ScopedClient wraps a Client and merges default fields into every event.
// Fields in the event passed to Log take priority over the scoped defaults.
type ScopedClient struct {
	parent   *Client
	defaults Event
}

// Log merges defaults then delegates to the parent client.
// Event fields take priority over scoped defaults when keys collide.
func (s *ScopedClient) Log(event Event) {
	merged := make(Event, len(s.defaults)+len(event))
	for k, v := range s.defaults {
		merged[k] = v
	}
	for k, v := range event {
		merged[k] = v
	}
	s.parent.Log(merged)
}

// Flush delegates to the parent client.
func (s *ScopedClient) Flush() { s.parent.Flush() }

// Shutdown delegates to the parent client.
func (s *ScopedClient) Shutdown(ctx context.Context) error { return s.parent.Shutdown(ctx) }

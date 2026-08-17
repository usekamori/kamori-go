package kamori_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/usekamori/kamori-go/kamori"
)

// recvJSON decodes the request body and passes it to h, then writes a 200.
func recvJSON(t *testing.T, h func([]map[string]any)) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		var events []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&events); err != nil {
			t.Errorf("decode: %v", err)
		}
		h(events)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"written":1}`))
	}
}

func TestLogAndFlush(t *testing.T) {
	done := make(chan []map[string]any, 1)
	srv := httptest.NewServer(recvJSON(t, func(ev []map[string]any) { done <- ev }))
	defer srv.Close()

	c := kamori.New(kamori.Options{URL: srv.URL})
	c.Log(kamori.Event{"level": "info", "message": "hello"})
	c.Flush()

	select {
	case events := <-done:
		if len(events) != 1 {
			t.Fatalf("expected 1 event, got %d", len(events))
		}
		if events[0]["message"] != "hello" {
			t.Errorf("unexpected message: %v", events[0]["message"])
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for request")
	}
}

func TestAutoFlushOnBatchSize(t *testing.T) {
	flushed := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flushed <- struct{}{}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := kamori.New(kamori.Options{URL: srv.URL, BatchSize: 3})
	c.Log(kamori.Event{"n": 1})
	c.Log(kamori.Event{"n": 2})
	c.Log(kamori.Event{"n": 3})

	select {
	case <-flushed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("auto-flush did not fire within timeout")
	}
}

func TestDefaultService(t *testing.T) {
	done := make(chan []map[string]any, 1)
	srv := httptest.NewServer(recvJSON(t, func(ev []map[string]any) { done <- ev }))
	defer srv.Close()

	c := kamori.New(kamori.Options{URL: srv.URL, Service: "my-svc"})
	c.Log(kamori.Event{"message": "test"})
	c.Flush()

	select {
	case events := <-done:
		if len(events) == 0 {
			t.Fatal("no events received")
		}
		if events[0]["service"] != "my-svc" {
			t.Errorf("expected service=my-svc, got %v", events[0]["service"])
		}
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestScopedClient(t *testing.T) {
	done := make(chan []map[string]any, 1)
	srv := httptest.NewServer(recvJSON(t, func(ev []map[string]any) { done <- ev }))
	defer srv.Close()

	c := kamori.New(kamori.Options{URL: srv.URL})
	scoped := c.Scoped(kamori.Event{"service": "api", "env": "test"})
	scoped.Log(kamori.Event{"message": "scoped event"})
	scoped.Flush()

	select {
	case events := <-done:
		if len(events) == 0 {
			t.Fatal("no events received")
		}
		if events[0]["service"] != "api" || events[0]["env"] != "test" {
			t.Errorf("defaults not merged: %v", events[0])
		}
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestAuthTokenHeader(t *testing.T) {
	gotAuth := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := kamori.New(kamori.Options{URL: srv.URL, Token: "my-secret-token"})
	c.Log(kamori.Event{"message": "auth test"})
	c.Flush()

	select {
	case auth := <-gotAuth:
		if auth != "Bearer my-secret-token" {
			t.Errorf("expected Bearer my-secret-token, got %q", auth)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestNoTokenHeaderWhenTokenEmpty(t *testing.T) {
	gotAuth := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := kamori.New(kamori.Options{URL: srv.URL})
	c.Log(kamori.Event{"message": "no auth"})
	c.Flush()

	select {
	case auth := <-gotAuth:
		if auth != "" {
			t.Errorf("expected no Authorization header, got %q", auth)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestIngestURLPath(t *testing.T) {
	gotPath := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath <- r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := kamori.New(kamori.Options{URL: srv.URL})
	c.Log(kamori.Event{"message": "path check"})
	c.Flush()

	select {
	case path := <-gotPath:
		if path != "/v1/ingest" {
			t.Errorf("expected path /v1/ingest, got %q", path)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestSendsBatchAsJSONArray(t *testing.T) {
	done := make(chan []map[string]any, 1)
	srv := httptest.NewServer(recvJSON(t, func(ev []map[string]any) { done <- ev }))
	defer srv.Close()

	c := kamori.New(kamori.Options{URL: srv.URL, BatchSize: 2})
	c.Log(kamori.Event{"n": 1})
	c.Log(kamori.Event{"n": 2})

	select {
	case events := <-done:
		if len(events) != 2 {
			t.Fatalf("expected 2 events in batch, got %d", len(events))
		}
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestFourXXDropsImmediatelyNoRetry(t *testing.T) {
	var callCount atomic.Int32
	dropped := make(chan []kamori.Event, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := kamori.New(kamori.Options{
		URL:    srv.URL,
		OnDrop: func(events []kamori.Event) { dropped <- events },
	})
	c.Log(kamori.Event{"message": "bad auth"})
	c.Flush()

	select {
	case evts := <-dropped:
		if n := callCount.Load(); n != 1 {
			t.Errorf("expected 1 HTTP call on 4xx (no retry), got %d", n)
		}
		if len(evts) != 1 {
			t.Errorf("expected 1 dropped event, got %d", len(evts))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drop handler never called on 4xx")
	}
}

func TestFiveXXRetriesThreeTimesThenDrops(t *testing.T) {
	var callCount atomic.Int32
	dropped := make(chan []kamori.Event, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := kamori.New(kamori.Options{
		URL:    srv.URL,
		OnDrop: func(events []kamori.Event) { dropped <- events },
	})
	c.Log(kamori.Event{"message": "server down"})
	c.Flush()

	select {
	case evts := <-dropped:
		if n := callCount.Load(); n != 4 {
			t.Errorf("expected 4 HTTP calls (1 + 3 retries), got %d", n)
		}
		if len(evts) != 1 {
			t.Errorf("expected 1 dropped event, got %d", len(evts))
		}
	case <-time.After(15 * time.Second):
		t.Fatal("drop handler never called after 5xx retries")
	}
}

func TestFiveXXSucceedsOnRetry(t *testing.T) {
	var callCount atomic.Int32
	dropped := make(chan struct{}, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if callCount.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := kamori.New(kamori.Options{
		URL:    srv.URL,
		OnDrop: func([]kamori.Event) { dropped <- struct{}{} },
	})
	c.Log(kamori.Event{"message": "transient error"})
	c.Flush()

	// Wait for the 250ms retry + response.
	time.Sleep(500 * time.Millisecond)

	select {
	case <-dropped:
		t.Error("on_drop must not be called when a retry succeeds")
	default:
	}
	if n := callCount.Load(); n < 2 {
		t.Errorf("expected at least 2 HTTP calls, got %d", n)
	}
}

func TestFlushOnEmptyBufferIsNoop(t *testing.T) {
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := kamori.New(kamori.Options{URL: srv.URL})
	c.Flush()
	time.Sleep(50 * time.Millisecond)

	if called.Load() {
		t.Error("Flush() on empty buffer must not make any HTTP calls")
	}
}

func TestDropHandlerOnPermanentFailure(t *testing.T) {
	dropped := make(chan []kamori.Event, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := kamori.New(kamori.Options{
		URL:    srv.URL,
		OnDrop: func(events []kamori.Event) { dropped <- events },
	})
	c.Log(kamori.Event{"message": "will fail"})
	c.Flush()

	select {
	case evts := <-dropped:
		if len(evts) != 1 {
			t.Errorf("expected 1 dropped event, got %d", len(evts))
		}
	case <-time.After(15 * time.Second):
		t.Fatal("drop handler never called")
	}
}

func TestFlushInterval(t *testing.T) {
	flushed := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flushed <- struct{}{}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := kamori.New(kamori.Options{URL: srv.URL, FlushInterval: 100 * time.Millisecond})
	c.Log(kamori.Event{"message": "interval test"})

	select {
	case <-flushed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("auto-flush via FlushInterval did not fire within timeout")
	}
}

func TestScopedClientEventFieldsOverrideDefaults(t *testing.T) {
	done := make(chan []map[string]any, 1)
	srv := httptest.NewServer(recvJSON(t, func(ev []map[string]any) { done <- ev }))
	defer srv.Close()

	c := kamori.New(kamori.Options{URL: srv.URL})
	sc := c.Scoped(kamori.Event{"service": "default-svc", "env": "production"})
	sc.Log(kamori.Event{"service": "override-svc", "message": "hello"})
	sc.Flush()

	select {
	case events := <-done:
		if len(events) != 1 {
			t.Fatalf("expected 1 event, got %d", len(events))
		}
		if events[0]["service"] != "override-svc" {
			t.Errorf("event field should override scoped default; got service=%v", events[0]["service"])
		}
		if events[0]["env"] != "production" {
			t.Errorf("scoped default not merged; got env=%v", events[0]["env"])
		}
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestMaxConcurrentDropsWhenAtCapacity(t *testing.T) {
	unblock := make(chan struct{})
	dropped := make(chan []kamori.Event, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-unblock
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := kamori.New(kamori.Options{
		URL:           srv.URL,
		MaxConcurrent: 1,
		OnDrop:        func(events []kamori.Event) { dropped <- events },
	})

	c.Log(kamori.Event{"batch": 1})
	c.Flush()
	time.Sleep(20 * time.Millisecond)

	c.Log(kamori.Event{"batch": 2})
	c.Flush()

	select {
	case evts := <-dropped:
		if len(evts) != 1 {
			t.Errorf("expected 1 dropped event, got %d", len(evts))
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("OnDrop was not called when MaxConcurrent was exceeded")
	}

	close(unblock)
}

func TestMaxBufferDropsWhenFull(t *testing.T) {
	dropped := make(chan []kamori.Event, 10)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Second)
	}))
	defer srv.Close()

	c := kamori.New(kamori.Options{
		URL:       srv.URL,
		BatchSize: 100,
		MaxBuffer: 2,
		OnDrop:    func(events []kamori.Event) { dropped <- events },
	})

	c.Log(kamori.Event{"n": 1})
	c.Log(kamori.Event{"n": 2})
	c.Log(kamori.Event{"n": 3})

	select {
	case evts := <-dropped:
		if len(evts) != 1 {
			t.Errorf("expected 1 dropped event, got %d", len(evts))
		}
		if evts[0]["n"] != 3 {
			t.Errorf("expected dropped event n=3, got %v", evts[0]["n"])
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("OnDrop was not called when MaxBuffer was exceeded")
	}
}

func TestShutdownContextDeadline(t *testing.T) {
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-unblock
	}))
	defer func() {
		close(unblock)
		srv.Close()
	}()

	c := kamori.New(kamori.Options{URL: srv.URL, BatchSize: 1})
	c.Log(kamori.Event{"message": "will block"})
	c.Flush()
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if err := c.Shutdown(ctx); err == nil {
		t.Error("expected Shutdown to return an error when context deadline exceeded")
	}
}

func TestConcurrentLogIsSafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := kamori.New(kamori.Options{URL: srv.URL, BatchSize: 10})

	done := make(chan struct{})
	for i := 0; i < 5; i++ {
		go func(n int) {
			for j := 0; j < 20; j++ {
				c.Log(kamori.Event{"goroutine": n, "seq": j})
			}
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 5; i++ {
		<-done
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.Shutdown(ctx)
}

// --- F11: HTTPS scheme enforcement ---

func TestInsecureNonLocalhostURLDropsEvents(t *testing.T) {
	var dropped int64
	c := kamori.New(kamori.Options{
		URL:    "http://example.com", // plaintext, non-localhost, not allowed
		Token:  "secret",
		OnDrop: func(events []kamori.Event) { atomic.AddInt64(&dropped, int64(len(events))) },
	})
	c.Log(kamori.Event{"message": "sensitive"})
	c.Flush()
	if got := atomic.LoadInt64(&dropped); got != 1 {
		t.Fatalf("expected 1 dropped event for insecure URL, got %d", got)
	}
}

func TestInsecureURLAllowedWithFlag(t *testing.T) {
	var received int64
	srv := httptest.NewServer(recvJSON(t, func(events []map[string]any) {
		atomic.AddInt64(&received, int64(len(events)))
	}))
	defer srv.Close()

	// srv.URL is http://127.0.0.1:... (loopback) — allowed anyway; assert the
	// AllowInsecure path does not block delivery.
	c := kamori.New(kamori.Options{URL: srv.URL, AllowInsecure: true})
	c.Log(kamori.Event{"message": "ok"})
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if got := atomic.LoadInt64(&received); got != 1 {
		t.Fatalf("expected 1 received event, got %d", got)
	}
}

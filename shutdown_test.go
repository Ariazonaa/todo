package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The SSE stream intentionally runs forever. Without the shutdown case,
// srv.Shutdown would wait on it until the 15-second timeout kicks in —
// every deploy would then hang for as long as devices keep the app open.
func TestEventStreamEndsOnShutdown(t *testing.T) {
	s := &server{hub: newHub(), shutdown: make(chan struct{})}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	zurueck := make(chan struct{})
	go func() {
		s.handleEvents(rec, req)
		close(zurueck)
	}()

	// wait until the handler is actually attached to the hub
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.hub.mu.Lock()
		n := len(s.hub.subs)
		s.hub.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Handler hat sich nicht am Hub angemeldet")
		}
		time.Sleep(5 * time.Millisecond)
	}

	close(s.shutdown)
	select {
	case <-zurueck:
	case <-time.After(3 * time.Second):
		t.Fatal("handleEvents kehrt beim Herunterfahren nicht zurück")
	}

	// and the hub must not keep the subscriber around
	s.hub.mu.Lock()
	n := len(s.hub.subs)
	s.hub.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d Abonnent(en) übrig", n)
	}
}

// A request that ends on its own (tab closed) must still exit cleanly,
// without anything being shut down.
func TestEventStreamEndsWhenClientDisconnects(t *testing.T) {
	s := &server{hub: newHub(), shutdown: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)

	zurueck := make(chan struct{})
	go func() {
		s.handleEvents(httptest.NewRecorder(), req)
		close(zurueck)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-zurueck:
	case <-time.After(3 * time.Second):
		t.Fatal("handleEvents kehrt nach Verbindungsabbruch nicht zurück")
	}
}

// main waits on sched.done before closing the DB — so the scheduler must
// react to the canceled context, or every stop hangs for twelve seconds
// in the timeout.
func TestSchedulerStopsOnContextCancel(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()
	store, err := newSettingsStore(db)
	if err != nil {
		t.Fatalf("settingsStore: %v", err)
	}
	sched := newScheduler(db, store, newHub(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	go sched.run(ctx)
	time.Sleep(50 * time.Millisecond) // first tick runs immediately
	cancel()

	select {
	case <-sched.done:
	case <-time.After(3 * time.Second):
		t.Fatal("Scheduler läuft nach abgebrochenem Kontext weiter")
	}
}

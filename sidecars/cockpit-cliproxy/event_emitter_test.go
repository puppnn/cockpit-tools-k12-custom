package main

import (
	"sync"
	"testing"
	"time"
)

type stalledEventWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *stalledEventWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}

func TestEventEmitterDoesNotBlockWhenWriterStalls(t *testing.T) {
	writer := &stalledEventWriter{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	defer close(writer.release)
	emitter := newEventEmitter(writer)
	emitter.emit(map[string]any{"type": "first"})

	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("event writer did not receive the first event")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for index := 0; index < eventEmitterQueueCapacity*2; index++ {
			emitter.emit(map[string]any{"type": "queued", "index": index})
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("event emission blocked behind a stalled writer")
	}
	if emitter.dropped.Load() == 0 {
		t.Fatal("expected the bounded event queue to drop diagnostics")
	}
}

func TestEventEmitterNeverDropsAccountingEvents(t *testing.T) {
	writer := &stalledEventWriter{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	emitter := newEventEmitter(writer)
	emitter.emit(map[string]any{"type": "first"})
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("event writer did not receive the first event")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for index := 0; index < eventEmitterQueueCapacity+1; index++ {
			emitter.emit(usagePayload{Type: "usage", RequestID: "request"})
		}
	}()

	select {
	case <-done:
		t.Fatal("critical accounting emission bypassed queue backpressure")
	case <-time.After(50 * time.Millisecond):
	}
	if emitter.dropped.Load() != 0 {
		t.Fatalf("critical accounting events were dropped: %d", emitter.dropped.Load())
	}

	close(writer.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("critical accounting emission did not resume after writer recovery")
	}
	if !emitter.flush(time.Second) {
		t.Fatal("critical accounting events were not fully written")
	}
}

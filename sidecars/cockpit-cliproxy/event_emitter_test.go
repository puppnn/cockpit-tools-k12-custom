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

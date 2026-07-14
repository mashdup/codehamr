package protocol

import (
	"sync"
	"time"
)

// outputStreamer coalesces the firehose of tiny writes a running command emits
// into a bounded rate of tool_output_delta events. A high-throughput build
// (`npm install`, `cargo build`) can emit thousands of writes a second; sending
// one NDJSON line each would drown the IPC channel and the renderer. Instead
// chunks accumulate in a buffer flushed on a size threshold (immediately, to
// bound memory and latency) or a time tick (to keep the stream live when output
// trickles).
type outputStreamer struct {
	callID string
	flush  func(callID, text string) // emits one tool_output_delta

	mu   sync.Mutex
	buf  []byte
	stop chan struct{}
	done chan struct{}
}

const (
	// streamFlushBytes flushes eagerly once the pending buffer crosses this, so
	// a burst doesn't grow unbounded or lag behind the terminal.
	streamFlushBytes = 8 << 10
	// streamFlushInterval flushes trickling output so a slow command still
	// shows progress promptly.
	streamFlushInterval = 60 * time.Millisecond
)

// newOutputStreamer starts a coalescing streamer for one tool call. Call write
// from the tool's output sink and close() once the tool returns to flush the
// tail and stop the ticker.
func newOutputStreamer(callID string, flush func(callID, text string)) *outputStreamer {
	s := &outputStreamer{
		callID: callID,
		flush:  flush,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go s.loop()
	return s
}

// write appends a chunk. The sink hands us a buffer it reuses, so we copy.
// Flushes inline when the pending buffer crosses the size threshold.
func (s *outputStreamer) write(p []byte) {
	s.mu.Lock()
	s.buf = append(s.buf, p...)
	over := len(s.buf) >= streamFlushBytes
	s.mu.Unlock()
	if over {
		s.emit()
	}
}

func (s *outputStreamer) loop() {
	defer close(s.done)
	t := time.NewTicker(streamFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.emit()
		case <-s.stop:
			return
		}
	}
}

// emit flushes the pending buffer as a single delta event, if non-empty.
func (s *outputStreamer) emit() {
	s.mu.Lock()
	if len(s.buf) == 0 {
		s.mu.Unlock()
		return
	}
	text := string(s.buf)
	s.buf = s.buf[:0]
	s.mu.Unlock()
	s.flush(s.callID, text)
}

// close stops the ticker and flushes whatever remains. Safe to call once.
func (s *outputStreamer) close() {
	close(s.stop)
	<-s.done
	s.emit()
}

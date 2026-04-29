// Package logbuf provides a per-sandbox bounded ring buffer for exec output.
// Writes are tagged stdout/stderr and recorded in temporal order. Snapshot
// returns the current contents; Subscribe streams new entries to a channel
// (lossy under back-pressure — slow subscribers drop entries rather than
// blocking writers).
package logbuf

import (
	"io"
	"sync"
	"time"
)

type Stream byte

const (
	Stdout Stream = 1
	Stderr Stream = 2
)

// Entry is one chunk of output as captured at write time.
type Entry struct {
	Stream Stream
	Data   []byte
	When   time.Time
}

// Buffer is a bounded log buffer for one sandbox. Bound is total bytes across
// all entries; oldest entries are evicted FIFO when the bound is exceeded.
// Safe for concurrent use.
type Buffer struct {
	mu      sync.Mutex
	entries []Entry
	bytes   int
	max     int
	subs    map[chan Entry]struct{}
}

// New returns a buffer with the given byte cap. A maxBytes <= 0 disables
// capture entirely (writes become no-ops).
func New(maxBytes int) *Buffer {
	return &Buffer{
		max:  maxBytes,
		subs: make(map[chan Entry]struct{}),
	}
}

// WriteStdout / WriteStderr return io.Writers that append into the buffer
// tagged with the appropriate stream. Useful for io.MultiWriter.
func (b *Buffer) WriteStdout() io.Writer { return &streamWriter{b: b, s: Stdout} }
func (b *Buffer) WriteStderr() io.Writer { return &streamWriter{b: b, s: Stderr} }

// AppendStdout / AppendStderr append a chunk directly. Used after buffered
// Exec returns, where the runtime already accumulated the strings.
func (b *Buffer) AppendStdout(p []byte) { b.write(Stdout, p) }
func (b *Buffer) AppendStderr(p []byte) { b.write(Stderr, p) }

// Snapshot returns a copy of all entries currently in the buffer.
func (b *Buffer) Snapshot() []Entry {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Entry, len(b.entries))
	copy(out, b.entries)
	return out
}

// Subscribe returns a channel of new entries plus a cancel func. The channel
// has a small buffer; if the consumer can't keep up, sends are dropped
// rather than blocking the writer (callers expecting completeness should
// pair with Snapshot first).
func (b *Buffer) Subscribe() (<-chan Entry, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan Entry, 64)
	b.subs[ch] = struct{}{}
	cancel := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
	}
	return ch, cancel
}

func (b *Buffer) write(s Stream, p []byte) {
	if b == nil || b.max <= 0 || len(p) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := make([]byte, len(p))
	copy(cp, p)
	e := Entry{Stream: s, Data: cp, When: time.Now()}
	b.entries = append(b.entries, e)
	b.bytes += len(p)
	for b.bytes > b.max && len(b.entries) > 0 {
		b.bytes -= len(b.entries[0].Data)
		b.entries = b.entries[1:]
	}
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
			// Subscriber lagged; drop. Snapshot still has the entry.
		}
	}
}

type streamWriter struct {
	b *Buffer
	s Stream
}

func (w *streamWriter) Write(p []byte) (int, error) {
	w.b.write(w.s, p)
	return len(p), nil
}

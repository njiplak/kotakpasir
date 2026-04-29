package logbuf

import (
	"bytes"
	"testing"
	"time"
)

func TestAppendAndSnapshot(t *testing.T) {
	b := New(1024)
	b.AppendStdout([]byte("hello "))
	b.AppendStderr([]byte("err\n"))
	b.AppendStdout([]byte("world"))

	snap := b.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("want 3 entries, got %d", len(snap))
	}
	if snap[0].Stream != Stdout || !bytes.Equal(snap[0].Data, []byte("hello ")) {
		t.Errorf("entry 0 unexpected: %+v", snap[0])
	}
	if snap[1].Stream != Stderr {
		t.Errorf("entry 1 should be stderr, got %v", snap[1].Stream)
	}
}

func TestEviction(t *testing.T) {
	b := New(10) // 10 bytes cap
	b.AppendStdout([]byte("aaaa"))
	b.AppendStdout([]byte("bbbb"))
	b.AppendStdout([]byte("cccc")) // total would be 12, must evict "aaaa"

	snap := b.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("want 2 entries after eviction, got %d", len(snap))
	}
	if !bytes.Equal(snap[0].Data, []byte("bbbb")) {
		t.Errorf("oldest survivor should be bbbb, got %s", snap[0].Data)
	}
}

func TestSubscribeReceivesNewEntries(t *testing.T) {
	b := New(1024)
	ch, cancel := b.Subscribe()
	defer cancel()

	go b.AppendStdout([]byte("live"))

	select {
	case e := <-ch:
		if !bytes.Equal(e.Data, []byte("live")) {
			t.Errorf("got %q, want %q", e.Data, "live")
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber timed out waiting for entry")
	}
}

func TestSubscribeCancelCloses(t *testing.T) {
	b := New(1024)
	ch, cancel := b.Subscribe()
	cancel()
	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed after cancel")
	}
}

func TestZeroCapDisablesCapture(t *testing.T) {
	b := New(0)
	b.AppendStdout([]byte("anything"))
	if got := len(b.Snapshot()); got != 0 {
		t.Errorf("zero-cap should drop, got %d entries", got)
	}
}

func TestStreamWriterTeeIntoBuffer(t *testing.T) {
	b := New(1024)
	w := b.WriteStdout()
	n, err := w.Write([]byte("from writer"))
	if err != nil {
		t.Fatal(err)
	}
	if n != len("from writer") {
		t.Errorf("short write: %d", n)
	}
	if got := b.Snapshot()[0].Data; !bytes.Equal(got, []byte("from writer")) {
		t.Errorf("got %q", got)
	}
}

package logmon

import (
	"bytes"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLogMonitor(t *testing.T) {
	logMonitor := NewWriter(io.Discard)

	var wg sync.WaitGroup

	client1Messages := make([]byte, 0)
	client2Messages := make([]byte, 0)

	defer logMonitor.OnLogData(func(data []byte) {
		client1Messages = append(client1Messages, data...)
		wg.Done()
	})()

	defer logMonitor.OnLogData(func(data []byte) {
		client2Messages = append(client2Messages, data...)
		wg.Done()
	})()

	wg.Add(6) // 2 x 3 writes

	logMonitor.Write([]byte("1"))
	logMonitor.Write([]byte("2"))
	logMonitor.Write([]byte("3"))

	wg.Wait()

	expectedHistory := "123"
	history := string(logMonitor.GetHistory())

	if history != expectedHistory {
		t.Errorf("Expected history: %s, got: %s", expectedHistory, history)
	}

	c1Data := string(client1Messages)
	if c1Data != expectedHistory {
		t.Errorf("Client1 expected %s, got: %s", expectedHistory, c1Data)
	}

	c2Data := string(client2Messages)
	if c2Data != expectedHistory {
		t.Errorf("Client2 expected %s, got: %s", expectedHistory, c2Data)
	}
}

func TestWrite_ImmutableBuffer(t *testing.T) {
	lm := NewWriter(io.Discard)

	msg := []byte("Hello, World!")
	lenmsg := len(msg)

	n, err := lm.Write(msg)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	if n != lenmsg {
		t.Errorf("Expected %d bytes written but got %d", lenmsg, n)
	}

	msg[0] = 'B'

	history := lm.GetHistory()

	expected := []byte("Hello, World!")
	if !bytes.Equal(history, expected) {
		t.Errorf("Expected history to be %q, got %q", expected, history)
	}
}

func TestWrite_LogTimeFormat(t *testing.T) {
	lm := NewWriter(io.Discard)

	lm.timeFormat = time.RFC3339

	lm.Info("Hello, World!")

	history := lm.GetHistory()

	timestamp := ""
	fields := strings.Fields(string(history))
	if len(fields) > 0 {
		timestamp = fields[0]
	} else {
		t.Fatalf("Cannot extract string from history")
	}

	_, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		t.Fatalf("Cannot find timestamp: %v", err)
	}
}

func TestCircularBuffer_WrapAround(t *testing.T) {
	cb := newCircularBuffer(10)

	cb.Write([]byte("hello"))
	if got := string(cb.GetHistory()); got != "hello" {
		t.Errorf("Expected 'hello', got %q", got)
	}

	cb.Write([]byte("world"))
	if got := string(cb.GetHistory()); got != "helloworld" {
		t.Errorf("Expected 'helloworld', got %q", got)
	}

	cb.Write([]byte("12345"))
	if got := string(cb.GetHistory()); got != "world12345" {
		t.Errorf("Expected 'world12345', got %q", got)
	}

	cb.Write([]byte("abcdefghijklmnop"))
	if got := string(cb.GetHistory()); got != "ghijklmnop" {
		t.Errorf("Expected 'ghijklmnop', got %q", got)
	}
}

func TestCircularBuffer_BoundaryConditions(t *testing.T) {
	cb := newCircularBuffer(10)
	if got := cb.GetHistory(); got != nil {
		t.Errorf("Expected nil for empty buffer, got %q", got)
	}

	cb.Write([]byte("1234567890"))
	if got := string(cb.GetHistory()); got != "1234567890" {
		t.Errorf("Expected '1234567890', got %q", got)
	}

	cb = newCircularBuffer(10)
	cb.Write([]byte("12345"))
	cb.Write([]byte("67890"))
	if got := string(cb.GetHistory()); got != "1234567890" {
		t.Errorf("Expected '1234567890', got %q", got)
	}
}

func TestLogMonitor_LazyInit(t *testing.T) {
	lm := NewWriter(io.Discard)

	if lm.buffer != nil {
		t.Error("Expected buffer to be nil before first write")
	}

	if got := lm.GetHistory(); got != nil {
		t.Errorf("Expected nil history before first write, got %q", got)
	}

	lm.Write([]byte("test"))

	if lm.buffer == nil {
		t.Error("Expected buffer to be initialized after write")
	}

	if got := string(lm.GetHistory()); got != "test" {
		t.Errorf("Expected 'test', got %q", got)
	}
}

func TestLogMonitor_Clear(t *testing.T) {
	lm := NewWriter(io.Discard)

	lm.Write([]byte("hello"))
	if got := string(lm.GetHistory()); got != "hello" {
		t.Errorf("Expected 'hello', got %q", got)
	}

	lm.Clear()

	if lm.buffer != nil {
		t.Error("Expected buffer to be nil after Clear")
	}

	if got := lm.GetHistory(); got != nil {
		t.Errorf("Expected nil history after Clear, got %q", got)
	}
}

func TestLogMonitor_ClearAndReuse(t *testing.T) {
	lm := NewWriter(io.Discard)

	lm.Write([]byte("first"))
	lm.Clear()
	lm.Write([]byte("second"))

	if got := string(lm.GetHistory()); got != "second" {
		t.Errorf("Expected 'second' after clear and reuse, got %q", got)
	}
}

// TestLogMonitor_DropsWhenSubscriberBlocked verifies that a stalled subscriber
// can never block Write (the upstream process's stdout drain) and that dropped
// data is reported in-stream with a marker once delivery resumes. See #875.
func TestLogMonitor_DropsWhenSubscriberBlocked(t *testing.T) {
	lm := NewWriter(io.Discard)

	release := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var received [][]byte

	cancel := lm.OnLogData(func(data []byte) {
		// Block the first delivery, stalling the broadcaster goroutine so the
		// hand-off channel and event queue fill and subsequent writes drop.
		once.Do(func() { <-release })
		mu.Lock()
		received = append(received, append([]byte(nil), data...))
		mu.Unlock()
	})
	defer cancel()

	// Flood well past the hand-off channel (1024) + event queue (1000)
	// capacity. None of these writes may block.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 4000; i++ {
			lm.Write([]byte("x"))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Write blocked while subscriber was stalled")
	}

	close(release)

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		found := false
		for _, d := range received {
			if strings.Contains(string(d), "bytes dropped") {
				found = true
				break
			}
		}
		mu.Unlock()
		if found {
			return
		}
		select {
		case <-deadline:
			t.Fatal("expected a 'bytes dropped' marker after resuming delivery")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// waitForGoroutines polls until the goroutine count returns to baseline,
// fataling on timeout. A baseline captured after a quiescent moment is
// stable enough: no background goroutines start/stop in this package.
func waitForGoroutines(t *testing.T, baseline int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if n := runtime.NumGoroutine(); n <= baseline {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutine leak: baseline %d, still %d after %v", baseline, runtime.NumGoroutine(), timeout)
}

// TestMonitor_Close_StopsBroadcastGoroutine is the regression test for the
// per-model monitor leak: every Monitor's broadcast goroutine must exit when
// the monitor is closed, so the monitor (ring buffer, dispatcher, channel)
// becomes garbage-collectable.
func TestMonitor_Close_StopsBroadcastGoroutine(t *testing.T) {
	baseline := runtime.NumGoroutine()

	const n = 50
	monitors := make([]*Monitor, n)
	for i := range monitors {
		monitors[i] = NewWriter(io.Discard)
		monitors[i].Write([]byte("line\n"))
	}
	for _, m := range monitors {
		m.Close()
	}

	waitForGoroutines(t, baseline, 5*time.Second)
}

// TestMonitor_Close_Idempotent verifies Close can be called any number of
// times (router teardown paths do not track whether they already closed).
func TestMonitor_Close_Idempotent(t *testing.T) {
	lm := NewWriter(io.Discard)
	lm.Write([]byte("hello"))
	lm.Close()
	lm.Close()
	lm.Close()
}

// TestMonitor_WriteAfterClose verifies the late-write safety guarantee: a
// stdout-drain write racing teardown must not panic (no send on a closed
// channel), must not block, and must still reach the downstream writer and
// the history buffer. Only live-broadcast delivery stops.
func TestMonitor_WriteAfterClose(t *testing.T) {
	var out bytes.Buffer
	lm := NewWriter(&out)
	lm.Close()

	done := make(chan struct{})
	go func() {
		lm.Write([]byte("late"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Write blocked after Close")
	}

	if got := out.String(); got != "late" {
		t.Errorf("downstream writer got %q, want %q", got, "late")
	}
	if got := string(lm.GetHistory()); got != "late" {
		t.Errorf("history after Close got %q, want %q", got, "late")
	}
}

// TestMonitor_Close_WithActiveSubscriber verifies the teardown contract with
// a live subscriber: Close must not panic, and the subscriber goroutine must
// terminate when its subscription is cancelled (Del), not when the bus is
// closed — the cond.Broadcast in group.Del is what wakes the parked consumer.
func TestMonitor_Close_WithActiveSubscriber(t *testing.T) {
	baseline := runtime.NumGoroutine()

	lm := NewWriter(io.Discard)
	cancel := lm.OnLogData(func(data []byte) {})
	lm.Write([]byte("x"))
	lm.Close()

	// Cancel the subscriber; the parked consumer goroutine must exit.
	cancel()
	waitForGoroutines(t, baseline, 5*time.Second)
}

func BenchmarkLogMonitorWrite(b *testing.B) {
	smallMsg := []byte("small message\n")
	mediumMsg := []byte(strings.Repeat("medium message content ", 10) + "\n")
	largeMsg := []byte(strings.Repeat("large message content for benchmarking ", 100) + "\n")

	b.Run("SmallWrite", func(b *testing.B) {
		lm := NewWriter(io.Discard)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			lm.Write(smallMsg)
		}
	})

	b.Run("MediumWrite", func(b *testing.B) {
		lm := NewWriter(io.Discard)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			lm.Write(mediumMsg)
		}
	})

	b.Run("LargeWrite", func(b *testing.B) {
		lm := NewWriter(io.Discard)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			lm.Write(largeMsg)
		}
	})

	b.Run("WithSubscribers", func(b *testing.B) {
		lm := NewWriter(io.Discard)
		for i := 0; i < 5; i++ {
			lm.OnLogData(func(data []byte) {})
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			lm.Write(mediumMsg)
		}
	})

	b.Run("GetHistory", func(b *testing.B) {
		lm := NewWriter(io.Discard)
		for i := 0; i < 1000; i++ {
			lm.Write(mediumMsg)
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			lm.GetHistory()
		}
	})
}

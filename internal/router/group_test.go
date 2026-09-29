package router

import (
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
)

// newTestGroup builds a Group directly from the supplied processes and config,
// bypassing NewGroup's call to process.New.
func newTestGroup(t *testing.T, conf config.Config, processes map[string]process.Process) *Group {
	t.Helper()
	modelToGroup := make(map[string]string)
	for gid, gcfg := range conf.Routing.Router.Settings.Groups {
		for _, mid := range gcfg.Members {
			modelToGroup[mid] = gid
		}
	}
	swapper := &groupSwapper{
		config:       conf,
		modelToGroup: modelToGroup,
	}
	base, err := newBaseRouter("group", conf, processes, logmon.NewWriter(io.Discard), swapper)
	if err != nil {
		t.Fatalf("newBaseRouter: %v", err)
	}
	base.testProcessed = make(chan struct{}, 64)
	g := &Group{baseRouter: base}
	go base.run()
	t.Cleanup(func() {
		if !g.shuttingDown.Load() {
			_ = g.Shutdown(time.Second)
		}
	})
	return g
}

func TestGroup_NewGroup_DuplicateMembership(t *testing.T) {
	conf := config.Config{
		Routing: groupRouting(map[string]config.GroupConfig{
			"g1": {Swap: true, Members: []string{"a"}},
			"g2": {Swap: true, Members: []string{"a"}},
		}),
		Models: map[string]config.ModelConfig{
			"a": {},
		},
	}
	if _, err := NewGroup(conf, logmon.NewGroup(io.Discard, true, true, true)); err == nil {
		t.Fatalf("expected error for duplicate membership")
	}
}

// TestGroup_NewGroup_ErrorPath_ReleasesMonitors verifies that a failed
// constructor does not leak the per-model log monitors (and their broadcast
// goroutines) created before the error. Map iteration order is random, so a
// goroutine-count assertion covers both orderings: the failing model hit
// first (nothing to release) or last (earlier models' monitors + the
// orphaned procLog must all be closed).
func TestGroup_NewGroup_ErrorPath_ReleasesMonitors(t *testing.T) {
	baseline := runtime.NumGoroutine()

	conf := config.Config{
		Routing: groupRouting(map[string]config.GroupConfig{
			"g1": {Swap: true, Members: []string{"a", "missing"}},
		}),
		Models: map[string]config.ModelConfig{
			"a": {},
		},
	}
	logs := logmon.NewGroup(io.Discard, true, true, true)
	if _, err := NewGroup(conf, logs); err == nil {
		t.Fatal("expected NewGroup to fail: member missing from Models")
	}
	// The error path deliberately does not close the shared log group (in
	// production it is owned by the server and outlives NewGroup), so the
	// test closes its monitors itself; otherwise their broadcastLoop
	// goroutines are the +4 the leak check below would flag.
	logs.MuxLogs.Close()
	logs.ProxyLogs.Close()
	logs.UpstreamLogs.Close()
	logs.HttpLogs.Close()

	deadline := time.Now().Add(5 * time.Second)
	min := runtime.NumGoroutine()
	for time.Now().Before(deadline) {
		if n := runtime.NumGoroutine(); n < min {
			min = n
		}
		if min <= baseline {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	t.Fatalf("goroutines leaked after failed NewGroup: baseline %d, min now %d\nllama-swap goroutines:\n%s",
		baseline, min, leakedGoroutines(buf[:n]))
}

// leakedGoroutines filters a full goroutine stack dump down to goroutines
// that reference this module, so a leak-check failure points at the culprit
// instead of drowning in runtime goroutines.
func leakedGoroutines(dump []byte) string {
	var sb strings.Builder
	for _, block := range strings.Split(string(dump), "\n\n") {
		if strings.Contains(block, "llama-swap") {
			sb.WriteString(block)
			sb.WriteString("\n\n")
		}
	}
	return sb.String()
}

func TestGroup_ServeHTTP_SwapStopsPrevious(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	go a.Run(0) // park a Run goroutine so Stop has something to release

	b := newFakeProcess("b")
	b.autoReady = true

	conf := config.Config{
		HealthCheckTimeout: 5,
		Routing: groupRouting(map[string]config.GroupConfig{
			"g": {Swap: true, Exclusive: true, Members: []string{"a", "b"}},
		}),
	}
	g := newTestGroup(t, conf, map[string]process.Process{"a": a, "b": b})

	w := httptest.NewRecorder()
	g.ServeHTTP(w, newRequest("b"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := a.stopCalls.Load(); got != 1 {
		t.Errorf("a.stopCalls=%d want 1", got)
	}
	if got := b.runCalls.Load(); got != 1 {
		t.Errorf("b.runCalls=%d want 1", got)
	}
	if got := b.serveCalls.Load(); got != 1 {
		t.Errorf("b.serveCalls=%d want 1", got)
	}
}

func TestGroup_NonSwapGroup_NoStop(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()

	b := newFakeProcess("b")
	b.autoReady = true

	conf := config.Config{
		HealthCheckTimeout: 5,
		Routing: groupRouting(map[string]config.GroupConfig{
			"g": {Swap: false, Exclusive: false, Members: []string{"a", "b"}},
		}),
	}
	g := newTestGroup(t, conf, map[string]process.Process{"a": a, "b": b})

	w := httptest.NewRecorder()
	g.ServeHTTP(w, newRequest("b"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := a.stopCalls.Load(); got != 0 {
		t.Errorf("a.stopCalls=%d want 0 (swap=false should not stop siblings)", got)
	}
	if got := b.runCalls.Load(); got != 1 {
		t.Errorf("b.runCalls=%d want 1", got)
	}
}

func TestGroup_CrossGroupExclusive(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	go a.Run(0)

	b := newFakeProcess("b")
	b.autoReady = true

	conf := config.Config{
		HealthCheckTimeout: 5,
		Routing: groupRouting(map[string]config.GroupConfig{
			"g1": {Swap: true, Exclusive: true, Members: []string{"a"}},
			"g2": {Swap: true, Exclusive: true, Members: []string{"b"}},
		}),
	}
	g := newTestGroup(t, conf, map[string]process.Process{"a": a, "b": b})

	w := httptest.NewRecorder()
	g.ServeHTTP(w, newRequest("b"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := a.stopCalls.Load(); got != 1 {
		t.Errorf("a.stopCalls=%d want 1 (cross-group exclusive must stop)", got)
	}
}

// TestGroup_CrossGroupNonExclusiveParallel verifies that two requests for
// models in distinct non-exclusive groups load in parallel rather than
// serializing through the router's run loop.
func TestGroup_CrossGroupNonExclusiveParallel(t *testing.T) {
	a := newFakeProcess("a")
	pb := newFakeProcess("b")

	conf := config.Config{
		HealthCheckTimeout: 5,
		Routing: groupRouting(map[string]config.GroupConfig{
			"g1": {Swap: true, Exclusive: false, Members: []string{"a"}},
			"g2": {Swap: true, Exclusive: false, Members: []string{"b"}},
		}),
	}
	g := newTestGroup(t, conf, map[string]process.Process{"a": a, "b": pb})

	w1 := httptest.NewRecorder()
	done1 := make(chan struct{})
	go func() {
		g.ServeHTTP(w1, newRequest("a"))
		close(done1)
	}()
	waitProcessed(t, g.testProcessed, 1)

	w2 := httptest.NewRecorder()
	done2 := make(chan struct{})
	go func() {
		g.ServeHTTP(w2, newRequest("b"))
		close(done2)
	}()
	waitProcessed(t, g.testProcessed, 1)

	// Both groups load concurrently — both must reach Run() before either is
	// marked ready. If the router still serialised, only one would proceed.
	<-a.runStarted
	<-pb.runStarted

	a.markReady()
	pb.markReady()

	for i, ch := range []chan struct{}{done1, done2} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("request %d did not complete", i)
		}
	}
	if got := a.stopCalls.Load(); got != 0 {
		t.Errorf("a.stopCalls=%d want 0 (parallel groups don't evict each other)", got)
	}
	if got := pb.stopCalls.Load(); got != 0 {
		t.Errorf("b.stopCalls=%d want 0 (parallel groups don't evict each other)", got)
	}
}

// TestGroup_SameGroupSwapSerialises verifies that two same-group requests
// (Swap=true) serialise even when both arrive while neither has reached
// StateStarting yet — the in-flight swap target the scheduler folds into the
// running set closes that race.
func TestGroup_SameGroupSwapSerialises(t *testing.T) {
	a := newFakeProcess("a")
	pb := newFakeProcess("b")

	conf := config.Config{
		HealthCheckTimeout: 5,
		Routing: groupRouting(map[string]config.GroupConfig{
			"g": {Swap: true, Exclusive: false, Members: []string{"a", "b"}},
		}),
	}
	g := newTestGroup(t, conf, map[string]process.Process{"a": a, "b": pb})

	w1 := httptest.NewRecorder()
	done1 := make(chan struct{})
	go func() {
		g.ServeHTTP(w1, newRequest("a"))
		close(done1)
	}()
	waitProcessed(t, g.testProcessed, 1)

	// Request B arrives before A transitions to StateStarting in the process
	// state machine. Without folding the in-flight swap target into the running
	// set, the swapper would not see A as running, and B would start in
	// parallel, violating Swap=true.
	w2 := httptest.NewRecorder()
	done2 := make(chan struct{})
	go func() {
		g.ServeHTTP(w2, newRequest("b"))
		close(done2)
	}()
	waitProcessed(t, g.testProcessed, 1)

	if got := pb.runCalls.Load(); got != 0 {
		t.Errorf("b started in parallel: runCalls=%d want 0", got)
	}

	<-a.runStarted
	a.markReady()
	waitProcessed(t, g.testProcessed, 1) // swapDone(a) → b promoted
	<-pb.runStarted
	pb.markReady()

	for i, ch := range []chan struct{}{done1, done2} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("request %d did not complete", i)
		}
	}
	if got := a.stopCalls.Load(); got != 1 {
		t.Errorf("a.stopCalls=%d want 1 (b's swap must stop a)", got)
	}
}

// TestGroup_PersistentNotEvicted verifies that a group with persistent=true
// is never evicted when another exclusive group starts loading. The running
// model in the persistent group stays alive alongside the new one.
func TestGroup_PersistentNotEvicted(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	go a.Run(0)

	b := newFakeProcess("b")
	b.autoReady = true

	conf := config.Config{
		HealthCheckTimeout: 5,
		Routing: groupRouting(map[string]config.GroupConfig{
			"persist": {Swap: true, Exclusive: false, Persistent: true, Members: []string{"a"}},
			"other":   {Swap: true, Exclusive: true, Members: []string{"b"}},
		}),
	}
	g := newTestGroup(t, conf, map[string]process.Process{"a": a, "b": b})

	w := httptest.NewRecorder()
	g.ServeHTTP(w, newRequest("b"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := a.stopCalls.Load(); got != 0 {
		t.Errorf("a.stopCalls=%d want 0 (persistent group must not be evicted)", got)
	}
	if a.State() != process.StateStarting && a.State() != process.StateReady {
		t.Errorf("a state=%s want still running", a.State())
	}
	if got := b.runCalls.Load(); got != 1 {
		t.Errorf("b.runCalls=%d want 1", got)
	}
}

// TestGroup_NonExclusiveDoesNotUnloadExclusive pins a backwards-compatible
// gotcha from the original ProcessGroup: when a model in a non-exclusive group
// is loaded, any running exclusive group keeps running. The two coexist.
func TestGroup_NonExclusiveDoesNotUnloadExclusive(t *testing.T) {
	a := newFakeProcess("a")
	a.markReady()
	go a.Run(0)

	b := newFakeProcess("b")
	b.autoReady = true

	conf := config.Config{
		HealthCheckTimeout: 5,
		Routing: groupRouting(map[string]config.GroupConfig{
			"g1": {Swap: true, Exclusive: true, Members: []string{"a"}},
			"g2": {Swap: true, Exclusive: false, Members: []string{"b"}},
		}),
	}
	g := newTestGroup(t, conf, map[string]process.Process{"a": a, "b": b})

	w := httptest.NewRecorder()
	g.ServeHTTP(w, newRequest("b"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := a.stopCalls.Load(); got != 0 {
		t.Errorf("a.stopCalls=%d want 0 (non-exclusive target must not unload exclusive group)", got)
	}
	if a.State() != process.StateStarting && a.State() != process.StateReady {
		t.Errorf("a state=%s want still running", a.State())
	}
	if got := b.runCalls.Load(); got != 1 {
		t.Errorf("b.runCalls=%d want 1", got)
	}
}

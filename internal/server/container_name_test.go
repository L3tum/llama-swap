package server

import (
	"errors"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/perf"
)

func TestServer_ContainerNameMapsPIDToContainerRoot(t *testing.T) {
	oldParent := processParentPID
	oldRoots := dockerContainerRoots
	defer func() {
		processParentPID = oldParent
		dockerContainerRoots = oldRoots
	}()

	processParentPID = func(pid int) (int, error) {
		parents := map[int]int{400: 300, 300: 200, 200: 100, 900: 1}
		return parents[pid], nil
	}
	dockerContainerRoots = func() map[int]string {
		return map[int]string{100: "llama-server", 500: "other"}
	}

	r := &containerNameResolver{}
	parentCache := map[int]int{}

	if got := r.nameFor(400, parentCache); got != "llama-server" {
		t.Fatalf("nameFor(400) = %q, want llama-server", got)
	}
	if got := r.nameFor(100, parentCache); got != "llama-server" {
		t.Fatalf("nameFor(100) = %q, want llama-server", got)
	}
	if got := r.nameFor(900, parentCache); got != "" {
		t.Fatalf("nameFor(900) = %q, want empty for non-container PID", got)
	}
	if got := r.nameFor(0, parentCache); got != "" {
		t.Fatalf("nameFor(0) = %q, want empty for zero PID", got)
	}
}

func TestServer_ContainerNameCachesRootsUntilTTLExpires(t *testing.T) {
	oldRoots := dockerContainerRoots
	defer func() { dockerContainerRoots = oldRoots }()

	calls := 0
	dockerContainerRoots = func() map[int]string {
		calls++
		return map[int]string{100: "llama-server"}
	}

	r := &containerNameResolver{}
	r.nameFor(100, map[int]int{})
	r.nameFor(100, map[int]int{})
	if calls != 1 {
		t.Fatalf("dockerContainerRoots called %d times within TTL, want 1", calls)
	}

	r.mu.Lock()
	r.fetched = time.Now().Add(-time.Hour)
	r.mu.Unlock()

	r.nameFor(100, map[int]int{})
	if calls != 2 {
		t.Fatalf("dockerContainerRoots called %d times after TTL expiry, want 2", calls)
	}
}

func TestServer_ContainerNameStopsAtMissingParent(t *testing.T) {
	oldParent := processParentPID
	oldRoots := dockerContainerRoots
	defer func() {
		processParentPID = oldParent
		dockerContainerRoots = oldRoots
	}()

	// PID 400's parent lookup fails, so the walk stops without a match.
	processParentPID = func(pid int) (int, error) {
		if pid == 400 {
			return 0, errors.New("process not found")
		}
		return 0, nil
	}
	dockerContainerRoots = func() map[int]string {
		return map[int]string{100: "llama-server"}
	}

	r := &containerNameResolver{}
	if got := r.nameFor(400, map[int]int{}); got != "" {
		t.Fatalf("nameFor(400) = %q, want empty when the parent chain is unresolvable", got)
	}
}

func TestServer_ParseDockerContainerRoots(t *testing.T) {
	output := "1234 /llama-server\n5678 other\n0 /stopped\nnot-a-pid name\n\n"
	got := parseDockerContainerRoots(output)
	want := map[int]string{1234: "llama-server", 5678: "other"}

	if len(got) != len(want) {
		t.Fatalf("parseDockerContainerRoots() = %v, want %v", got, want)
	}
	for pid, name := range want {
		if got[pid] != name {
			t.Fatalf("parseDockerContainerRoots()[%d] = %q, want %q", pid, got[pid], name)
		}
	}
}

func TestServer_AnnotateContainerNamesDoesNotMutateInput(t *testing.T) {
	oldParent := processParentPID
	oldRoots := dockerContainerRoots
	defer func() {
		processParentPID = oldParent
		dockerContainerRoots = oldRoots
	}()

	processParentPID = func(pid int) (int, error) { return 0, nil }
	dockerContainerRoots = func() map[int]string {
		return map[int]string{100: "llama-server"}
	}

	s := &Server{containerNames: &containerNameResolver{}}
	input := []perf.GpuProcStat{{PID: 100, ProcessName: "server"}}
	out := s.annotateContainerNames(input)

	if input[0].ContainerName != "" {
		t.Fatalf("input was mutated: %q", input[0].ContainerName)
	}
	if len(out) != 1 || out[0].ContainerName != "llama-server" {
		t.Fatalf("annotateContainerNames() = %+v, want container name set on the copy", out)
	}
}

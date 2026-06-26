package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/perf"
)

func TestServer_GPUProcessVramMatchesRootAndDescendants(t *testing.T) {
	oldParent := processParentPID
	defer func() { processParentPID = oldParent }()

	parents := map[int]int{
		200: 100,
		300: 200,
		400: 1,
	}
	processParentPID = func(pid int) (int, error) {
		return parents[pid], nil
	}

	got := gpuProcessVramMB([]int{100}, []perf.GpuProcStat{
		{PID: 100, MemUsedMB: 128},
		{PID: 200, MemUsedMB: 256},
		{PID: 300, MemUsedMB: 512},
		{PID: 400, MemUsedMB: 1024},
	})

	if got != 896 {
		t.Fatalf("gpuProcessVramMB() = %d, want 896", got)
	}
}

func TestServer_DockerContainerRefs(t *testing.T) {
	dir := t.TempDir()
	cidPath := filepath.Join(dir, "container.id")
	if err := os.WriteFile(cidPath, []byte("abc123\n"), 0o644); err != nil {
		t.Fatalf("write cidfile: %v", err)
	}

	refs := dockerContainerRefs(config.ModelConfig{Cmd: "docker run --rm --name llama-model --cidfile " + cidPath + " image"})
	want := map[string]bool{"llama-model": true, "abc123": true}
	if len(refs) != len(want) {
		t.Fatalf("refs = %v, want two refs", refs)
	}
	for _, ref := range refs {
		if !want[ref] {
			t.Fatalf("unexpected ref %q in %v", ref, refs)
		}
	}
}

func TestServer_ModelProcessVramIncludesNamedDockerContainer(t *testing.T) {
	oldParent := processParentPID
	oldInspect := dockerInspectContainerPID
	defer func() {
		processParentPID = oldParent
		dockerInspectContainerPID = oldInspect
	}()

	processParentPID = func(pid int) (int, error) {
		parents := map[int]int{
			900: 800,
		}
		return parents[pid], nil
	}
	dockerInspectContainerPID = func(ref string) (int, error) {
		if ref != "llama-model" {
			t.Fatalf("inspect ref = %q, want llama-model", ref)
		}
		return 800, nil
	}

	got := modelProcessVramMB(
		config.ModelConfig{Cmd: "docker run --rm --name llama-model image"},
		700,
		[]perf.GpuProcStat{
			{PID: 900, MemUsedMB: 4096},
			{PID: 901, MemUsedMB: 512},
		},
	)

	if got != 4096 {
		t.Fatalf("modelProcessVramMB() = %d, want 4096", got)
	}
}

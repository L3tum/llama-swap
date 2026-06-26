package server

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/perf"
	psprocess "github.com/shirou/gopsutil/v4/process"
)

var (
	processParentPID          = defaultProcessParentPID
	dockerInspectContainerPID = defaultDockerInspectContainerPID
)

func modelProcessVramMB(mc config.ModelConfig, procPID int, procStats []perf.GpuProcStat) int {
	if procPID <= 0 || len(procStats) == 0 {
		return 0
	}

	roots := []int{procPID}
	roots = append(roots, dockerContainerRootPIDs(mc)...)
	return gpuProcessVramMB(roots, procStats)
}

func gpuProcessVramMB(rootPIDs []int, procStats []perf.GpuProcStat) int {
	if len(rootPIDs) == 0 || len(procStats) == 0 {
		return 0
	}

	roots := make(map[int]struct{}, len(rootPIDs))
	for _, pid := range rootPIDs {
		if pid > 0 {
			roots[pid] = struct{}{}
		}
	}
	if len(roots) == 0 {
		return 0
	}

	parentCache := make(map[int]int)
	var total int
	for _, p := range procStats {
		if pidInTree(p.PID, roots, parentCache) {
			total += p.MemUsedMB
		}
	}
	return total
}

func pidInTree(pid int, roots map[int]struct{}, parentCache map[int]int) bool {
	seen := make(map[int]struct{})
	for pid > 0 {
		if _, ok := roots[pid]; ok {
			return true
		}
		if _, ok := seen[pid]; ok {
			return false
		}
		seen[pid] = struct{}{}

		ppid, ok := parentCache[pid]
		if !ok {
			var err error
			ppid, err = processParentPID(pid)
			if err != nil {
				return false
			}
			parentCache[pid] = ppid
		}
		if ppid <= 0 || ppid == pid {
			return false
		}
		pid = ppid
	}
	return false
}

func defaultProcessParentPID(pid int) (int, error) {
	p, err := psprocess.NewProcess(int32(pid))
	if err != nil {
		return 0, err
	}
	ppid, err := p.Ppid()
	return int(ppid), err
}

func dockerContainerRootPIDs(mc config.ModelConfig) []int {
	refs := dockerContainerRefs(mc)
	if len(refs) == 0 {
		return nil
	}

	pids := make([]int, 0, len(refs))
	seen := make(map[int]struct{}, len(refs))
	for _, ref := range refs {
		pid, err := dockerInspectContainerPID(ref)
		if err != nil || pid <= 0 {
			continue
		}
		if _, ok := seen[pid]; ok {
			continue
		}
		seen[pid] = struct{}{}
		pids = append(pids, pid)
	}
	return pids
}

func dockerContainerRefs(mc config.ModelConfig) []string {
	args, err := mc.SanitizedCommand()
	if err != nil || len(args) == 0 || filepath.Base(args[0]) != "docker" {
		return nil
	}

	runIdx := -1
	for i := 1; i < len(args); i++ {
		if args[i] == "run" {
			runIdx = i
			break
		}
	}
	if runIdx == -1 {
		return nil
	}

	refs := make([]string, 0, 2)
	for i := runIdx + 1; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--name" && i+1 < len(args):
			refs = append(refs, args[i+1])
			i++
		case strings.HasPrefix(arg, "--name="):
			refs = append(refs, strings.TrimPrefix(arg, "--name="))
		case arg == "--cidfile" && i+1 < len(args):
			refs = append(refs, readDockerCIDFile(args[i+1]))
			i++
		case strings.HasPrefix(arg, "--cidfile="):
			refs = append(refs, readDockerCIDFile(strings.TrimPrefix(arg, "--cidfile=")))
		}
	}

	out := refs[:0]
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref != "" {
			out = append(out, ref)
		}
	}
	return out
}

func readDockerCIDFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func defaultDockerInspectContainerPID(ref string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.State.Pid}}", ref).Output()
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, err
	}
	return pid, nil
}

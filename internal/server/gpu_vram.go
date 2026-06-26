package server

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/perf"
	psprocess "github.com/shirou/gopsutil/v4/process"
)

var (
	processParentPID          = defaultProcessParentPID
	dockerInspectContainerPID = defaultDockerInspectContainerPID
)

type dockerPIDCacheKey struct {
	modelID string
	procPID int
}

type dockerPIDCache struct {
	mu      sync.Mutex
	entries map[dockerPIDCacheKey][]int
}

func (c *dockerPIDCache) rootPIDs(modelID string, procPID int, mc config.ModelConfig) []int {
	refs := dockerContainerRefs(mc)
	if len(refs) == 0 {
		return nil
	}

	key := dockerPIDCacheKey{modelID: modelID, procPID: procPID}
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.entries == nil {
		c.entries = make(map[dockerPIDCacheKey][]int)
	}
	for existing := range c.entries {
		if existing.modelID == modelID && existing.procPID != procPID {
			delete(c.entries, existing)
		}
	}
	if pids, ok := c.entries[key]; ok {
		return pids
	}

	pids := inspectDockerRootPIDs(refs)
	if len(pids) > 0 {
		c.entries[key] = pids
	}
	return pids
}

func (s *Server) modelProcessVramMB(modelID string, mc config.ModelConfig, procPID int, procStats []perf.GpuProcStat) int {
	if procPID <= 0 || len(procStats) == 0 {
		return 0
	}

	roots := []int{procPID}
	roots = append(roots, s.vramCache.rootPIDs(modelID, procPID, mc)...)
	return gpuProcessVramMB(roots, procStats)
}

func modelProcessVramMB(mc config.ModelConfig, procPID int, procStats []perf.GpuProcStat) int {
	if procPID <= 0 || len(procStats) == 0 {
		return 0
	}

	roots := []int{procPID}
	roots = append(roots, inspectDockerRootPIDs(dockerContainerRefs(mc))...)
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

func inspectDockerRootPIDs(refs []string) []int {
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
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			break
		}

		opt, _, hasValue := strings.Cut(arg, "=")
		switch {
		case opt == "--name" && hasValue:
			refs = append(refs, strings.TrimPrefix(arg, "--name="))
		case opt == "--cidfile" && hasValue:
			refs = append(refs, readDockerCIDFile(strings.TrimPrefix(arg, "--cidfile=")))
		case arg == "--name" && i+1 < len(args):
			refs = append(refs, args[i+1])
			i++
		case arg == "--cidfile" && i+1 < len(args):
			refs = append(refs, readDockerCIDFile(args[i+1]))
			i++
		case dockerRunOptionTakesValue(opt) && !hasValue && i+1 < len(args):
			i++
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

func dockerRunOptionTakesValue(opt string) bool {
	switch opt {
	case "-a", "--attach",
		"--add-host",
		"--annotation",
		"--blkio-weight",
		"--blkio-weight-device",
		"--cap-add", "--cap-drop",
		"--cgroup-parent",
		"--cpu-period", "--cpu-quota", "--cpu-rt-period", "--cpu-rt-runtime", "--cpu-shares", "-c", "--cpus", "--cpuset-cpus", "--cpuset-mems",
		"--device", "--device-cgroup-rule", "--device-read-bps", "--device-read-iops", "--device-write-bps", "--device-write-iops",
		"--dns", "--dns-option", "--dns-search",
		"--domainname",
		"--entrypoint",
		"-e", "--env", "--env-file",
		"--expose",
		"--gpus",
		"--group-add",
		"-h", "--hostname",
		"--health-cmd", "--health-interval", "--health-retries", "--health-start-interval", "--health-start-period", "--health-timeout",
		"--ip", "--ip6", "--ipc",
		"--isolation",
		"--kernel-memory",
		"-l", "--label", "--label-file",
		"--link", "--link-local-ip", "--log-driver", "--log-opt",
		"--mac-address",
		"-m", "--memory", "--memory-reservation", "--memory-swap", "--memory-swappiness",
		"--mount",
		"--name",
		"--network", "--network-alias",
		"--oom-score-adj",
		"--pid",
		"--pids-limit",
		"--platform",
		"-p", "--publish",
		"--pull",
		"--restart",
		"--runtime",
		"--security-opt", "--shm-size", "--stop-signal", "--stop-timeout", "--storage-opt", "--sysctl",
		"--tmpfs",
		"--ulimit", "-u", "--user", "--userns", "--uts",
		"-v", "--volume", "--volume-driver", "--volumes-from",
		"-w", "--workdir":
		return true
	default:
		return false
	}
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

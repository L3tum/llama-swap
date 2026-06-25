package perf

import (
	"bufio"
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// CollectGpuProcs runs nvidia-smi --query-compute-apps and returns per-process
// VRAM stats aggregated by PID. Returns nil if nvidia-smi is unavailable.
func CollectGpuProcs() []GpuProcStat {
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		return nil
	}

	cmd := exec.Command("nvidia-smi",
		"--query-compute-apps=pid,used_memory,name",
		"--format=csv,noheader,nounits",
	)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	// Aggregate by PID: sum used_memory across GPUs
	byPID := make(map[int]*GpuProcStat)
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 3 {
			continue
		}

		pid, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
		memMB, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
		name := strings.TrimSpace(strings.Join(parts[2:], ","))

		if existing, ok := byPID[pid]; ok {
			existing.MemUsedMB += memMB
		} else {
			byPID[pid] = &GpuProcStat{
				Timestamp:   time.Now(),
				PID:         pid,
				MemUsedMB:   memMB,
				ProcessName: name,
			}
		}
	}

	result := make([]GpuProcStat, 0, len(byPID))
	for _, p := range byPID {
		result = append(result, *p)
	}
	return result
}

// startProcPolling starts a goroutine that periodically collects GPU process
// stats and pushes them to the procRing. The interval is captured at call time
// so it remains stable even if UpdateConfig replaces the config.
func (m *Monitor) startProcPolling(ctx context.Context, every time.Duration) {
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				procs := CollectGpuProcs()
				if len(procs) == 0 {
					continue
				}
				m.mutex.Lock()
				m.procRing.Push(procs)
				m.mutex.Unlock()
			}
		}
	}()
}


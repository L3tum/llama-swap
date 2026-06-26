package perf

import (
	"bufio"
	"context"
	"encoding/csv"
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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "nvidia-smi",
		"--query-compute-apps=pid,used_memory,process_name",
		"--format=csv,noheader,nounits",
	)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	return parseNvidiaSmiComputeApps(string(out))
}

// parseNvidiaSmiComputeApps parses nvidia-smi --query-compute-apps output.
// Format: pid, used_memory(MiB), process_name. Memory is aggregated by PID
// across GPUs. The process name is parsed as CSV because paths can contain
// commas.
func parseNvidiaSmiComputeApps(output string) []GpuProcStat {
	type procAccum struct {
		memUsedMB   int
		processName string
	}

	byPID := make(map[int]*procAccum)
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		reader := csv.NewReader(strings.NewReader(line))
		reader.TrimLeadingSpace = true
		fields, err := reader.Read()
		if err != nil || len(fields) < 3 {
			continue
		}

		pid, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil || pid <= 0 {
			continue
		}
		memMB, err := strconv.Atoi(strings.TrimSpace(fields[1]))
		if err != nil {
			continue
		}
		name := strings.TrimSpace(strings.Join(fields[2:], ","))

		if existing, ok := byPID[pid]; ok {
			existing.memUsedMB += memMB
			if existing.processName == "" {
				existing.processName = name
			}
		} else {
			byPID[pid] = &procAccum{memUsedMB: memMB, processName: name}
		}
	}

	now := time.Now()
	result := make([]GpuProcStat, 0, len(byPID))
	for pid, p := range byPID {
		result = append(result, GpuProcStat{
			Timestamp:   now,
			PID:         pid,
			MemUsedMB:   p.memUsedMB,
			ProcessName: p.processName,
		})
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
				m.RecordProcesses(CollectGpuProcs())
			}
		}
	}()
}

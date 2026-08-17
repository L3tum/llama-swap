package perf

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseNvidiaSmiComputeApps_AggregatesByPID(t *testing.T) {
	output := "123, 1024, /usr/bin/llama-server\n123, 512, /usr/bin/llama-server\n456, 256, python\n"

	result := parseNvidiaSmiComputeApps(output)

	require.Len(t, result, 2)
	byPID := map[int]GpuProcStat{}
	for _, p := range result {
		byPID[p.PID] = p
	}
	assert.Equal(t, 1536, byPID[123].MemUsedMB)
	assert.Equal(t, "/usr/bin/llama-server", byPID[123].ProcessName)
	assert.Equal(t, 256, byPID[456].MemUsedMB)
	assert.Equal(t, "python", byPID[456].ProcessName)
}

func TestParseNvidiaSmiComputeApps_ProcessNameWithComma(t *testing.T) {
	output := "123, 1024, \"/path/with,comma/llama-server\"\n"

	result := parseNvidiaSmiComputeApps(output)

	require.Len(t, result, 1)
	assert.Equal(t, 123, result[0].PID)
	assert.Equal(t, 1024, result[0].MemUsedMB)
	assert.Equal(t, "/path/with,comma/llama-server", result[0].ProcessName)
}

func TestParseNvidiaSmiComputeApps_SkipsInvalidRows(t *testing.T) {
	output := "bad, 1024, proc\n123, bad, proc\n0, 1, proc\n456, 256, ok\n"

	result := parseNvidiaSmiComputeApps(output)

	require.Len(t, result, 1)
	assert.Equal(t, 456, result[0].PID)
	assert.Equal(t, 256, result[0].MemUsedMB)
}

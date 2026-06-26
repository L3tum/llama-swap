package perf

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/ring"
)

var (
	ErrNotImplemented = errors.New("not implemented")
	ErrNoGpuTool      = errors.New("no GPU monitoring tool available")
)

type Monitor struct {
	mutex    sync.RWMutex
	log      *logmon.Monitor
	conf     config.PerformanceConfig
	sysRing  ring.Buffer[SysStat]
	gpuRing  ring.Buffer[[]GpuStat]
	procRing ring.Buffer[[]GpuProcStat]

	stopCtx    context.Context
	stopCancel context.CancelFunc

	sysListeners  map[chan SysStat]struct{}
	gpuListeners  map[chan []GpuStat]struct{}
	procListeners map[chan []GpuProcStat]struct{}
}

func ringCapacity(c config.PerformanceConfig) int {
	n := int(time.Hour / c.Every)
	if n < 1 {
		n = 1
	}
	return n
}

func New(c config.PerformanceConfig, logger *logmon.Monitor) (*Monitor, error) {

	if c.Every < 100*time.Millisecond {
		c.Every = 100 * time.Millisecond
	}

	if logger == nil {
		return nil, errors.New("logger is required")
	}

	capacity := ringCapacity(c)
	return &Monitor{
		conf:          c,
		log:           logger,
		sysRing:       ring.NewBuffer[SysStat](capacity),
		gpuRing:       ring.NewBuffer[[]GpuStat](capacity),
		procRing:      ring.NewBuffer[[]GpuProcStat](capacity),
		sysListeners:  make(map[chan SysStat]struct{}),
		gpuListeners:  make(map[chan []GpuStat]struct{}),
		procListeners: make(map[chan []GpuProcStat]struct{}),
	}, nil
}

func (m *Monitor) Stop() {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	if m.stopCancel == nil {
		return
	}
	m.stopCancel()
	m.stopCancel = nil
}

// UpdateConfig updates the monitor configuration and restarts if changed.
func (m *Monitor) UpdateConfig(newConf config.PerformanceConfig) {
	m.mutex.RLock()
	changed := m.conf != newConf
	m.mutex.RUnlock()

	if !changed {
		return
	}

	m.Stop()
	m.mutex.Lock()
	m.conf = newConf
	capacity := ringCapacity(newConf)
	m.sysRing = ring.NewBuffer[SysStat](capacity)
	m.gpuRing = ring.NewBuffer[[]GpuStat](capacity)
	m.procRing = ring.NewBuffer[[]GpuProcStat](capacity)
	m.mutex.Unlock()
	if !newConf.Disabled {
		m.Start()
	}
}

// Subscribe returns channels to listen to system and GPU stats.
func (m *Monitor) Subscribe() (chan SysStat, chan []GpuStat, func()) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	sysChan := make(chan SysStat, 1)
	gpuChan := make(chan []GpuStat, 1)

	m.sysListeners[sysChan] = struct{}{}
	m.gpuListeners[gpuChan] = struct{}{}

	unsub := func() {
		m.mutex.Lock()
		defer m.mutex.Unlock()
		delete(m.sysListeners, sysChan)
		delete(m.gpuListeners, gpuChan)
	}

	return sysChan, gpuChan, unsub
}

// SubscribeProcesses returns a channel receiving per-process GPU snapshots.
func (m *Monitor) SubscribeProcesses() (chan []GpuProcStat, func()) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	procChan := make(chan []GpuProcStat, 1)
	m.procListeners[procChan] = struct{}{}

	unsub := func() {
		m.mutex.Lock()
		defer m.mutex.Unlock()
		delete(m.procListeners, procChan)
	}

	return procChan, unsub
}

func (m *Monitor) Start() {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	if m.stopCancel != nil {
		return
	}

	m.stopCtx, m.stopCancel = context.WithCancel(context.Background())

	go func() {
		tick := time.NewTicker(m.conf.Every)
		defer tick.Stop()
		for {
			select {
			case <-m.stopCtx.Done():
				return
			case <-tick.C:
				s, err := ReadSysStats()
				if err != nil {
					if err != ErrNotImplemented {
						m.log.Errorf("failed to read sys stats: %s", err.Error())
					}
					continue
				}
				m.mutex.Lock()
				m.sysRing.Push(s)
				for l := range m.sysListeners {
					select {
					case l <- s:
					default:
					}
				}
				m.mutex.Unlock()
			}
		}
	}()

	go func() {
		gpuCh, err := getGpuStats(m.stopCtx, m.conf.Every, m.log)
		if err != nil {
			if errors.Is(err, ErrNotImplemented) || errors.Is(err, ErrNoGpuTool) {
				m.log.Infof("GPU monitoring not available: %s", err.Error())
			} else {
				m.log.Errorf("failed to initialize GPU monitoring: %s", err.Error())
			}
			return
		}

		for {
			select {
			case <-m.stopCtx.Done():
				return
			case g, ok := <-gpuCh:
				if !ok {
					m.log.Errorf("failed reading from gpuCh - stopping read goroutine")
					return
				}
				m.mutex.Lock()
				m.gpuRing.Push(g)
				for l := range m.gpuListeners {
					select {
					case l <- g:
					default:
					}
				}
				m.mutex.Unlock()
			}
		}
	}()

	// Start per-process GPU VRAM polling.
	m.startProcPolling(m.stopCtx, m.conf.Every)
}

// Current returns a copy of the full history of system and GPU stats.
// Use Latest() for the most recent snapshot only (O(1) vs O(N)).
func (m *Monitor) Current() ([]SysStat, []GpuStat) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	sysStats := m.sysRing.Slice()

	snapshots := m.gpuRing.Slice()
	var gpuStats []GpuStat
	for _, snapshot := range snapshots {
		gpuStats = append(gpuStats, snapshot...)
	}
	return sysStats, gpuStats
}

// Latest returns the most recent system and GPU snapshot.
// Returns nil slices if no data has been collected yet.
// This is O(1) compared to Current() which is O(N) over the full ring buffer.
func (m *Monitor) Latest() (SysStat, []GpuStat) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	var sysStat SysStat
	if latest, ok := m.sysRing.Latest(); ok {
		sysStat = latest
	}

	var gpuStats []GpuStat
	if latest, ok := m.gpuRing.Latest(); ok {
		gpuStats = latest
	}
	return sysStat, gpuStats
}

// CurrentProcesses returns a copy of the full history of per-process GPU stats.
// Use LatestProcesses() for the most recent snapshot only (O(1) vs O(N)).
func (m *Monitor) CurrentProcesses() []GpuProcStat {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	snapshots := m.procRing.Slice()
	var procs []GpuProcStat
	for _, snapshot := range snapshots {
		procs = append(procs, snapshot...)
	}
	return procs
}

// LatestProcesses returns the most recent per-process GPU stats snapshot.
// Returns nil if no data has been collected yet.
// This is O(1) compared to CurrentProcesses() which is O(N) over the full ring buffer.
func (m *Monitor) LatestProcesses() []GpuProcStat {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	latest, ok := m.procRing.Latest()
	if !ok {
		return nil
	}
	return latest
}

func ReadSysStats() (SysStat, error) {
	return readSysStats()
}

func GetGpuStats(ctx context.Context, every time.Duration, logger *logmon.Monitor) (chan []GpuStat, error) {
	return getGpuStats(ctx, every, logger)
}

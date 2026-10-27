package service

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/brogergvhs/kaodoku/internal/browserdownload"
	"github.com/brogergvhs/kaodoku/internal/kotatsu"
)

const (
	memoryEvery  = 5 * time.Second
	memoryBucket = 5 * time.Minute
	memoryKeep   = 12 * time.Hour
)

var memoryServices = []string{"Kaodoku", "Kotatsu worker", "Browser worker"}

// MemoryStats is each service's memory over the last 12 hours in 5-minute buckets holding the peak reading; 0 means no reading.
type MemoryStats struct {
	Times      []time.Time
	Series     []MemorySeries
	Unmeasured string
}

type MemorySeries struct {
	Name          string
	Current, Peak int64
	Points        []int64
}

func (m MemoryStats) SpanSeconds() int64 {
	if len(m.Times) < 2 {
		return 0
	}
	return int64(time.Since(m.Times[0]).Seconds())
}

type memoryReading struct {
	at    time.Time
	bytes map[string]int64
}

type memoryHistory struct {
	mu      sync.Mutex
	latest  map[string]int64
	buckets []memoryReading
}

func (h *memoryHistory) add(now time.Time, sample map[string]int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.latest = sample
	if n := len(h.buckets); n == 0 || !h.buckets[n-1].at.Truncate(memoryBucket).Equal(now.Truncate(memoryBucket)) {
		h.buckets = append(h.buckets, memoryReading{at: now, bytes: map[string]int64{}})
	}
	last := h.buckets[len(h.buckets)-1].bytes
	for name, n := range sample {
		last[name] = max(last[name], n)
	}
	cut := 0
	for cut < len(h.buckets) && h.buckets[cut].at.Before(now.Add(-memoryKeep)) {
		cut++
	}
	h.buckets = h.buckets[cut:]
}

func (h *memoryHistory) stats() MemoryStats {
	h.mu.Lock()
	defer h.mu.Unlock()
	var m MemoryStats
	for _, b := range h.buckets {
		m.Times = append(m.Times, b.at)
	}
	for _, name := range memoryServices {
		s := MemorySeries{Name: name, Current: h.latest[name], Points: make([]int64, len(h.buckets))}
		for i, b := range h.buckets {
			s.Points[i] = b.bytes[name]
			s.Peak = max(s.Peak, s.Points[i])
		}
		if s.Peak > 0 || s.Current > 0 {
			m.Series = append(m.Series, s)
		}
	}
	return m
}

// WatchMemory samples each service's memory every few seconds.
func (s *JobService) WatchMemory(ctx context.Context) {
	for {
		s.memory.add(time.Now(), s.sampleMemory(ctx))
		select {
		case <-ctx.Done():
			return
		case <-time.After(memoryEvery):
		}
	}
}

func (s *JobService) sampleMemory(ctx context.Context) map[string]int64 {
	out := map[string]int64{"Kaodoku": selfMemory()}
	cfg, _, err := s.RuntimeConfig(ctx)
	if err != nil || cfg == nil {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if cfg.KotatsuWorker.Enabled {
		if h, err := kotatsu.New(cfg.KotatsuWorker.Endpoint, cfg.KotatsuWorker.APIKey).Health(ctx); err == nil && h.MemoryBytes > 0 {
			out["Kotatsu worker"] = h.MemoryBytes
		}
	}
	if cfg.BrowserDownload.Enabled {
		if n, err := browserdownload.New(cfg.BrowserDownload.Endpoint, 8*time.Second, nil).Health(ctx); err == nil && n > 0 {
			out["Browser worker"] = n
		}
	}
	return out
}

func (s *JobService) MemoryStats(ctx context.Context) MemoryStats {
	m := s.memory.stats()
	if cfg, _, err := s.RuntimeConfig(ctx); err == nil && cfg != nil {
		var off []string
		if cfg.BrowserSolver.Enabled {
			off = append(off, "FlareSolverr")
		}
		if cfg.BrowserDownload.Enabled {
			off = append(off, "Selenium")
		}
		m.Unmeasured = strings.Join(off, " and ")
	}
	return m
}

func selfMemory() int64 {
	if n, ok := cgroupMemory("/sys/fs/cgroup"); ok {
		return n
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.Sys)
}

// cgroupMemory reads a cgroup v2 working set the way docker stats does: usage minus inactive page cache.
func cgroupMemory(dir string) (int64, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "memory.current"))
	if err != nil {
		return 0, false
	}
	used, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, false
	}
	stat, _ := os.ReadFile(filepath.Join(dir, "memory.stat"))
	for line := range strings.SplitSeq(string(stat), "\n") {
		if v, ok := strings.CutPrefix(line, "inactive_file "); ok {
			if cache, err := strconv.ParseInt(v, 10, 64); err == nil {
				used -= min(cache, used)
			}
		}
	}
	return used, true
}

package service

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMemoryHistoryKeepsBucketPeaksForTwelveHours(t *testing.T) {
	t.Parallel()
	var h memoryHistory
	t0 := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	h.add(t0, map[string]int64{"Kaodoku": 100})
	h.add(t0.Add(time.Minute), map[string]int64{"Kaodoku": 150, "Kotatsu worker": 50})
	h.add(t0.Add(6*time.Minute), map[string]int64{"Kaodoku": 120})

	m := h.stats()
	if got := fmt.Sprint(len(m.Times), m.Series); got != "2 [{Kaodoku 120 150 [150 120]} {Kotatsu worker 0 50 [50 0]}]" {
		t.Fatalf("stats = %s", got)
	}

	h.add(t0.Add(12*time.Hour+5*time.Minute), map[string]int64{"Kaodoku": 90})
	if m := h.stats(); len(m.Times) != 2 || !m.Times[0].Equal(t0.Add(6*time.Minute)) {
		t.Fatalf("after 12h kept %v", m.Times)
	}
}

func TestCgroupMemorySubtractsInactiveCache(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "memory.current"), []byte("1000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.stat"), []byte("anon 600\ninactive_file 300\nactive_file 100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, ok := cgroupMemory(dir); !ok || n != 700 {
		t.Fatalf("cgroupMemory = %d, %v", n, ok)
	}
	if _, ok := cgroupMemory(filepath.Join(dir, "missing")); ok {
		t.Fatal("missing cgroup reported memory")
	}
}

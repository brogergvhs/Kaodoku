package server

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/brogergvhs/kaodoku/internal/service"
)

func TestMemoryLineBreaksAtMissingReadings(t *testing.T) {
	t.Parallel()
	t0 := time.Now().Add(-20 * time.Minute)
	m := service.MemoryStats{Times: []time.Time{t0, t0.Add(5 * time.Minute), t0.Add(10 * time.Minute), t0.Add(15 * time.Minute)}}
	svg := string(memoryLine(m, service.MemorySeries{Name: "Kaodoku", Peak: 30, Points: []int64{10, 0, 20, 30}}))
	line := regexp.MustCompile(`<path d="([^"]*)" fill="none"`).FindStringSubmatch(svg)
	if line == nil || strings.Count(line[1], "M") != 2 {
		t.Fatalf("want two line segments around the gap:\n%s", svg)
	}
	if strings.Count(svg, "<rect") != 4 || !strings.Contains(svg, "no reading") {
		t.Fatalf("want one hover target per bucket:\n%s", svg)
	}
}

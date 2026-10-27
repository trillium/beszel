package agent

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/henrygd/beszel/internal/entities/system"
	"github.com/shirou/gopsutil/v4/process"
)

const (
	// topProcessesCount is how many processes are returned per ranking.
	topProcessesCount = 5
	// topProcessesMaxScan caps how many processes are read per sample.
	topProcessesMaxScan = 2000
	// topProcessesWindow is the gap between the two CPU time reads.
	topProcessesWindow = 500 * time.Millisecond
	// topProcessesBudget bounds the whole sample, including the window.
	topProcessesBudget = 3 * time.Second
	// topProcessesCacheTTL lets alerts that fire together share one sample.
	topProcessesCacheTTL = 10 * time.Second
)

// procReading is one read of a process. cpu is total CPU time in seconds.
// name may be empty if the reader does not provide it cheaply.
type procReading struct {
	cpu  float64
	rss  uint64
	name string
}

// procReader reads up to maxScan processes and reports how many exist.
type procReader func(ctx context.Context, maxScan int) (readings map[int32]procReading, total int, err error)

// topProcessesSampler takes on-demand samples of the heaviest processes.
// Nothing runs between requests; the zero value is ready to use.
type topProcessesSampler struct {
	mu       sync.Mutex
	last     system.TopProcesses
	lastTime time.Time
}

// get returns a recent sample if one exists, otherwise takes a new one.
func (s *topProcessesSampler) get() system.TopProcesses {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.lastTime.IsZero() && time.Since(s.lastTime) < topProcessesCacheTTL {
		return s.last
	}
	ctx, cancel := context.WithTimeout(context.Background(), topProcessesBudget)
	defer cancel()
	s.last = sampleTopProcesses(ctx, defaultProcReader, topProcessesWindow, topProcessesMaxScan, topProcessesCount)
	s.lastTime = time.Now()
	return s.last
}

// sampleTopProcesses reads every process twice, window apart, and returns the
// top n by CPU and by RSS. CPU percent is the CPU time used during the window,
// not a lifetime average. Processes that start during the window are not counted.
func sampleTopProcesses(ctx context.Context, read procReader, window time.Duration, maxScan, n int) system.TopProcesses {
	result := system.TopProcesses{SampleMs: uint32(window.Milliseconds())}

	start := time.Now()
	first, _, err := read(ctx, maxScan)
	if err != nil {
		return result
	}

	select {
	case <-ctx.Done():
		return result
	case <-time.After(window):
	}

	elapsed := time.Since(start)
	second, total, err := read(ctx, maxScan)
	if err != nil {
		return result
	}

	result = buildTopProcesses(first, second, elapsed, n)
	result.SampleMs = uint32(elapsed.Milliseconds())
	result.Total = uint32(total)
	return result
}

// buildTopProcesses ranks processes present in both reads. Names missing from
// the reads are looked up only for the processes that are reported.
func buildTopProcesses(first, second map[int32]procReading, elapsed time.Duration, n int) system.TopProcesses {
	samples := make([]system.ProcessUsage, 0, len(second))
	for pid, cur := range second {
		prev, ok := first[pid]
		if !ok {
			continue
		}
		usage := system.ProcessUsage{Pid: pid, Name: cur.name, Rss: cur.rss}
		if delta := cur.cpu - prev.cpu; delta > 0 && elapsed > 0 {
			usage.Cpu = delta / elapsed.Seconds() * 100
		}
		samples = append(samples, usage)
	}

	byCpu, byMem := rankTopProcesses(samples, n)
	names := make(map[int32]string, len(byCpu)+len(byMem))
	fillNames(byCpu, names)
	fillNames(byMem, names)
	return system.TopProcesses{
		ByCpu:   byCpu,
		ByMem:   byMem,
		Scanned: uint32(len(samples)),
	}
}

// rankTopProcesses returns the top n samples by CPU and by RSS, highest first.
// Samples with no CPU use are left out of the CPU ranking and samples with no
// RSS are left out of the memory ranking.
func rankTopProcesses(samples []system.ProcessUsage, n int) (byCpu, byMem []system.ProcessUsage) {
	byCpu = topN(samples, n, func(p system.ProcessUsage) bool { return p.Cpu > 0 }, func(a, b system.ProcessUsage) int {
		return cmp.Or(cmp.Compare(b.Cpu, a.Cpu), cmp.Compare(b.Rss, a.Rss), cmp.Compare(a.Pid, b.Pid))
	})
	byMem = topN(samples, n, func(p system.ProcessUsage) bool { return p.Rss > 0 }, func(a, b system.ProcessUsage) int {
		return cmp.Or(cmp.Compare(b.Rss, a.Rss), cmp.Compare(b.Cpu, a.Cpu), cmp.Compare(a.Pid, b.Pid))
	})
	return byCpu, byMem
}

func topN(samples []system.ProcessUsage, n int, keep func(system.ProcessUsage) bool, compare func(a, b system.ProcessUsage) int) []system.ProcessUsage {
	kept := make([]system.ProcessUsage, 0, len(samples))
	for _, s := range samples {
		if keep(s) {
			kept = append(kept, s)
		}
	}
	slices.SortFunc(kept, compare)
	if len(kept) > n {
		kept = kept[:n]
	}
	return kept
}

func fillNames(usage []system.ProcessUsage, names map[int32]string) {
	for i := range usage {
		if usage[i].Name != "" {
			continue
		}
		name, ok := names[usage[i].Pid]
		if !ok {
			name, _ = (&process.Process{Pid: usage[i].Pid}).Name()
			names[usage[i].Pid] = name
		}
		usage[i].Name = name
	}
}

// readProcessesGopsutil reads CPU time and RSS through gopsutil (/proc on Linux).
// Names are left empty and filled in later for reported processes only.
func readProcessesGopsutil(ctx context.Context, maxScan int) (map[int32]procReading, int, error) {
	pids, err := process.PidsWithContext(ctx)
	if err != nil {
		return nil, 0, err
	}
	total := len(pids)
	if len(pids) > maxScan {
		pids = pids[:maxScan]
	}
	readings := make(map[int32]procReading, len(pids))
	for _, pid := range pids {
		if ctx.Err() != nil {
			break
		}
		proc := &process.Process{Pid: pid}
		times, err := proc.TimesWithContext(ctx)
		if err != nil {
			continue
		}
		reading := procReading{cpu: times.User + times.System}
		if mem, err := proc.MemoryInfoWithContext(ctx); err == nil {
			reading.rss = mem.RSS
		}
		readings[pid] = reading
	}
	return readings, total, nil
}

// readProcessesPs reads processes with `ps`. On macOS ps is setuid root, so
// it can read processes owned by other users that gopsutil cannot.
func readProcessesPs(ctx context.Context, maxScan int) (map[int32]procReading, int, error) {
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,time=,rss=,comm=").Output()
	if err != nil {
		return nil, 0, err
	}
	return parsePsOutput(out, maxScan)
}

// parsePsOutput parses `ps -A -o pid=,time=,rss=,comm=` output. time is CPU
// time as [[dd-]hh:]mm:ss[.cc], rss is in KiB, comm is the executable path.
func parsePsOutput(out []byte, maxScan int) (map[int32]procReading, int, error) {
	readings := make(map[int32]procReading)
	total := 0
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.ParseInt(fields[0], 10, 32)
		if err != nil {
			continue
		}
		total++
		if len(readings) >= maxScan {
			continue
		}
		cpu, err := parseCPUTime(fields[1])
		if err != nil {
			continue
		}
		rssKiB, _ := strconv.ParseUint(fields[2], 10, 64)
		reading := procReading{cpu: cpu, rss: rssKiB * 1024}
		if len(fields) > 3 {
			reading.name = filepath.Base(strings.Join(fields[3:], " "))
		}
		readings[int32(pid)] = reading
	}
	if total == 0 {
		return nil, 0, errors.New("no processes in ps output")
	}
	return readings, total, scanner.Err()
}

// parseCPUTime parses a ps CPU time like "520:57.59", "1:02:03" or "2-01:02:03".
func parseCPUTime(s string) (float64, error) {
	var days float64
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.ParseFloat(d, 64)
		if err != nil {
			return 0, err
		}
		days, s = n, rest
	}
	var seconds float64
	for part := range strings.SplitSeq(s, ":") {
		n, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return 0, err
		}
		seconds = seconds*60 + n
	}
	return days*86400 + seconds, nil
}

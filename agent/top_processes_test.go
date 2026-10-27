//go:build testing

package agent

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/henrygd/beszel/internal/common"
	"github.com/henrygd/beszel/internal/entities/system"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCPUTime(t *testing.T) {
	tests := []struct {
		in   string
		want float64
	}{
		{"0:00.06", 0.06},
		{"520:57.59", 520*60 + 57.59},
		{"1:02:03", 3723},
		{"2-01:02:03", 2*86400 + 3723},
		{"00:00:05", 5},
	}
	for _, tt := range tests {
		got, err := parseCPUTime(tt.in)
		require.NoError(t, err, tt.in)
		assert.InDelta(t, tt.want, got, 1e-9, tt.in)
	}
	for _, bad := range []string{"", "abc", "1:x", "x-1:00"} {
		_, err := parseCPUTime(bad)
		assert.Error(t, err, bad)
	}
}

func TestParsePsOutput(t *testing.T) {
	out := []byte(`    1  67:10.56  23440 /sbin/launchd
  519 223:26.22  41120 /usr/libexec/syspolicyd
 4580   0:01.50 997326 /Applications/Google Chrome.app/Contents/Frameworks/Google Chrome Helper (Renderer)
  bad line here
  777   x:yy     100 /bin/broken
  900   0:00.00      0
`)
	readings, total, err := parsePsOutput(out, 100)
	require.NoError(t, err)
	assert.Equal(t, 5, total, "lines with a numeric pid are counted")
	require.Len(t, readings, 4, "the line with a bad CPU time is skipped")

	assert.Equal(t, procReading{cpu: 67*60 + 10.56, rss: 23440 * 1024, name: "launchd"}, readings[1])
	assert.Equal(t, "syspolicyd", readings[519].name)
	assert.Equal(t, "Google Chrome Helper (Renderer)", readings[4580].name, "names with spaces are kept whole")
	assert.Equal(t, uint64(997326*1024), readings[4580].rss)
	assert.Equal(t, procReading{}, readings[900], "a process without comm has no name")

	capped, total, err := parsePsOutput(out, 2)
	require.NoError(t, err)
	assert.Len(t, capped, 2, "maxScan caps processes read")
	assert.Equal(t, 5, total, "total still counts every process")

	_, _, err = parsePsOutput([]byte("  PID TIME RSS COMM\n"), 100)
	assert.Error(t, err)
}

func TestBuildTopProcesses(t *testing.T) {
	first := map[int32]procReading{
		10: {cpu: 100, rss: 1 << 20, name: "busy"},
		11: {cpu: 50, rss: 8 << 30, name: "big"},
		12: {cpu: 7, rss: 2 << 20, name: "half"},
		13: {cpu: 3, rss: 0, name: "unreadable"},
		14: {cpu: 9, rss: 3 << 20, name: "gone"},
	}
	second := map[int32]procReading{
		10: {cpu: 102, rss: 1 << 20, name: "busy"}, // 2s of CPU in 2s = 100%
		11: {cpu: 50, rss: 8 << 30, name: "big"},   // idle
		12: {cpu: 8, rss: 2 << 20, name: "half"},   // 50%
		13: {cpu: 3, rss: 0, name: "unreadable"},
		15: {cpu: 99, rss: 9 << 30, name: "new"}, // started during the window
	}

	got := buildTopProcesses(first, second, 2*time.Second, 5)

	assert.Equal(t, uint32(4), got.Scanned, "only processes in both reads are ranked")
	assert.Equal(t, []system.ProcessUsage{
		{Pid: 10, Name: "busy", Cpu: 100, Rss: 1 << 20},
		{Pid: 12, Name: "half", Cpu: 50, Rss: 2 << 20},
	}, got.ByCpu, "idle processes are left out of the CPU ranking")
	assert.Equal(t, []system.ProcessUsage{
		{Pid: 11, Name: "big", Cpu: 0, Rss: 8 << 30},
		{Pid: 12, Name: "half", Cpu: 50, Rss: 2 << 20},
		{Pid: 10, Name: "busy", Cpu: 100, Rss: 1 << 20},
	}, got.ByMem, "processes with no RSS are left out of the memory ranking")

	top1 := buildTopProcesses(first, second, 2*time.Second, 1)
	assert.Len(t, top1.ByCpu, 1)
	assert.Len(t, top1.ByMem, 1)
	assert.Equal(t, int32(10), top1.ByCpu[0].Pid)
	assert.Equal(t, int32(11), top1.ByMem[0].Pid)
}

func TestBuildTopProcessesLooksUpMissingNames(t *testing.T) {
	pid := int32(os.Getpid())
	first := map[int32]procReading{pid: {cpu: 1, rss: 1 << 20}}
	second := map[int32]procReading{pid: {cpu: 2, rss: 1 << 20}}
	got := buildTopProcesses(first, second, time.Second, 5)
	require.Len(t, got.ByCpu, 1)
	require.Len(t, got.ByMem, 1)
	assert.NotEmpty(t, got.ByCpu[0].Name)
	assert.Equal(t, got.ByCpu[0].Name, got.ByMem[0].Name)
}

func TestSampleTopProcesses(t *testing.T) {
	t.Run("reads twice and reports totals", func(t *testing.T) {
		calls := 0
		reader := func(_ context.Context, maxScan int) (map[int32]procReading, int, error) {
			calls++
			assert.Equal(t, 50, maxScan)
			return map[int32]procReading{1: {cpu: float64(calls), rss: 1 << 20, name: "p"}}, 80, nil
		}
		got := sampleTopProcesses(context.Background(), reader, 10*time.Millisecond, 50, 5)
		assert.Equal(t, 2, calls)
		assert.Equal(t, uint32(80), got.Total)
		assert.Equal(t, uint32(1), got.Scanned)
		assert.GreaterOrEqual(t, got.SampleMs, uint32(10))
		require.Len(t, got.ByCpu, 1)
		assert.Equal(t, "p", got.ByCpu[0].Name)
	})

	t.Run("returns empty sample on read error", func(t *testing.T) {
		reader := func(context.Context, int) (map[int32]procReading, int, error) {
			return nil, 0, errors.New("boom")
		}
		got := sampleTopProcesses(context.Background(), reader, time.Millisecond, 50, 5)
		assert.Empty(t, got.ByCpu)
		assert.Empty(t, got.ByMem)
	})

	t.Run("stops when the budget expires during the window", func(t *testing.T) {
		calls := 0
		reader := func(context.Context, int) (map[int32]procReading, int, error) {
			calls++
			return map[int32]procReading{1: {cpu: 1, rss: 1}}, 1, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		start := time.Now()
		got := sampleTopProcesses(ctx, reader, time.Minute, 50, 5)
		assert.Less(t, time.Since(start), 5*time.Second)
		assert.Equal(t, 1, calls)
		assert.Empty(t, got.ByMem)
	})
}

// TestSampleTopProcessesLive takes real samples with the platform reader and
// with gopsutil, which is the platform reader everywhere but macOS.
func TestSampleTopProcessesLive(t *testing.T) {
	t.Run("platform", func(t *testing.T) { testSampleTopProcessesLive(t, defaultProcReader) })
	t.Run("gopsutil", func(t *testing.T) { testSampleTopProcessesLive(t, readProcessesGopsutil) })
}

func testSampleTopProcessesLive(t *testing.T, read procReader) {
	start := time.Now()
	got := sampleTopProcesses(context.Background(), read, 100*time.Millisecond, topProcessesMaxScan, topProcessesCount)
	t.Logf("sample took %v, read %d of %d processes", time.Since(start), got.Scanned, got.Total)

	require.NotZero(t, got.Total)
	assert.LessOrEqual(t, got.Scanned, got.Total)
	require.NotEmpty(t, got.ByMem, "at least this test process has RSS")
	assert.LessOrEqual(t, len(got.ByMem), topProcessesCount)
	assert.LessOrEqual(t, len(got.ByCpu), topProcessesCount)
	for i, p := range got.ByMem {
		assert.Positive(t, p.Pid)
		assert.Positive(t, p.Rss)
		if i > 0 {
			assert.GreaterOrEqual(t, got.ByMem[i-1].Rss, p.Rss, "sorted by RSS")
		}
	}
	for i, p := range got.ByCpu {
		assert.Positive(t, p.Cpu)
		if i > 0 {
			assert.GreaterOrEqual(t, got.ByCpu[i-1].Cpu, p.Cpu, "sorted by CPU")
		}
	}
}

func TestTopProcessesSamplerCaches(t *testing.T) {
	var s topProcessesSampler
	s.last = system.TopProcesses{Total: 42}
	s.lastTime = time.Now()
	assert.Equal(t, uint32(42), s.get().Total, "a recent sample is reused")
}

func TestGetTopProcessesHandler(t *testing.T) {
	agent := &Agent{}
	agent.topProcesses.last = system.TopProcesses{
		ByCpu: []system.ProcessUsage{{Pid: 7, Name: "spin", Cpu: 99.5, Rss: 1 << 20}},
		Total: 1, Scanned: 1, SampleMs: 500,
	}
	agent.topProcesses.lastTime = time.Now()

	var sent any
	ctx := &HandlerContext{
		Agent:   agent,
		Request: &common.HubRequest[cbor.RawMessage]{Action: common.GetTopProcesses},
		SendResponse: func(data any, _ *uint32) error {
			sent = data
			return nil
		},
	}
	handler, ok := NewHandlerRegistry().GetHandler(common.GetTopProcesses)
	require.True(t, ok, "handler is registered")
	require.NoError(t, handler.Handle(ctx))

	// The response goes through the generic Data field; check it round-trips.
	response := newAgentResponse(sent, nil)
	var decoded system.TopProcesses
	require.NoError(t, cbor.Unmarshal(response.Data, &decoded))
	assert.Equal(t, agent.topProcesses.last, decoded)
}

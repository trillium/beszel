//go:build testing

package alerts_test

import (
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/henrygd/beszel/internal/alerts"
	"github.com/henrygd/beszel/internal/entities/system"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testTopProcesses = system.TopProcesses{
	ByCpu: []system.ProcessUsage{
		{Pid: 519, Name: "syspolicyd", Cpu: 67.84, Rss: 40 << 20},
		{Pid: 1200, Name: "dolt", Cpu: 12.3, Rss: 3 << 29},
	},
	ByMem: []system.ProcessUsage{
		{Pid: 1200, Name: "dolt", Cpu: 12.3, Rss: 3 << 29},
		{Pid: 519, Name: "syspolicyd", Cpu: 67.84, Rss: 40 << 20},
	},
	SampleMs: 512,
	Scanned:  600,
	Total:    600,
}

func TestFormatTopProcesses(t *testing.T) {
	assert.Equal(t, `Top processes by CPU (0.5s sample):
syspolicyd (pid 519): 67.8% CPU, 40 MB
dolt (pid 1200): 12.3% CPU, 1.5 GB

Top processes by memory:
dolt (pid 1200): 12.3% CPU, 1.5 GB
syspolicyd (pid 519): 67.8% CPU, 40 MB`, alerts.FormatTopProcesses(testTopProcesses, false))

	memFirst := alerts.FormatTopProcesses(testTopProcesses, true)
	assert.True(t, strings.HasPrefix(memFirst, "Top processes by memory:"), memFirst)

	t.Run("empty sample formats to nothing", func(t *testing.T) {
		assert.Empty(t, alerts.FormatTopProcesses(system.TopProcesses{SampleMs: 500, Total: 10}, false))
	})

	t.Run("partial scan, unknown and long names", func(t *testing.T) {
		got := alerts.FormatTopProcesses(system.TopProcesses{
			ByMem: []system.ProcessUsage{
				{Pid: 1, Rss: 1 << 20},
				{Pid: 2, Name: strings.Repeat("x", 60), Rss: 1 << 20},
			},
			Scanned: 2000,
			Total:   2500,
		}, false)
		assert.Equal(t, "Top processes by memory:\n? (pid 1): 0.0% CPU, 1 MB\n"+strings.Repeat("x", 39)+"… (pid 2): 0.0% CPU, 1 MB\n\n(2000 of 2500 processes read)", got)
	})
}

func TestSystemAlertTopProcesses(t *testing.T) {
	tests := []struct {
		name       string
		alertName  string
		setValue   systemAlertValueSetter[float64]
		fetch      alerts.FetchTopProcessesFunc
		wantFirst  string // first top processes heading in the triggered email, "" for none
		wantFetchN int
	}{
		{
			name:       "CPU alert includes sample",
			alertName:  "CPU",
			setValue:   setCPUAlertValue,
			fetch:      func(string) (system.TopProcesses, error) { return testTopProcesses, nil },
			wantFirst:  "Top processes by CPU",
			wantFetchN: 1,
		},
		{
			name:       "memory alert lists memory first",
			alertName:  "Memory",
			setValue:   setMemoryAlertValue,
			fetch:      func(string) (system.TopProcesses, error) { return testTopProcesses, nil },
			wantFirst:  "Top processes by memory",
			wantFetchN: 1,
		},
		{
			name:      "load alert includes sample",
			alertName: "LoadAvg5",
			setValue: func(info *system.Info, stats *system.Stats, value float64) {
				info.LoadAvg[1] = value
				stats.LoadAvg[1] = value
			},
			fetch:      func(string) (system.TopProcesses, error) { return testTopProcesses, nil },
			wantFirst:  "Top processes by CPU",
			wantFetchN: 1,
		},
		{
			name:       "failed sample still sends the alert",
			alertName:  "CPU",
			setValue:   setCPUAlertValue,
			fetch:      func(string) (system.TopProcesses, error) { return system.TopProcesses{}, errors.New("unknown action") },
			wantFetchN: 1,
		},
		{
			name:      "no fetcher sends the alert without a sample",
			alertName: "CPU",
			setValue:  setCPUAlertValue,
		},
		{
			name:      "other alerts do not sample",
			alertName: "GPU",
			setValue: func(info *system.Info, _ *system.Stats, value float64) {
				info.GpuPct = value
			},
			fetch:      func(string) (system.TopProcesses, error) { return testTopProcesses, nil },
			wantFetchN: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newSystemAlertTestFixture(t, tt.alertName, 1, 50)
			defer fixture.cleanup()

			var fetched []string
			if tt.fetch != nil {
				fixture.hub.SetTopProcessesFetcher(func(systemID string) (system.TopProcesses, error) {
					fetched = append(fetched, systemID)
					return tt.fetch(systemID)
				})
			} else {
				fixture.hub.SetTopProcessesFetcher(nil)
			}

			synctest.Test(t, func(t *testing.T) {
				submitValue(fixture, t, 80, tt.setValue)
				waitForSystemAlert(time.Second)

				fixture.assertTriggered(t, true, "Alert should be triggered")
				require.Equal(t, 1, fixture.hub.TestMailer.TotalSend(), "The alert is sent")
				text := fixture.hub.TestMailer.LastMessage().Text
				assert.Contains(t, text, "averaged 80.00")
				if tt.wantFirst == "" {
					assert.NotContains(t, text, "Top processes")
				} else {
					idx := strings.Index(text, "Top processes")
					require.GreaterOrEqual(t, idx, 0, text)
					assert.True(t, strings.HasPrefix(text[idx:], tt.wantFirst), text)
					assert.Contains(t, text, "syspolicyd (pid 519): 67.8% CPU, 40 MB")
				}
				assert.Len(t, fetched, tt.wantFetchN)

				// Resolving the alert does not take a sample.
				submitValue(fixture, t, 10, tt.setValue)
				waitForSystemAlert(time.Second)
				fixture.assertTriggered(t, false, "Alert should be resolved")
				require.Equal(t, 2, fixture.hub.TestMailer.TotalSend())
				assert.NotContains(t, fixture.hub.TestMailer.LastMessage().Text, "Top processes")
				assert.Len(t, fetched, tt.wantFetchN)

				waitForSystemAlert(time.Minute)
			})
		})
	}
}

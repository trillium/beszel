package alerts

import (
	"fmt"
	"strings"

	"github.com/henrygd/beszel/internal/entities/system"
)

// FetchTopProcessesFunc requests a one-off top processes sample from the agent
// of the given system. Implementations should apply their own timeout.
type FetchTopProcessesFunc = func(systemID string) (system.TopProcesses, error)

// topProcessesNameMaxChars caps process names in notifications.
const topProcessesNameMaxChars = 40

// SetTopProcessesFetcher sets how CPU, memory and load alerts get a sample of
// the top processes to include in the notification. nil disables it.
func (am *AlertManager) SetTopProcessesFetcher(fetch FetchTopProcessesFunc) {
	am.fetchTopProcesses = fetch
}

// alertWantsTopProcesses reports whether an alert (by its stored name) should
// include the top processes when it fires.
func alertWantsTopProcesses(name string) bool {
	switch name {
	case "CPU", "Memory", "LoadAvg1", "LoadAvg5", "LoadAvg15":
		return true
	}
	return false
}

// topProcessesText fetches a sample for the system and formats it for the
// alert body. It returns "" if fetching is disabled or fails, so the alert is
// still sent without it.
func (am *AlertManager) topProcessesText(systemID, alertName string) string {
	if am.fetchTopProcesses == nil {
		return ""
	}
	sample, err := am.fetchTopProcesses(systemID)
	if err != nil {
		am.hub.Logger().Debug("Top processes sample unavailable", "system", systemID, "err", err)
		return ""
	}
	return formatTopProcesses(sample, alertName == "Memory")
}

// formatTopProcesses formats a sample as text blocks for a notification body.
// The memory list comes first if memoryFirst is true.
func formatTopProcesses(sample system.TopProcesses, memoryFirst bool) string {
	cpuBlock := formatProcessList(fmt.Sprintf("Top processes by CPU (%.1fs sample):", float64(sample.SampleMs)/1000), sample.ByCpu)
	memBlock := formatProcessList("Top processes by memory:", sample.ByMem)
	blocks := []string{cpuBlock, memBlock}
	if memoryFirst {
		blocks = []string{memBlock, cpuBlock}
	}
	var parts []string
	for _, block := range blocks {
		if block != "" {
			parts = append(parts, block)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	if sample.Scanned < sample.Total {
		parts = append(parts, fmt.Sprintf("(%d of %d processes read)", sample.Scanned, sample.Total))
	}
	return strings.Join(parts, "\n\n")
}

func formatProcessList(heading string, procs []system.ProcessUsage) string {
	if len(procs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(heading)
	for _, p := range procs {
		name := p.Name
		if name == "" {
			name = "?"
		}
		if runes := []rune(name); len(runes) > topProcessesNameMaxChars {
			name = string(runes[:topProcessesNameMaxChars-1]) + "…"
		}
		fmt.Fprintf(&b, "\n%s (pid %d): %.1f%% CPU, %s", name, p.Pid, p.Cpu, formatRss(p.Rss))
	}
	return b.String()
}

func formatRss(bytes uint64) string {
	const mb = 1024 * 1024
	if bytes >= 1024*mb {
		return fmt.Sprintf("%.1f GB", float64(bytes)/(1024*mb))
	}
	return fmt.Sprintf("%.0f MB", float64(bytes)/mb)
}

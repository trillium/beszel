//go:build darwin

package agent

import "context"

// defaultProcReader uses ps on macOS, where gopsutil cannot read processes
// owned by other users unless the agent runs as root.
func defaultProcReader(ctx context.Context, maxScan int) (map[int32]procReading, int, error) {
	if readings, total, err := readProcessesPs(ctx, maxScan); err == nil {
		return readings, total, nil
	}
	return readProcessesGopsutil(ctx, maxScan)
}

//go:build !darwin

package agent

import "context"

func defaultProcReader(ctx context.Context, maxScan int) (map[int32]procReading, int, int, error) {
	return readProcessesGopsutil(ctx, maxScan)
}

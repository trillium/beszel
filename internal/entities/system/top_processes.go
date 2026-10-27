package system

// ProcessUsage is one process in a top processes sample.
type ProcessUsage struct {
	Pid  int32  `json:"pid" cbor:"0,keyasint"`
	Name string `json:"name" cbor:"1,keyasint"`
	// Cpu is the percent of one core used during the sample window (can exceed 100).
	Cpu float64 `json:"cpu" cbor:"2,keyasint"`
	// Rss is the resident set size in bytes.
	Rss uint64 `json:"rss" cbor:"3,keyasint"`
}

// TopProcesses is the payload returned by the agent for the GetTopProcesses
// action: a single on-demand sample of the heaviest processes.
type TopProcesses struct {
	ByCpu []ProcessUsage `json:"byCpu" cbor:"0,keyasint"`
	ByMem []ProcessUsage `json:"byMem" cbor:"1,keyasint"`
	// SampleMs is the length of the CPU sample window in milliseconds.
	SampleMs uint32 `json:"sampleMs" cbor:"2,keyasint"`
	// Scanned is the number of processes read; Total is the number found.
	// Scanned < Total means the scan hit its cap or time budget.
	Scanned uint32 `json:"scanned" cbor:"3,keyasint"`
	Total   uint32 `json:"total" cbor:"4,keyasint"`
}

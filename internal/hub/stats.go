package hub

// Stats contains only aggregate operational counters. It deliberately never
// exposes user identifiers, prompts, session content, or API keys to admins.
type Stats struct {
	Accounts         int      `json:"accounts"`
	Devices          int      `json:"devices"`
	OnlineDevices    int      `json:"online_devices"`
	AppStreams       int      `json:"app_streams"`
	PendingCommand   int      `json:"pending_commands"`
	PairedDevices    int      `json:"paired_devices"`
	BannedDevices    int      `json:"banned_devices"`
	RunningSessions  int      `json:"running_sessions"`
	WaitingApprovals int      `json:"waiting_approvals"`
	RequestErrors    uint64   `json:"request_errors"`
	CommandSuccess   uint64   `json:"command_success"`
	CommandFailures  uint64   `json:"command_failures"`
	CommandTimeouts  uint64   `json:"command_timeouts"`
	LatencySamples   uint64   `json:"latency_samples"`
	LatencyMeanMS    *float64 `json:"latency_mean_ms"`
}

func (h *Hub) Stats() Stats {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.statsLocked()
}
func (h *Hub) statsLocked() Stats {
	stats := Stats{Accounts: len(h.accounts), PendingCommand: len(h.pending), RequestErrors: h.requestErrors, CommandSuccess: h.commandSuccess, CommandFailures: h.commandFailures, CommandTimeouts: h.commandTimeouts}
	var latencyTotal float64
	for _, account := range h.accounts {
		stats.Devices += len(account.devices)
		stats.AppStreams += len(account.apps)
		for _, device := range account.devices {
			if device.conn != nil && !device.banned {
				stats.OnlineDevices++
			}
			if device.hasPair && !device.banned {
				stats.PairedDevices++
			}
			if device.banned {
				stats.BannedDevices++
			}
			running, waiting := sessionCounts(account, device)
			stats.RunningSessions += running
			stats.WaitingApprovals += waiting
			stats.LatencySamples += device.latency.Samples
			latencyTotal += device.latency.MeanMS * float64(device.latency.Samples)
		}
	}
	if stats.LatencySamples > 0 {
		mean := latencyTotal / float64(stats.LatencySamples)
		stats.LatencyMeanMS = &mean
	}
	return stats
}

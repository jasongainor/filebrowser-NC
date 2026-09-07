package cncapi

// Shared logic behind POST /api/cnc/recovery/ack. Mirrors
// http/cnc.go's cncRecoveryAckHandler.

// RecoveryAck acknowledges machineID's pending recovery, clearing the
// ErrRecoveryPending gate on the next Start.
func (d Deps) RecoveryAck(machineID string) (int, error) {
	st, _, status, err := d.ResolveStreamer(machineID)
	if err != nil {
		return status, err
	}
	st.AckRecovery()
	return 0, nil
}

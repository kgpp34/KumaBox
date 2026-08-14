package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kumabox/kumabox/internal/vm"
)

type eventRecord struct {
	Time          time.Time        `json:"time"`
	Type          string           `json:"type"`
	VMID          string           `json:"vmId"`
	VMName        string           `json:"vmName"`
	State         vm.VMState       `json:"state"`
	ObservedState vm.ObservedState `json:"observedState"`
	Reason        string           `json:"reason,omitempty"`
	PID           int              `json:"pid,omitempty"`
	APISocket     string           `json:"apiSocket,omitempty"`
}

func writeVMEvent(rec *vm.VMRecord, eventType string, obs vm.Observation) (err error) {
	if rec.LogDir == "" {
		return nil
	}
	if err := os.MkdirAll(rec.LogDir, 0o755); err != nil {
		return fmt.Errorf("create VM log dir: %w", err)
	}

	path := filepath.Join(rec.LogDir, "events.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open events log: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close VM events log: %w", closeErr)
		}
	}()

	event := eventRecord{
		Time:          obs.CheckedAt,
		Type:          eventType,
		VMID:          rec.ID,
		VMName:        rec.Name,
		State:         rec.State,
		ObservedState: obs.State,
		Reason:        obs.Reason,
		PID:           rec.PID,
		APISocket:     rec.APISocket,
	}
	if err := json.NewEncoder(file).Encode(event); err != nil {
		return fmt.Errorf("write events log: %w", err)
	}
	return nil
}

package network

import (
	stdjson "encoding/json"
	"fmt"

	metajson "github.com/kumabox/kumabox/internal/metastore/json"
)

const hostTapTable = "host-tap"
const hostTapRecord = "root"

type hostTapCodec struct{}

func (hostTapCodec) Decode(raw []byte) (*metajson.Model, error) {
	model := metajson.NewModel()
	if len(raw) == 0 {
		return model, nil
	}
	var state HostTapState
	if err := stdjson.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("parse host-tap state: %w", err)
	}
	if state.SchemaVersion != "" && state.SchemaVersion != hostTapSchemaVersion {
		return nil, fmt.Errorf("unsupported host-tap schema %q", state.SchemaVersion)
	}
	encoded, err := stdjson.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("encode host-tap state record: %w", err)
	}
	model.Tables[hostTapTable] = map[string]stdjson.RawMessage{hostTapRecord: encoded}
	return model, nil
}

func (hostTapCodec) Encode(model *metajson.Model) ([]byte, error) {
	if model == nil {
		return nil, fmt.Errorf("host-tap metadata model must not be nil")
	}
	raw := model.Tables[hostTapTable][hostTapRecord]
	if len(raw) == 0 {
		return nil, fmt.Errorf("host-tap state is absent")
	}
	var state HostTapState
	if err := stdjson.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("parse host-tap state record: %w", err)
	}
	if state.SchemaVersion == "" {
		state.SchemaVersion = hostTapSchemaVersion
	}
	return stdjson.MarshalIndent(state, "", "  ")
}

package vm

import (
	stdjson "encoding/json"
	"fmt"

	metajson "github.com/kumabox/kumabox/internal/meta/json"
)

const vmIndexTable = "vm-index"
const vmIndexRecord = "root"

// indexCodec keeps the legacy VM index document stable while storing it
// through the engine-neutral metadata transaction boundary.
type indexCodec struct{}

func (indexCodec) Decode(raw []byte) (*metajson.Model, error) {
	model := metajson.NewModel()
	if len(raw) == 0 {
		return model, nil
	}
	var index vmIndex
	if err := stdjson.Unmarshal(raw, &index); err != nil {
		return nil, fmt.Errorf("parse VM index: %w", err)
	}
	index.init()
	encoded, err := stdjson.Marshal(index)
	if err != nil {
		return nil, fmt.Errorf("encode VM index record: %w", err)
	}
	model.Tables[vmIndexTable] = map[string]stdjson.RawMessage{
		vmIndexRecord: encoded,
	}
	return model, nil
}

func (indexCodec) Encode(model *metajson.Model) ([]byte, error) {
	if model == nil {
		return nil, fmt.Errorf("VM index metadata model must not be nil")
	}
	raw := model.Tables[vmIndexTable][vmIndexRecord]
	if len(raw) == 0 {
		index := vmIndex{}
		index.init()
		raw, _ = stdjson.Marshal(index)
	}
	var index vmIndex
	if err := stdjson.Unmarshal(raw, &index); err != nil {
		return nil, fmt.Errorf("parse VM index record: %w", err)
	}
	index.init()
	return stdjson.MarshalIndent(index, "", "  ")
}

package snapshot

import (
	stdjson "encoding/json"
	"fmt"

	metajson "github.com/kumabox/kumabox/internal/metastore/json"
)

const snapshotIndexTable = "snapshot-index"
const snapshotIndexRecord = "root"

// indexCodec keeps the existing snapshot index document stable while the
// metadata engine owns locking and durable publication.
type indexCodec struct{}

func (indexCodec) Decode(raw []byte) (*metajson.Model, error) {
	model := metajson.NewModel()
	if len(raw) == 0 {
		return model, nil
	}
	var index snapshotIndex
	if err := stdjson.Unmarshal(raw, &index); err != nil {
		return nil, fmt.Errorf("decode snapshot index: %w", err)
	}
	index.init()
	encoded, err := stdjson.Marshal(index)
	if err != nil {
		return nil, fmt.Errorf("encode snapshot index record: %w", err)
	}
	model.Tables[snapshotIndexTable] = map[string]stdjson.RawMessage{
		snapshotIndexRecord: encoded,
	}
	return model, nil
}

func (indexCodec) Encode(model *metajson.Model) ([]byte, error) {
	if model == nil {
		return nil, fmt.Errorf("snapshot index metadata model must not be nil")
	}
	raw := model.Tables[snapshotIndexTable][snapshotIndexRecord]
	if len(raw) == 0 {
		index := snapshotIndex{}
		index.init()
		raw, _ = stdjson.Marshal(index)
	}
	var index snapshotIndex
	if err := stdjson.Unmarshal(raw, &index); err != nil {
		return nil, fmt.Errorf("parse snapshot index record: %w", err)
	}
	index.init()
	return stdjson.MarshalIndent(index, "", "  ")
}

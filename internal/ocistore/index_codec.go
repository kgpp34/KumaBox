package ocistore

import (
	stdjson "encoding/json"
	"fmt"

	metajson "github.com/kumabox/kumabox/internal/metastore/json"
)

const contentIndexTable = "oci-content"
const contentIndexRecord = "root"

type indexCodec struct{}

func (indexCodec) Decode(raw []byte) (*metajson.Model, error) {
	model := metajson.NewModel()
	if len(raw) == 0 {
		return model, nil
	}
	var index indexFile
	if err := stdjson.Unmarshal(raw, &index); err != nil {
		return nil, fmt.Errorf("parse OCI content index: %w", err)
	}
	index.init()
	encoded, err := stdjson.Marshal(index)
	if err != nil {
		return nil, fmt.Errorf("encode OCI content index record: %w", err)
	}
	model.Tables[contentIndexTable] = map[string]stdjson.RawMessage{contentIndexRecord: encoded}
	return model, nil
}

func (indexCodec) Encode(model *metajson.Model) ([]byte, error) {
	if model == nil {
		return nil, fmt.Errorf("OCI content metadata model must not be nil")
	}
	raw := model.Tables[contentIndexTable][contentIndexRecord]
	if len(raw) == 0 {
		index := indexFile{}
		index.init()
		raw, _ = stdjson.Marshal(index)
	}
	var index indexFile
	if err := stdjson.Unmarshal(raw, &index); err != nil {
		return nil, fmt.Errorf("parse OCI content index record: %w", err)
	}
	index.init()
	return stdjson.MarshalIndent(index, "", "  ")
}

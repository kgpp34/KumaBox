package image

import (
	stdjson "encoding/json"
	"fmt"

	metajson "github.com/kumabox/kumabox/internal/meta/json"
)

const imageIndexTable = "image-index"
const imageIndexRecord = "root"

type indexCodec struct{}

func (indexCodec) Decode(raw []byte) (*metajson.Model, error) {
	model := metajson.NewModel()
	if len(raw) == 0 {
		return model, nil
	}
	var index imageIndex
	if err := stdjson.Unmarshal(raw, &index); err != nil {
		return nil, fmt.Errorf("parse image index: %w", err)
	}
	index.init()
	encoded, err := stdjson.Marshal(index)
	if err != nil {
		return nil, fmt.Errorf("encode image index record: %w", err)
	}
	model.Tables[imageIndexTable] = map[string]stdjson.RawMessage{imageIndexRecord: encoded}
	return model, nil
}

func (indexCodec) Encode(model *metajson.Model) ([]byte, error) {
	if model == nil {
		return nil, fmt.Errorf("image index metadata model must not be nil")
	}
	raw := model.Tables[imageIndexTable][imageIndexRecord]
	if len(raw) == 0 {
		index := imageIndex{}
		index.init()
		raw, _ = stdjson.Marshal(index)
	}
	var index imageIndex
	if err := stdjson.Unmarshal(raw, &index); err != nil {
		return nil, fmt.Errorf("parse image index record: %w", err)
	}
	index.init()
	return stdjson.MarshalIndent(index, "", "  ")
}

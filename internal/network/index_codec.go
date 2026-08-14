package network

import (
	stdjson "encoding/json"
	"fmt"

	metajson "github.com/kumabox/kumabox/internal/meta/json"
)

const networkIndexTable = "network-index"
const networkIndexRecord = "root"

type networkIndex = index

const networkLeaseTable = "network-leases"
const networkLeaseRecord = "root"

func (idx *networkIndex) init() {
	if idx.SchemaVersion == "" {
		idx.SchemaVersion = indexSchemaVersion
	}
	if idx.Networks == nil {
		idx.Networks = map[string]*Record{}
	}
}

type leaseCodec struct{}

func (leases *leaseIndex) init() {
	if leases.SchemaVersion == "" {
		leases.SchemaVersion = leaseSchemaVersion
	}
	if leases.Leases == nil {
		leases.Leases = map[string]*Lease{}
	}
}

func (leaseCodec) Decode(raw []byte) (*metajson.Model, error) {
	model := metajson.NewModel()
	if len(raw) == 0 {
		return model, nil
	}
	var leases leaseIndex
	if err := stdjson.Unmarshal(raw, &leases); err != nil {
		return nil, fmt.Errorf("parse network leases: %w", err)
	}
	leases.init()
	encoded, err := stdjson.Marshal(leases)
	if err != nil {
		return nil, fmt.Errorf("encode network leases record: %w", err)
	}
	model.Tables[networkLeaseTable] = map[string]stdjson.RawMessage{networkLeaseRecord: encoded}
	return model, nil
}

func (leaseCodec) Encode(model *metajson.Model) ([]byte, error) {
	if model == nil {
		return nil, fmt.Errorf("network leases metadata model must not be nil")
	}
	raw := model.Tables[networkLeaseTable][networkLeaseRecord]
	if len(raw) == 0 {
		leases := leaseIndex{}
		leases.init()
		raw, _ = stdjson.Marshal(leases)
	}
	var leases leaseIndex
	if err := stdjson.Unmarshal(raw, &leases); err != nil {
		return nil, fmt.Errorf("parse network leases record: %w", err)
	}
	leases.init()
	return stdjson.MarshalIndent(leases, "", "  ")
}

type indexCodec struct{}

func (indexCodec) Decode(raw []byte) (*metajson.Model, error) {
	model := metajson.NewModel()
	if len(raw) == 0 {
		return model, nil
	}
	var index networkIndex
	if err := stdjson.Unmarshal(raw, &index); err != nil {
		return nil, fmt.Errorf("parse network index: %w", err)
	}
	index.init()
	encoded, err := stdjson.Marshal(index)
	if err != nil {
		return nil, fmt.Errorf("encode network index record: %w", err)
	}
	model.Tables[networkIndexTable] = map[string]stdjson.RawMessage{networkIndexRecord: encoded}
	return model, nil
}

func (indexCodec) Encode(model *metajson.Model) ([]byte, error) {
	if model == nil {
		return nil, fmt.Errorf("network index metadata model must not be nil")
	}
	raw := model.Tables[networkIndexTable][networkIndexRecord]
	if len(raw) == 0 {
		index := networkIndex{}
		index.init()
		raw, _ = stdjson.Marshal(index)
	}
	var index networkIndex
	if err := stdjson.Unmarshal(raw, &index); err != nil {
		return nil, fmt.Errorf("parse network index record: %w", err)
	}
	index.init()
	return stdjson.MarshalIndent(index, "", "  ")
}

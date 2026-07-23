// Package json implements the metadata engine backed by one JSON document per
// metadata namespace.
package json

import (
	stdjson "encoding/json"
	"fmt"
	"sort"
)

// Codec translates one namespace's existing JSON shape to and from tables.
// Domain packages own codecs so legacy index formats remain compatible while
// the engine owns locking and transaction semantics.
type Codec interface {
	Decode([]byte) (*Model, error)
	Encode(*Model) ([]byte, error)
}

// Model is the engine-neutral in-memory representation of one namespace.
type Model struct {
	Tables map[string]map[string]stdjson.RawMessage
}

func NewModel() *Model {
	return &Model{Tables: map[string]map[string]stdjson.RawMessage{}}
}

func (m *Model) table(name string) map[string]stdjson.RawMessage {
	if m.Tables == nil {
		m.Tables = map[string]map[string]stdjson.RawMessage{}
	}
	if m.Tables[name] == nil {
		m.Tables[name] = map[string]stdjson.RawMessage{}
	}
	return m.Tables[name]
}

// TableCodec handles a document whose top-level fields are table objects. It
// is useful for indexes shaped like {"records":{"id":{...}}} and keeps the
// adapter independent from any particular resource type.
type TableCodec struct {
	Specs []TableSpec
}

// TableSpec maps one top-level JSON field to a metadata table.
type TableSpec struct {
	Key   string
	Table string
}

func (c TableCodec) Decode(raw []byte) (*Model, error) {
	model := NewModel()
	if len(raw) == 0 {
		return model, nil
	}
	var document map[string]stdjson.RawMessage
	if err := stdjson.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("decode metadata JSON: %w", err)
	}
	for _, spec := range c.Specs {
		if spec.Key == "" || spec.Table == "" {
			return nil, fmt.Errorf("metadata table spec is incomplete")
		}
		value, ok := document[spec.Key]
		if !ok {
			continue
		}
		var records map[string]stdjson.RawMessage
		if err := stdjson.Unmarshal(value, &records); err != nil {
			return nil, fmt.Errorf("decode metadata table %q: %w", spec.Key, err)
		}
		for id, record := range records {
			if id == "" || !stdjson.Valid(record) {
				return nil, fmt.Errorf("metadata table %q contains invalid record", spec.Key)
			}
			model.table(spec.Table)[id] = cloneRaw(record)
		}
	}
	return model, nil
}

func (c TableCodec) Encode(model *Model) ([]byte, error) {
	if model == nil {
		return nil, fmt.Errorf("metadata model must not be nil")
	}
	document := make(map[string]map[string]stdjson.RawMessage, len(c.Specs))
	for _, spec := range c.Specs {
		if spec.Key == "" || spec.Table == "" {
			return nil, fmt.Errorf("metadata table spec is incomplete")
		}
		records := model.Tables[spec.Table]
		if records == nil {
			records = map[string]stdjson.RawMessage{}
		}
		copied := make(map[string]stdjson.RawMessage, len(records))
		for id, record := range records {
			if id == "" || !stdjson.Valid(record) {
				return nil, fmt.Errorf("metadata table %q contains invalid record", spec.Table)
			}
			copied[id] = cloneRaw(record)
		}
		document[spec.Key] = copied
	}
	return stdjson.MarshalIndent(document, "", "  ")
}

func cloneRaw(raw stdjson.RawMessage) stdjson.RawMessage {
	if raw == nil {
		return nil
	}
	return append(stdjson.RawMessage(nil), raw...)
}

// TableNames returns stable table names for diagnostics and tests.
func (m *Model) TableNames() []string {
	names := make([]string, 0, len(m.Tables))
	for name := range m.Tables {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

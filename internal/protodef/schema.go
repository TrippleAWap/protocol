// Package protodef generates a ProtoDef schema (the protocol.json format used by
// PrismarineJS minecraft-data) from the Go source of the protocol package.
//
// The schema is derived by reading the Marshal methods of every packet and shared type
// as the Reader runs them: each IO call becomes a field, helpers such as Slice and
// OptionalFunc become arrays and options, and conditions on fields read earlier
// become switches. Only the few encodings that cannot be followed statically (the
// item user-data sub-buffer and the stack request action variant shift) are written by
// hand in overrides.go, and a test decodes packets written by the real Writer with the
// generated schema to check that both agree byte for byte.
package protodef

import (
	"bytes"
	"encoding/json"
)

// Field is one field of a ProtoDef container.
type Field struct {
	Name string `json:"name,omitempty"`
	Anon bool   `json:"anon,omitempty"`
	Type any    `json:"type"`
}

// Map is a JSON object that keeps the insertion order of its keys.
type Map struct {
	keys []string
	vals map[string]any
}

// NewMap returns an empty Map.
func NewMap() *Map { return &Map{vals: map[string]any{}} }

// Set sets key to v. A key that is already set keeps its position.
func (m *Map) Set(key string, v any) {
	if _, ok := m.vals[key]; !ok {
		m.keys = append(m.keys, key)
	}
	m.vals[key] = v
}

// Get returns the value of key.
func (m *Map) Get(key string) (any, bool) {
	v, ok := m.vals[key]
	return v, ok
}

// Keys returns the keys of m in insertion order.
func (m *Map) Keys() []string { return m.keys }

// Len returns the number of keys in m.
func (m *Map) Len() int { return len(m.keys) }

// MarshalJSON writes m with its keys in insertion order.
func (m *Map) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range m.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		vb, err := json.Marshal(m.vals[k])
		if err != nil {
			return nil, err
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// Helpers that build the ProtoDef forms used by the generator.

func container(fields []*Field) any {
	if fields == nil {
		fields = []*Field{}
	}
	return []any{"container", fields}
}

func arrayOf(countType string, typ any) any {
	m := NewMap()
	m.Set("countType", countType)
	m.Set("type", typ)
	return []any{"array", m}
}

func arrayCount(count any, typ any) any {
	m := NewMap()
	m.Set("count", count)
	m.Set("type", typ)
	return []any{"array", m}
}

func option(typ any) any { return []any{"option", typ} }

func switchOf(compareTo string, fields *Map, def any) any {
	m := NewMap()
	m.Set("compareTo", compareTo)
	m.Set("fields", fields)
	if def != nil {
		m.Set("default", def)
	}
	return []any{"switch", m}
}

func encapsulated(lengthType, typ string) any {
	m := NewMap()
	m.Set("lengthType", lengthType)
	m.Set("type", typ)
	return []any{"encapsulated", m}
}

func mapper(typ any, mappings *Map) any {
	m := NewMap()
	m.Set("type", typ)
	m.Set("mappings", mappings)
	return []any{"mapper", m}
}

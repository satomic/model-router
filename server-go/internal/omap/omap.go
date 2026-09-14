// Package omap provides an insertion-ordered string-keyed map that round-trips
// through both JSON and YAML without losing key order.
//
// Why this exists: the Python backend stores its configuration in dicts, and a Python
// dict preserves insertion order. That order is load-bearing, not cosmetic --
// `models` decides the model catalog order, which decides the default model, the
// candidate list handed to the decision model, and the order every narrowed model
// list comes back in. A Go map would randomise all of that on every request.
//
// Values are restricted to the JSON/YAML value set: *Map, []any, string, bool,
// json.Number, and nil. Numbers are kept as json.Number so an integer written by the
// Python backend reads back as an integer rather than as 1.0.
package omap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Map is an insertion-ordered map with string keys.
type Map struct {
	keys []string
	vals map[string]any
}

func New() *Map {
	return &Map{vals: map[string]any{}}
}

func (m *Map) ensure() {
	if m.vals == nil {
		m.vals = map[string]any{}
	}
}

func (m *Map) Len() int {
	if m == nil {
		return 0
	}
	return len(m.keys)
}

// Keys returns the keys in insertion order. The slice is a copy: callers iterate
// while mutating (deleting a model while walking the catalog, for one).
func (m *Map) Keys() []string {
	if m == nil {
		return nil
	}
	out := make([]string, len(m.keys))
	copy(out, m.keys)
	return out
}

func (m *Map) Get(key string) (any, bool) {
	if m == nil {
		return nil, false
	}
	v, ok := m.vals[key]
	return v, ok
}

// Value returns the value or nil, for the common "read a field that may be absent" case.
func (m *Map) Value(key string) any {
	if m == nil {
		return nil
	}
	return m.vals[key]
}

func (m *Map) Has(key string) bool {
	if m == nil {
		return false
	}
	_, ok := m.vals[key]
	return ok
}

func (m *Map) Set(key string, value any) {
	m.ensure()
	if _, exists := m.vals[key]; !exists {
		m.keys = append(m.keys, key)
	}
	m.vals[key] = value
}

func (m *Map) Delete(key string) {
	if m == nil || m.vals == nil {
		return
	}
	if _, exists := m.vals[key]; !exists {
		return
	}
	delete(m.vals, key)
	for i, k := range m.keys {
		if k == key {
			m.keys = append(m.keys[:i], m.keys[i+1:]...)
			break
		}
	}
}

// Clone deep-copies the map, so a caller can hand a mutable view to a request
// without the shared configuration seeing the edits.
func (m *Map) Clone() *Map {
	if m == nil {
		return nil
	}
	out := New()
	for _, k := range m.keys {
		out.Set(k, CloneValue(m.vals[k]))
	}
	return out
}

func CloneValue(v any) any {
	switch t := v.(type) {
	case *Map:
		return t.Clone()
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = CloneValue(item)
		}
		return out
	default:
		return v
	}
}

// -- Typed readers ------------------------------------------------------------
// Every one of these mirrors the Python `(x or {}).get(...)` idiom: a missing key,
// a null, or a value of the wrong type all read as the zero value rather than as an
// error. config.yaml is hand-editable, so a wrong type must never panic a request.

func (m *Map) Map(key string) *Map {
	if v, ok := m.Value(key).(*Map); ok {
		return v
	}
	return nil
}

func (m *Map) Slice(key string) []any {
	if v, ok := m.Value(key).([]any); ok {
		return v
	}
	return nil
}

func (m *Map) Str(key string) string {
	return AsString(m.Value(key))
}

func (m *Map) Bool(key string, fallback bool) bool {
	if v, ok := m.Value(key).(bool); ok {
		return v
	}
	return fallback
}

func (m *Map) Float(key string, fallback float64) float64 {
	if f, ok := AsFloat(m.Value(key)); ok {
		return f
	}
	return fallback
}

func (m *Map) Int(key string, fallback int) int {
	if f, ok := AsFloat(m.Value(key)); ok {
		return int(f)
	}
	return fallback
}

func AsString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	default:
		return fmt.Sprint(t)
	}
}

func AsFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// StringSlice reads a list of scalars as strings, skipping nested containers --
// the same tolerance the Python `[str(x) for x in (raw or [])]` idiom has.
func StringSlice(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		switch item.(type) {
		case *Map, []any:
			continue
		}
		out = append(out, AsString(item))
	}
	return out
}

// -- JSON ---------------------------------------------------------------------

func (m *Map) MarshalJSON() ([]byte, error) {
	if m == nil {
		return []byte("null"), nil
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range m.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		val, err := json.Marshal(m.vals[k])
		if err != nil {
			return nil, err
		}
		buf.Write(val)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func (m *Map) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return fmt.Errorf("omap: expected an object, got %v", tok)
	}
	return m.decodeObject(dec)
}

func (m *Map) decodeObject(dec *json.Decoder) error {
	m.keys = nil
	m.vals = map[string]any{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := tok.(string)
		if !ok {
			return fmt.Errorf("omap: object key is not a string: %v", tok)
		}
		value, err := decodeValue(dec)
		if err != nil {
			return err
		}
		m.Set(key, value)
	}
	_, err := dec.Token() // closing '}'
	return err
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			inner := New()
			if err := inner.decodeObject(dec); err != nil {
				return nil, err
			}
			return inner, nil
		case '[':
			items := []any{}
			for dec.More() {
				item, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				items = append(items, item)
			}
			if _, err := dec.Token(); err != nil { // closing ']'
				return nil, err
			}
			return items, nil
		}
		return nil, fmt.Errorf("omap: unexpected delimiter %v", t)
	default:
		return t, nil
	}
}

// FromJSON decodes an arbitrary JSON document into the omap value set.
func FromJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// -- YAML ---------------------------------------------------------------------

// FromYAMLNode converts a parsed YAML node into the omap value set.
func FromYAMLNode(node *yaml.Node) (any, error) {
	if node == nil {
		return nil, nil
	}
	switch node.Kind {
	case yaml.DocumentNode:
		if len(node.Content) == 0 {
			return nil, nil
		}
		return FromYAMLNode(node.Content[0])
	case yaml.AliasNode:
		return FromYAMLNode(node.Alias)
	case yaml.MappingNode:
		out := New()
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i].Value
			value, err := FromYAMLNode(node.Content[i+1])
			if err != nil {
				return nil, err
			}
			out.Set(key, value)
		}
		return out, nil
	case yaml.SequenceNode:
		out := make([]any, 0, len(node.Content))
		for _, item := range node.Content {
			value, err := FromYAMLNode(item)
			if err != nil {
				return nil, err
			}
			out = append(out, value)
		}
		return out, nil
	case yaml.ScalarNode:
		return scalarValue(node), nil
	}
	return nil, fmt.Errorf("omap: unsupported YAML node kind %d", node.Kind)
}

func scalarValue(node *yaml.Node) any {
	switch node.Tag {
	case "!!null":
		return nil
	case "!!bool":
		return node.Value == "true" || node.Value == "True" || node.Value == "yes" || node.Value == "on"
	case "!!int", "!!float":
		return json.Number(node.Value)
	case "!!str":
		return node.Value
	}
	// An untagged scalar from a hand-written file: resolve it the way YAML would.
	if node.Style == yaml.SingleQuotedStyle || node.Style == yaml.DoubleQuotedStyle {
		return node.Value
	}
	var probe any
	if err := node.Decode(&probe); err == nil {
		switch t := probe.(type) {
		case nil:
			return nil
		case bool:
			return t
		case int:
			return json.Number(strconv.Itoa(t))
		case int64:
			return json.Number(strconv.FormatInt(t, 10))
		case float64:
			return json.Number(node.Value)
		}
	}
	return node.Value
}

// ToYAMLNode renders a value back into a YAML node, so it can be written into an
// existing document without disturbing the rest of it.
func ToYAMLNode(v any) *yaml.Node {
	switch t := v.(type) {
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
	case *Map:
		node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		if t == nil {
			return node
		}
		for _, k := range t.keys {
			node.Content = append(node.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k},
				ToYAMLNode(t.vals[k]),
			)
		}
		return node
	case []any:
		node := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, item := range t {
			node.Content = append(node.Content, ToYAMLNode(item))
		}
		return node
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(t)}
	case json.Number:
		tag := "!!int"
		if strings.ContainsAny(t.String(), ".eE") {
			tag = "!!float"
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: t.String()}
	case int:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(t)}
	case int64:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.FormatInt(t, 10)}
	case float64:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: strconv.FormatFloat(t, 'g', -1, 64)}
	case string:
		node := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: t}
		switch {
		case literalStyleSuits(t):
			// A multi-line value -- the AI decision prompt, above all -- is meant to be read and
			// edited in the file. Left to the emitter it comes back as one folded single-quoted
			// blob with every newline doubled, which is valid YAML and unusable by a human.
			node.Style = yaml.LiteralStyle
		case needsQuoting(t):
			// A string that would otherwise resolve as a number, a bool or null has to be
			// quoted, or reading the file back would change its type.
			node.Style = yaml.SingleQuotedStyle
		case t == "":
			// The emitter spells an unstyled empty string "", where every other quoted scalar
			// in these files is single-quoted. One style keeps the diffs quiet.
			node.Style = yaml.SingleQuotedStyle
		}
		return node
	default:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: fmt.Sprint(t)}
	}
}

// literalStyleSuits reports whether a value can be written as a `|` block without the emitter
// having to re-encode it some other way.
//
// Literal style cannot represent a line with trailing whitespace (the indicator would be lost on
// the way back in) or a carriage return, so those fall through to the quoted path rather than
// risking a value that reads back different from what was written.
func literalStyleSuits(s string) bool {
	if !strings.Contains(s, "\n") || strings.ContainsRune(s, '\r') {
		return false
	}
	for _, line := range strings.Split(s, "\n") {
		if line != strings.TrimRight(line, " \t") {
			return false
		}
	}
	return true
}

func needsQuoting(s string) bool {
	if s == "" {
		return false
	}
	var probe any
	if err := yaml.Unmarshal([]byte(s), &probe); err != nil {
		return true
	}
	if _, ok := probe.(string); !ok {
		return true
	}
	return false
}

// Num renders a float as a JSON number that always carries a decimal point, the way Python's
// json.dumps writes a float. Keeping the two backends' numbers spelled identically means a
// config document or a stored snapshot written by one is byte-comparable with the other's.
func Num(v float64) json.Number {
	text := strconv.FormatFloat(v, 'f', -1, 64)
	if !strings.ContainsAny(text, ".eE") {
		text += ".0"
	}
	return json.Number(text)
}

// Int renders an integer as a JSON number.
func Int(v int) json.Number { return json.Number(strconv.Itoa(v)) }

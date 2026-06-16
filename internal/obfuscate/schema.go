package obfuscate

import "fmt"

// Field is a node in the declarative schema. IDs are stable, internal, and are
// NEVER written to the wire — only their daily obfuscated names are. A field
// with non-empty Children is a nested object; a leaf field carries a value.
type Field struct {
	ID       string
	Children []Field // non-empty only when the field is an object
}

// Build is used by the server. For every field it looks up the real value in
// values (keyed by field ID) and writes it under the field's obfuscated name
// for the day, recursing into Children for nested objects.
func Build(schema []Field, values map[string]any, shift int) map[string]any {
	out := make(map[string]any, len(schema))
	for _, f := range schema {
		name := ObfuscatedName(f.ID, shift)
		if len(f.Children) > 0 {
			child, _ := values[f.ID].(map[string]any)
			out[name] = Build(f.Children, child, shift)
			continue
		}
		out[name] = values[f.ID]
	}
	return out
}

// Parse is used by the client. For every field it recomputes the expected
// obfuscated name, looks it up in raw, and returns a map keyed by field ID. It
// returns an explicit error if an expected field is missing — a quick signal
// that the schema or the shift is out of sync between client and server.
func Parse(schema []Field, raw map[string]any, shift int) (map[string]any, error) {
	out := make(map[string]any, len(schema))
	for _, f := range schema {
		name := ObfuscatedName(f.ID, shift)
		v, ok := raw[name]
		if !ok {
			return nil, fmt.Errorf("obfuscate: expected field %q (obfuscated %q) not found in response", f.ID, name)
		}
		if len(f.Children) > 0 {
			child, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("obfuscate: field %q (obfuscated %q) expected to be an object, got %T", f.ID, name, v)
			}
			parsed, err := Parse(f.Children, child, shift)
			if err != nil {
				return nil, err
			}
			out[f.ID] = parsed
			continue
		}
		out[f.ID] = v
	}
	return out, nil
}

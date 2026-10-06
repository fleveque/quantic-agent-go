package provenance

import (
	"encoding/json"
	"fmt"
	"sort"
)

// DataFinding is a value in structured data that no tool returned.
type DataFinding struct {
	Path  string // where in the data: "ex_dividends[0].amount"
	Value any
}

func (f DataFinding) String() string { return fmt.Sprintf("%s = %v", f.Path, f.Value) }

// CheckData reports every value in data that the manifest doesn't contain.
// data is a post's typed data block: a struct, a map, or anything else that
// encodes to JSON. Every number and every string in it must be one a tool
// returned, exactly. That is the whole point of keeping figures out of prose
// and in typed fields: "ex_dividends[0].amount is 0.83" is checked by lookup,
// not by parsing text (docs/rendering.md).
//
// Strings are held to the same rule as numbers, so an invented ticker or a
// date no tool returned is caught too. Booleans and nulls carry no claims.
func CheckData(data any, m *Manifest) ([]DataFinding, error) {
	// Encoding to JSON and back turns structs, maps and slices alike into the
	// same few types (map[string]any, []any, float64, string), with the field
	// names the post will use. One walk then handles them all.
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("provenance: encoding data: %w", err)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("provenance: decoding data: %w", err)
	}
	var out []DataFinding
	m.check(v, "", &out)
	return out, nil
}

func (m *Manifest) check(v any, path string, out *[]DataFinding) {
	switch v := v.(type) {
	case float64:
		if len(m.numbers[v]) == 0 {
			*out = append(*out, DataFinding{Path: path, Value: v})
		}
	case string:
		if len(m.strings[v]) == 0 {
			*out = append(*out, DataFinding{Path: path, Value: v})
		}
	case []any:
		for i, item := range v {
			m.check(item, fmt.Sprintf("%s[%d]", path, i), out)
		}
	case map[string]any:
		// Map order is random in Go; sorting the keys keeps the findings in
		// the same order every run, which tests and people both rely on.
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			next := k
			if path != "" {
				next = path + "." + k
			}
			m.check(v[k], next, out)
		}
	}
}

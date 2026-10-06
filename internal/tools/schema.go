package tools

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Schema is the subset of JSON Schema that tool arguments need: what a model
// is shown so it knows which arguments a tool takes.
type Schema struct {
	Type        string             `json:"type"`
	Description string             `json:"description,omitempty"`
	Properties  map[string]*Schema `json:"properties,omitempty"`
	Required    []string           `json:"required,omitempty"`
	Items       *Schema            `json:"items,omitempty"`

	// Bounds on a number, from `min` and `max` tags. Pointers, because 0 is
	// a real bound and must stay distinguishable from no bound at all.
	Minimum *float64 `json:"minimum,omitempty"`
	Maximum *float64 `json:"maximum,omitempty"`
}

// SchemaOf derives the JSON Schema of a tool's argument struct from the
// struct's own definition, so the Go type is the single source of truth: the
// schema shown to the model and the type its arguments are decoded into can't
// disagree.
//
// The rules, read off each exported field:
//   - the property name is the field's json tag, or the field name without one;
//     a json tag of "-" leaves the field out;
//   - a field is required unless its json tag says omitempty, or it's a pointer;
//   - a `desc` tag becomes the property's description;
//   - `min` and `max` tags on a number become its minimum and maximum, which
//     Tool.DecodeArgs also enforces.
//
// Fields of a type JSON Schema has no word for (maps, channels, funcs) are an
// error rather than something silently guessed.
func SchemaOf(args any) (*Schema, error) {
	t := reflect.TypeOf(args)
	if t == nil || t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("tools: arguments must be a struct, got %T", args)
	}
	return schemaOf(t)
}

func schemaOf(t reflect.Type) (*Schema, error) {
	switch t.Kind() {
	case reflect.Pointer:
		return schemaOf(t.Elem())
	case reflect.String:
		return &Schema{Type: "string"}, nil
	case reflect.Bool:
		return &Schema{Type: "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return &Schema{Type: "integer"}, nil
	case reflect.Float32, reflect.Float64:
		return &Schema{Type: "number"}, nil
	case reflect.Slice, reflect.Array:
		items, err := schemaOf(t.Elem())
		if err != nil {
			return nil, err
		}
		return &Schema{Type: "array", Items: items}, nil
	case reflect.Struct:
		return objectSchema(t)
	}
	return nil, fmt.Errorf("tools: no JSON Schema type for %s", t)
}

func objectSchema(t reflect.Type) (*Schema, error) {
	s := &Schema{Type: "object", Properties: map[string]*Schema{}}
	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		name, opts, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}

		prop, err := schemaOf(field.Type)
		if err != nil {
			return nil, fmt.Errorf("tools: field %s: %w", field.Name, err)
		}
		prop.Description = field.Tag.Get("desc")
		if prop.Minimum, err = bound(field, "min"); err != nil {
			return nil, err
		}
		if prop.Maximum, err = bound(field, "max"); err != nil {
			return nil, err
		}
		s.Properties[name] = prop

		optional := field.Type.Kind() == reflect.Pointer || strings.Contains(opts, "omitempty")
		if !optional {
			s.Required = append(s.Required, name)
		}
	}
	return s, nil
}

// bound reads a `min` or `max` tag. Only numbers can have one.
func bound(field reflect.StructField, tag string) (*float64, error) {
	raw, ok := field.Tag.Lookup(tag)
	if !ok {
		return nil, nil
	}
	if k := field.Type.Kind(); k == reflect.String || k == reflect.Bool || k == reflect.Struct || k == reflect.Slice {
		return nil, fmt.Errorf("tools: field %s: a %s tag needs a number, not %s", field.Name, tag, field.Type)
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return nil, fmt.Errorf("tools: field %s: %s tag %q is not a number", field.Name, tag, raw)
	}
	return &v, nil
}

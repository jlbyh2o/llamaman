package openapi

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

// JSON Schema generation by reflection over the DTO types.
//
// Reflection rather than hand-written schemas, for one reason: a hand-written
// schema is a second description of the same struct, and D43's drift check can
// only prove that the COMMITTED document matches the GENERATED one — it cannot
// prove that a hand-written schema matches the Go type it claims to describe.
// Reflecting means adding a field to a DTO changes openapi.json in the same
// commit, and the check has teeth.
//
// The subset handled here is the subset the DTO conventions of section 3 admit:
// structs with json tags, pointers (nullable), slices, maps with string keys,
// the basic scalars, and time.Time (which no DTO should carry — timestamps are
// RFC 3339 strings — but which is mapped rather than rejected so a mistake
// produces a document rather than a build failure).

// schemas collects named component schemas as they are discovered, so a type
// used by three routes is emitted once and referenced three times.
type schemas struct {
	byName map[string]map[string]any
	// inProgress guards against a self-referential type recursing forever.
	inProgress map[reflect.Type]string
}

func newSchemas() *schemas {
	return &schemas{
		byName:     map[string]map[string]any{},
		inProgress: map[reflect.Type]string{},
	}
}

// ref returns a $ref to the named component schema for v's type, registering it
// (and everything it contains) on first sight. A non-struct type has no useful
// name, so it is returned inline instead.
func (s *schemas) ref(v any) (map[string]any, error) {
	if v == nil {
		return nil, nil
	}
	t := reflect.TypeOf(v)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || t == reflect.TypeOf(time.Time{}) {
		return s.schemaFor(t)
	}
	name, err := s.register(t)
	if err != nil {
		return nil, err
	}
	return map[string]any{"$ref": "#/components/schemas/" + name}, nil
}

// register names a struct type and adds its schema to the component set.
//
// The name is the Go type name, qualified by its package only when two packages
// contribute a type with the same name — an unqualified `Meta` is what a
// TypeScript consumer wants to read, and qualifying every name defensively
// would make the generated types unpleasant for a collision that may never
// happen.
func (s *schemas) register(t reflect.Type) (string, error) {
	if name, busy := s.inProgress[t]; busy {
		return name, nil
	}

	name := t.Name()
	if name == "" {
		return "", fmt.Errorf("openapi: anonymous struct types cannot be documented; give %s a name", t.String())
	}
	if existing, taken := s.byName[name]; taken {
		if existing["x-go-type"] != t.String() {
			name = qualified(t)
		}
	}

	s.inProgress[t] = name
	defer delete(s.inProgress, t)

	body, err := s.structSchema(t)
	if err != nil {
		return "", err
	}
	body["x-go-type"] = t.String()
	s.byName[name] = body
	return name, nil
}

// qualified disambiguates two same-named types from different packages by
// prefixing the package name: `model.Error` and `api.Error` become `Error` and
// `ApiError`, whichever was registered second.
func qualified(t reflect.Type) string {
	pkg := t.PkgPath()
	if i := strings.LastIndexByte(pkg, '/'); i >= 0 {
		pkg = pkg[i+1:]
	}
	if pkg == "" {
		return t.Name()
	}
	return strings.ToUpper(pkg[:1]) + pkg[1:] + t.Name()
}

func (s *schemas) structSchema(t reflect.Type) (map[string]any, error) {
	props := map[string]any{}
	var required []string

	var walk func(reflect.Type) error
	walk = func(t reflect.Type) error {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			tag := f.Tag.Get("json")
			if tag == "-" {
				continue
			}
			parts := strings.Split(tag, ",")
			jsonName := parts[0]
			omitempty := false
			for _, o := range parts[1:] {
				if o == "omitempty" || o == "omitzero" {
					omitempty = true
				}
			}
			if f.Anonymous && jsonName == "" {
				ft := f.Type
				for ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}
				if ft.Kind() == reflect.Struct {
					if err := walk(ft); err != nil {
						return err
					}
					continue
				}
			}
			if jsonName == "" {
				jsonName = f.Name
			}

			sch, err := s.schemaFor(f.Type)
			if err != nil {
				return fmt.Errorf("%s.%s: %w", t.Name(), f.Name, err)
			}
			props[jsonName] = sch

			// A pointer field is nullable and therefore never required; an
			// omitempty field may be absent. Everything else is required,
			// which is what makes "a missing documented field" a conformance
			// failure rather than a shrug.
			if f.Type.Kind() != reflect.Pointer && !omitempty {
				required = append(required, jsonName)
			}
		}
		return nil
	}
	if err := walk(t); err != nil {
		return nil, err
	}

	out := map[string]any{
		"type":       "object",
		"properties": props,
		// The response-conformance middleware fails on an extra field, so the
		// document says so in the vocabulary a validator already understands.
		"additionalProperties": false,
	}
	if len(required) > 0 {
		sort.Strings(required)
		out["required"] = required
	}
	return out, nil
}

func (s *schemas) schemaFor(t reflect.Type) (map[string]any, error) {
	nullable := false
	for t.Kind() == reflect.Pointer {
		nullable = true
		t = t.Elem()
	}

	var out map[string]any
	switch {
	case t == reflect.TypeOf(time.Time{}):
		out = map[string]any{"type": "string", "format": "date-time"}

	case t.Kind() == reflect.Struct:
		name, err := s.register(t)
		if err != nil {
			return nil, err
		}
		out = map[string]any{"$ref": "#/components/schemas/" + name}

	case t.Kind() == reflect.Slice || t.Kind() == reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			// []byte marshals as base64.
			out = map[string]any{"type": "string", "format": "byte"}
			break
		}
		item, err := s.schemaFor(t.Elem())
		if err != nil {
			return nil, err
		}
		out = map[string]any{"type": "array", "items": item}

	case t.Kind() == reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("openapi: map keys must be strings, got %s", t.Key())
		}
		if t.Elem().Kind() == reflect.Interface {
			// The `details` bag of the error envelope: free-form by design.
			out = map[string]any{"type": "object"}
			break
		}
		val, err := s.schemaFor(t.Elem())
		if err != nil {
			return nil, err
		}
		out = map[string]any{"type": "object", "additionalProperties": val}

	case t.Kind() == reflect.String:
		out = map[string]any{"type": "string"}

	case t.Kind() == reflect.Bool:
		out = map[string]any{"type": "boolean"}

	case isInt(t.Kind()):
		out = map[string]any{"type": "integer", "format": int64Format(t.Kind())}

	case t.Kind() == reflect.Float32 || t.Kind() == reflect.Float64:
		out = map[string]any{"type": "number"}

	case t.Kind() == reflect.Interface:
		out = map[string]any{}

	default:
		return nil, fmt.Errorf("openapi: no schema for Go kind %s", t.Kind())
	}

	if nullable {
		// OpenAPI 3.1 is JSON Schema 2020-12, where nullability is a type
		// union rather than the 3.0 `nullable: true` keyword. A $ref cannot
		// carry sibling keywords portably, so it is wrapped in a one-element
		// anyOf.
		if _, isRef := out["$ref"]; isRef {
			return map[string]any{"anyOf": []any{out, map[string]any{"type": "null"}}}, nil
		}
		out["type"] = []any{out["type"], "null"}
	}
	return out, nil
}

func isInt(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	}
	return false
}

// int64Format tells a TypeScript generator that a value may exceed 2^53. Byte
// counts on this API routinely do — a 70 B model is ~4×10^10 bytes — so the
// distinction is not academic.
func int64Format(k reflect.Kind) string {
	switch k {
	case reflect.Int64, reflect.Uint64, reflect.Int, reflect.Uint:
		return "int64"
	default:
		return "int32"
	}
}

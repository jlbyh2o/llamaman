package gguf

import "math"

// Value is one decoded metadata value. Type selects which of the remaining
// fields carries the payload; the others are zero.
//
// The fields are exported rather than hidden behind accessors because a caller
// that already knows a key's type — and the arch keys all have known types —
// should be able to read it without a conversion. The As* methods exist for the
// caller that does not: GGUF producers disagree about the width of the same
// logical key (block_count appears as uint32 and as uint64 in the wild, and
// llama.cpp accepts either), so anything reading a number by name must coerce
// across widths rather than demand one.
type Value struct {
	Type ValueType

	Uint   uint64  // ValueUint8, ValueUint16, ValueUint32, ValueUint64
	Int    int64   // ValueInt8, ValueInt16, ValueInt32, ValueInt64
	Float  float64 // ValueFloat32, ValueFloat64
	Bool   bool    // ValueBool
	String string  // ValueString
	Array  *Array  // ValueArray
}

// Array is a decoded metadata array. GGUF arrays are homogeneous and may nest,
// so Type is itself allowed to be ValueArray.
//
// Len is the count the file declares and is always filled in. Values is filled
// in only when the array was retained: a tokenizer vocabulary is a few hundred
// thousand strings that no consumer of this package wants in memory, and DESIGN
// section 8.5 says such arrays are parsed but not kept. When the limit elides
// one, Elided is true, Values is nil, and Len still answers the only question
// anyone asks of a token array — how many tokens, which is n_vocab.
type Array struct {
	Type   ValueType
	Len    uint64
	Values []Value
	Elided bool
}

// AsUint returns the value as an unsigned integer, widening any of the four
// unsigned types, accepting a non-negative signed one, and accepting a bool as
// 0 or 1. It reports false for a negative number, a float, a string or an array.
func (v Value) AsUint() (uint64, bool) {
	switch v.Type {
	case ValueUint8, ValueUint16, ValueUint32, ValueUint64:
		return v.Uint, true
	case ValueInt8, ValueInt16, ValueInt32, ValueInt64:
		if v.Int < 0 {
			return 0, false
		}
		return uint64(v.Int), true
	case ValueBool:
		if v.Bool {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

// AsInt returns the value as a signed integer, widening any of the four signed
// types and accepting an unsigned one that fits. A uint64 above MaxInt64 is
// refused rather than wrapped: every consumer of this package treats these
// numbers as counts, and a negative count is worse than a missing one.
func (v Value) AsInt() (int64, bool) {
	switch v.Type {
	case ValueInt8, ValueInt16, ValueInt32, ValueInt64:
		return v.Int, true
	case ValueUint8, ValueUint16, ValueUint32, ValueUint64:
		if v.Uint > math.MaxInt64 {
			return 0, false
		}
		return int64(v.Uint), true
	case ValueBool:
		if v.Bool {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

// AsFloat returns the value as a float, accepting either float width and any
// integer type.
func (v Value) AsFloat() (float64, bool) {
	switch v.Type {
	case ValueFloat32, ValueFloat64:
		return v.Float, true
	case ValueInt8, ValueInt16, ValueInt32, ValueInt64:
		return float64(v.Int), true
	case ValueUint8, ValueUint16, ValueUint32, ValueUint64:
		return float64(v.Uint), true
	default:
		return 0, false
	}
}

// AsString returns the value as a string, and reports false for every other
// type. It does not stringify numbers: a caller asking for a string wants the
// key to have been one.
func (v Value) AsString() (string, bool) {
	if v.Type != ValueString {
		return "", false
	}
	return v.String, true
}

// AsBool returns the value as a bool, accepting an integer as C does — zero is
// false, anything else true — because `clip.has_vision_encoder` and its
// neighbors have been written both ways.
func (v Value) AsBool() (bool, bool) {
	switch v.Type {
	case ValueBool:
		return v.Bool, true
	case ValueUint8, ValueUint16, ValueUint32, ValueUint64:
		return v.Uint != 0, true
	case ValueInt8, ValueInt16, ValueInt32, ValueInt64:
		return v.Int != 0, true
	default:
		return false, false
	}
}

// AsInts returns a value that is logically a list of integers, as one: a scalar
// becomes a one-element slice, and an array of any integer type becomes its
// elements. This is what reads `{arch}.attention.head_count_kv`, which DESIGN
// section 8.1 says is a scalar OR a per-layer array and which section 8.3 then
// indexes per layer either way.
//
// It reports false for an elided array — the elements were not kept, so there
// is nothing to return and silently answering with a short slice would put
// wrong per-layer numbers into the KV-cache formula.
func (v Value) AsInts() ([]int64, bool) {
	if v.Type != ValueArray {
		n, ok := v.AsInt()
		if !ok {
			return nil, false
		}
		return []int64{n}, true
	}
	if v.Array == nil || v.Array.Elided {
		return nil, false
	}
	out := make([]int64, 0, len(v.Array.Values))
	for _, e := range v.Array.Values {
		n, ok := e.AsInt()
		if !ok {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// AsStrings returns a value that is logically a list of strings, as one, with
// the same scalar-widening and elision rules as AsInts.
func (v Value) AsStrings() ([]string, bool) {
	if v.Type != ValueArray {
		s, ok := v.AsString()
		if !ok {
			return nil, false
		}
		return []string{s}, true
	}
	if v.Array == nil || v.Array.Elided {
		return nil, false
	}
	out := make([]string, 0, len(v.Array.Values))
	for _, e := range v.Array.Values {
		s, ok := e.AsString()
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// Count is the number of elements a value holds: the declared length for an
// array — which survives elision — and 1 for a scalar. It is how n_vocab is
// read off `tokenizer.ggml.tokens` without keeping the tokens.
func (v Value) Count() uint64 {
	if v.Type == ValueArray {
		if v.Array == nil {
			return 0
		}
		return v.Array.Len
	}
	return 1
}

// Any renders the value as an ordinary Go value — uint64, int64, float64, bool,
// string, or []any for an array — for callers that marshal the metadata map to
// JSON (`models.metadata_json`). An elided array renders as nil, so a token list
// that was never retained does not reappear as an empty list and get mistaken
// for a model with no vocabulary.
func (v Value) Any() any {
	switch v.Type {
	case ValueUint8, ValueUint16, ValueUint32, ValueUint64:
		return v.Uint
	case ValueInt8, ValueInt16, ValueInt32, ValueInt64:
		return v.Int
	case ValueFloat32, ValueFloat64:
		return v.Float
	case ValueBool:
		return v.Bool
	case ValueString:
		return v.String
	case ValueArray:
		if v.Array == nil || v.Array.Elided {
			return nil
		}
		out := make([]any, 0, len(v.Array.Values))
		for _, e := range v.Array.Values {
			out = append(out, e.Any())
		}
		return out
	default:
		return nil
	}
}

// KVPair is one entry of the metadata table, in file order.
type KVPair struct {
	Key   string
	Value Value
}

// KV is a GGUF metadata table: an ordered list of key/value pairs with a lookup
// index. Order is kept because it is the file's own and a round-trip test that
// loses it proves less; lookup is by exact key, because every key this product
// reads is a literal from DESIGN section 8.1.
type KV struct {
	pairs []KVPair
	index map[string]int
}

// Len is the number of pairs.
func (kv KV) Len() int { return len(kv.pairs) }

// All returns the pairs in file order. The slice is the table's own; callers
// must not modify it.
func (kv KV) All() []KVPair { return kv.pairs }

// Get returns the value for a key.
func (kv KV) Get(key string) (Value, bool) {
	i, ok := kv.index[key]
	if !ok {
		return Value{}, false
	}
	return kv.pairs[i].Value, true
}

// Has reports whether the key is present, which for the optional keys of DESIGN
// section 8.3 is the whole question: `attention.sliding_window` being ABSENT is
// what means "this model has no sliding-window attention", and is not the same
// as its being zero.
func (kv KV) Has(key string) bool { _, ok := kv.index[key]; return ok }

// String returns a string-typed value.
func (kv KV) String(key string) (string, bool) {
	v, ok := kv.Get(key)
	if !ok {
		return "", false
	}
	return v.AsString()
}

// Uint returns a value coerced to an unsigned integer (see Value.AsUint).
func (kv KV) Uint(key string) (uint64, bool) {
	v, ok := kv.Get(key)
	if !ok {
		return 0, false
	}
	return v.AsUint()
}

// Int returns a value coerced to a signed integer (see Value.AsInt).
func (kv KV) Int(key string) (int64, bool) {
	v, ok := kv.Get(key)
	if !ok {
		return 0, false
	}
	return v.AsInt()
}

// Float returns a value coerced to a float (see Value.AsFloat).
func (kv KV) Float(key string) (float64, bool) {
	v, ok := kv.Get(key)
	if !ok {
		return 0, false
	}
	return v.AsFloat()
}

// Bool returns a value coerced to a bool (see Value.AsBool).
func (kv KV) Bool(key string) (bool, bool) {
	v, ok := kv.Get(key)
	if !ok {
		return false, false
	}
	return v.AsBool()
}

// Ints returns a scalar or array value as a list of integers (see Value.AsInts).
func (kv KV) Ints(key string) ([]int64, bool) {
	v, ok := kv.Get(key)
	if !ok {
		return nil, false
	}
	return v.AsInts()
}

// Strings returns a scalar or array value as a list of strings (see
// Value.AsStrings).
func (kv KV) Strings(key string) ([]string, bool) {
	v, ok := kv.Get(key)
	if !ok {
		return nil, false
	}
	return v.AsStrings()
}

// Map renders the whole table as ordinary Go values for JSON encoding (see
// Value.Any). Keys whose value was an elided array are present with a nil value,
// so the encoded metadata still records that the key existed.
func (kv KV) Map() map[string]any {
	out := make(map[string]any, len(kv.pairs))
	for _, p := range kv.pairs {
		out[p.Key] = p.Value.Any()
	}
	return out
}

func (kv *KV) add(key string, v Value) bool {
	if kv.index == nil {
		kv.index = make(map[string]int)
	}
	if _, dup := kv.index[key]; dup {
		return false
	}
	kv.index[key] = len(kv.pairs)
	kv.pairs = append(kv.pairs, KVPair{Key: key, Value: v})
	return true
}

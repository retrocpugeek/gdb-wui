// Package mi implements a GDB/MI (machine interface) codec and process
// supervisor. It has zero domain knowledge: it knows how to speak to a gdb
// process and how to turn MI text into values, and nothing about breakpoints,
// frames, or debugging sessions.
package mi

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Kind discriminates the three MI value shapes.
type Kind uint8

const (
	// KindConst is a C string literal: value="0x1234".
	KindConst Kind = iota
	// KindTuple is a brace group: frame={level="0",func="main"}.
	KindTuple
	// KindList is a bracket group: stack=[frame={...},frame={...}].
	KindList
)

func (k Kind) String() string {
	switch k {
	case KindConst:
		return "const"
	case KindTuple:
		return "tuple"
	case KindList:
		return "list"
	}
	return "invalid"
}

// Result is one name=value pair. Name is empty for anonymous list elements,
// which is how MI writes lists of tuples: variables=[{name="i"},{name="j"}].
type Result struct {
	Name  string
	Value Value
}

// Value is an MI value. Tuples and lists share Items deliberately: MI lists can
// hold *named* elements with the same name repeated (stack=[frame={},frame={}],
// body=[bkpt={},bkpt={}]), so an ordered slice is the only representation that
// keeps both order and duplicates. A map would silently drop every frame but
// the last, which is why encoding/json cannot read MI and this type exists.
type Value struct {
	Kind  Kind
	Str   string // KindConst only; already unescaped and valid UTF-8
	Items []Result
}

// Results is the ordered result list of a record, or the contents of a tuple
// or list. The accessors below are the entire read API.
type Results []Result

// Get returns the first value with the given name.
func (rs Results) Get(name string) (Value, bool) {
	for _, r := range rs {
		if r.Name == name {
			return r.Value, true
		}
	}
	return Value{}, false
}

// All returns every value with the given name, in order. This is the accessor
// that makes repeated keys usable: v.All("frame"), v.All("bkpt").
func (rs Results) All(name string) []Value {
	var out []Value
	for _, r := range rs {
		if r.Name == name {
			out = append(out, r.Value)
		}
	}
	return out
}

// Has reports whether a result with the given name is present. MI uses
// *absence* as a signal in places that matter — -stack-list-variables
// --simple-values omits "value" exactly for the aggregates that are
// expandable — so this is a first-class query, not sugar for Get.
func (rs Results) Has(name string) bool {
	_, ok := rs.Get(name)
	return ok
}

// Str returns the named constant, or "" if absent or not a constant.
func (rs Results) Str(name string) string {
	s, _ := rs.StrOK(name)
	return s
}

// StrOK returns the named constant and whether it was present as a constant.
func (rs Results) StrOK(name string) (string, bool) {
	v, ok := rs.Get(name)
	if !ok || v.Kind != KindConst {
		return "", false
	}
	return v.Str, true
}

// U64 parses the named constant as an unsigned integer. Base 0, so both
// "0x00000000004af4a0" and "42" work — MI mixes the two freely.
func (rs Results) U64(name string) (uint64, bool) {
	s, ok := rs.StrOK(name)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(s), 0, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// Int parses the named constant as a signed int (base 0).
func (rs Results) Int(name string) (int, bool) {
	s, ok := rs.StrOK(name)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 0, 64)
	if err != nil {
		return 0, false
	}
	return int(n), true
}

// Bool reads MI's "y"/"n" (and "0"/"1", "true"/"false") booleans.
func (rs Results) Bool(name string) (bool, bool) {
	s, ok := rs.StrOK(name)
	if !ok {
		return false, false
	}
	switch s {
	case "y", "1", "true":
		return true, true
	case "n", "0", "false":
		return false, true
	}
	return false, false
}

// Tuple returns the named value's contents if it is a tuple.
func (rs Results) Tuple(name string) (Results, bool) {
	v, ok := rs.Get(name)
	if !ok || v.Kind != KindTuple {
		return nil, false
	}
	return v.Items, true
}

// List returns the elements of the named value if it is a list. Element names
// (the "frame=" in stack=[frame={...}]) are dropped here; use Get + Elements
// when the name matters.
func (rs Results) List(name string) ([]Value, bool) {
	v, ok := rs.Get(name)
	if !ok || v.Kind != KindList {
		return nil, false
	}
	return v.Elements(), true
}

// Delegating accessors so callers can chain through nested values without
// reaching into .Items at every step.
//
// There is deliberately no Value.Str(name) — Str is the constant's own text,
// and a field and a method cannot share a name. Reach for v.StrOK(name) when
// presence matters, or v.Results().Str(name) when it does not.

// Results views a tuple's or list's contents as a result list.
func (v Value) Results() Results { return Results(v.Items) }

func (v Value) Get(name string) (Value, bool)    { return Results(v.Items).Get(name) }
func (v Value) All(name string) []Value          { return Results(v.Items).All(name) }
func (v Value) Has(name string) bool             { return Results(v.Items).Has(name) }
func (v Value) StrOK(name string) (string, bool) { return Results(v.Items).StrOK(name) }
func (v Value) U64(name string) (uint64, bool)   { return Results(v.Items).U64(name) }
func (v Value) Int(name string) (int, bool)      { return Results(v.Items).Int(name) }
func (v Value) Bool(name string) (bool, bool)    { return Results(v.Items).Bool(name) }
func (v Value) Tuple(name string) (Results, bool) {
	return Results(v.Items).Tuple(name)
}
func (v Value) List(name string) ([]Value, bool) { return Results(v.Items).List(name) }

// Elements returns the values of a list or tuple, discarding element names.
func (v Value) Elements() []Value {
	out := make([]Value, 0, len(v.Items))
	for _, it := range v.Items {
		out = append(out, it.Value)
	}
	return out
}

// Const builds a constant value (used by tests and the fake gdb).
func Const(s string) Value { return Value{Kind: KindConst, Str: s} }

// MarshalJSON renders the canonical JSON form. This is what makes the raw MI
// console and passthrough free: any record can be handed to the browser without
// a per-command DTO.
//
// The mapping, which is stable and documented in docs/protocol.md:
//
//	const  -> JSON string
//	tuple  -> JSON object, keys in MI order
//	list   -> JSON array; a *named* element becomes a single-key object, so
//	          stack=[frame={...}] is [{"frame":{...}}] and no name is lost.
//
// Named list elements keep their name because dropping it would erase the only
// thing distinguishing body=[bkpt={...}] from a bare list of tuples, and the
// name is what the frontend keys on.
func (v Value) MarshalJSON() ([]byte, error) {
	var b []byte
	var err error
	b, err = v.appendJSON(b)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (v Value) appendJSON(b []byte) ([]byte, error) {
	switch v.Kind {
	case KindConst:
		s, err := json.Marshal(v.Str)
		if err != nil {
			return nil, err
		}
		return append(b, s...), nil

	case KindTuple:
		b = append(b, '{')
		for i, it := range v.Items {
			if i > 0 {
				b = append(b, ',')
			}
			name, err := json.Marshal(it.Name)
			if err != nil {
				return nil, err
			}
			b = append(b, name...)
			b = append(b, ':')
			if b, err = it.Value.appendJSON(b); err != nil {
				return nil, err
			}
		}
		return append(b, '}'), nil

	case KindList:
		b = append(b, '[')
		for i, it := range v.Items {
			if i > 0 {
				b = append(b, ',')
			}
			var err error
			if it.Name == "" {
				if b, err = it.Value.appendJSON(b); err != nil {
					return nil, err
				}
				continue
			}
			name, err := json.Marshal(it.Name)
			if err != nil {
				return nil, err
			}
			b = append(b, '{')
			b = append(b, name...)
			b = append(b, ':')
			if b, err = it.Value.appendJSON(b); err != nil {
				return nil, err
			}
			b = append(b, '}')
		}
		return append(b, ']'), nil
	}
	return append(b, "null"...), nil
}

// MarshalJSON renders a result list as a JSON object, matching the tuple form.
func (rs Results) MarshalJSON() ([]byte, error) {
	return Value{Kind: KindTuple, Items: rs}.MarshalJSON()
}

// MI re-renders the value in MI syntax. Its purpose is the round-trip test:
// parse -> MI -> parse must be a fixed point, which is the cheapest way to know
// the parser and the unescaper agree about every byte.
func (v Value) MI() string {
	var sb strings.Builder
	v.appendMI(&sb)
	return sb.String()
}

func (v Value) appendMI(sb *strings.Builder) {
	switch v.Kind {
	case KindConst:
		sb.WriteByte('"')
		writeEscaped(sb, v.Str)
		sb.WriteByte('"')
	case KindTuple, KindList:
		open, close := byte('{'), byte('}')
		if v.Kind == KindList {
			open, close = '[', ']'
		}
		sb.WriteByte(open)
		for i, it := range v.Items {
			if i > 0 {
				sb.WriteByte(',')
			}
			if it.Name != "" {
				sb.WriteString(it.Name)
				sb.WriteByte('=')
			}
			it.Value.appendMI(sb)
		}
		sb.WriteByte(close)
	}
}

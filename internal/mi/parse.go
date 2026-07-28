package mi

import (
	"errors"
	"strconv"
	"strings"
)

// Errors returned in Record.Err. They are diagnostic only: ParseRecord never
// fails, it downgrades to RecGarbage.
var (
	errUnterminatedString = errors.New("mi: unterminated c-string")
	errExpectedValue      = errors.New("mi: expected a value")
	errExpectedEquals     = errors.New("mi: expected '=' after result name")
	errTrailingInput      = errors.New("mi: trailing input after record")
	errEmptyClass         = errors.New("mi: empty result class")
	errUnclosedGroup      = errors.New("mi: unclosed tuple or list")
)

const promptText = "(gdb)"

// ParseRecord parses one line of gdb's MI output.
//
// It never returns an error and never panics: a line it cannot understand comes
// back as RecGarbage carrying the original text, because the alternative — an
// error path — would mean discarding inferior output that happens to share the
// stream (see RecGarbage). Callers switch on Type; they do not check errors.
func ParseRecord(line string) Record {
	raw := strings.TrimRight(line, "\r\n")

	// gdb sometimes writes the prompt without a trailing newline, gluing it to
	// the front of the next record. Strip any number of leading prompts before
	// deciding what the line is.
	s := raw
	stripped := false
	for {
		t := strings.TrimLeft(s, " \t")
		if !strings.HasPrefix(t, promptText) {
			break
		}
		s = t[len(promptText):]
		stripped = true
	}
	if stripped {
		s = strings.TrimLeft(s, " \t")
		if s == "" {
			return Record{Type: RecPrompt, Raw: raw}
		}
	}

	p := &parser{s: s}
	tok, hasTok := p.token()
	if p.eof() {
		return garbage(raw)
	}

	var typ Type
	switch p.s[p.i] {
	case '^':
		typ = RecResult
	case '*':
		typ = RecExec
	case '=':
		typ = RecNotify
	case '+':
		typ = RecStatus
	case '~':
		typ = RecConsole
	case '@':
		typ = RecTarget
	case '&':
		typ = RecLog
	default:
		return garbage(raw)
	}
	p.i++

	rec := Record{Type: typ, Token: tok, HasToken: hasTok, Raw: raw}

	if rec.IsStream() {
		text, err := p.cstring()
		if err != nil {
			return malformed(raw, err)
		}
		if !p.eof() {
			// Stream records carry exactly one string; anything after it means
			// we misread the line.
			return malformed(raw, errTrailingInput)
		}
		rec.Text = text
		return rec
	}

	class := p.class()
	if class == "" {
		return malformed(raw, errEmptyClass)
	}
	rec.Class = class

	for !p.eof() {
		if p.s[p.i] != ',' {
			return malformed(raw, errTrailingInput)
		}
		p.i++
		res, err := p.result()
		if err != nil {
			return malformed(raw, err)
		}
		rec.Results = append(rec.Results, res)
	}
	return rec
}

func garbage(raw string) Record {
	return Record{Type: RecGarbage, Text: raw, Raw: raw}
}

func malformed(raw string, err error) Record {
	return Record{Type: RecGarbage, Text: raw, Raw: raw, Err: err}
}

type parser struct {
	s string
	i int
}

func (p *parser) eof() bool { return p.i >= len(p.s) }

// token reads the optional leading decimal correlation token.
func (p *parser) token() (uint64, bool) {
	start := p.i
	for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
		p.i++
	}
	if p.i == start {
		return 0, false
	}
	n, err := strconv.ParseUint(p.s[start:p.i], 10, 64)
	if err != nil {
		// Absurdly long digit run; treat it as not-a-token and let the
		// classifier check fail into garbage.
		p.i = start
		return 0, false
	}
	return n, true
}

// class reads a result or async class name.
func (p *parser) class() string {
	start := p.i
	for p.i < len(p.s) && isNameByte(p.s[p.i]) {
		p.i++
	}
	return p.s[start:p.i]
}

// result parses name=value, or a bare value for anonymous list elements.
func (p *parser) result() (Result, error) {
	if p.eof() {
		return Result{}, errExpectedValue
	}
	// A name only counts as a name if an '=' follows it; otherwise this is an
	// anonymous element and the bytes belong to the value.
	if isNameStart(p.s[p.i]) {
		start := p.i
		for p.i < len(p.s) && isNameByte(p.s[p.i]) {
			p.i++
		}
		name := p.s[start:p.i]
		if !p.eof() && p.s[p.i] == '=' {
			p.i++
			v, err := p.value()
			if err != nil {
				return Result{}, err
			}
			return Result{Name: name, Value: v}, nil
		}
		// Not a name after all. A bare identifier is not a valid MI value, so
		// this is malformed rather than an anonymous constant.
		p.i = start
		return Result{}, errExpectedEquals
	}
	v, err := p.value()
	if err != nil {
		return Result{}, err
	}
	return Result{Value: v}, nil
}

func (p *parser) value() (Value, error) {
	if p.eof() {
		return Value{}, errExpectedValue
	}
	switch p.s[p.i] {
	case '"':
		s, err := p.cstring()
		if err != nil {
			return Value{}, err
		}
		return Value{Kind: KindConst, Str: s}, nil
	case '{':
		return p.group('{', '}', KindTuple)
	case '[':
		return p.group('[', ']', KindList)
	}
	return Value{}, errExpectedValue
}

// group parses a tuple or list. The two shapes are parsed identically because
// gdb does not respect the distinction the grammar implies: tuples hold named
// results, lists hold either bare values or named results, and both can be
// empty.
func (p *parser) group(open, close byte, kind Kind) (Value, error) {
	p.i++ // consume open
	v := Value{Kind: kind}
	if p.eof() {
		return Value{}, errUnclosedGroup
	}
	if p.s[p.i] == close {
		p.i++
		return v, nil
	}
	for {
		res, err := p.result()
		if err != nil {
			return Value{}, err
		}
		v.Items = append(v.Items, res)
		if p.eof() {
			return Value{}, errUnclosedGroup
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case close:
			p.i++
			return v, nil
		default:
			return Value{}, errUnclosedGroup
		}
	}
}

// cstring parses a quoted, C-escaped string starting at the opening quote.
func (p *parser) cstring() (string, error) {
	if p.eof() || p.s[p.i] != '"' {
		return "", errExpectedValue
	}
	p.i++
	start := p.i
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case '\\':
			// Skip the escaped byte so an escaped quote does not end the
			// string. Bounds-checked so a line truncated mid-escape — which is
			// what a gdb crash looks like — cannot run off the end.
			p.i += 2
			if p.i > len(p.s) {
				return "", errUnterminatedString
			}
		case '"':
			body := p.s[start:p.i]
			p.i++
			return unescape(body), nil
		default:
			p.i++
		}
	}
	return "", errUnterminatedString
}

func isNameStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
}

func isNameByte(c byte) bool {
	return isNameStart(c) || c >= '0' && c <= '9' || c == '-'
}

func u64s(n uint64) string { return strconv.FormatUint(n, 10) }

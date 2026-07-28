package mi

import (
	"strings"
	"unicode/utf8"
)

// unescape decodes a C string body (the bytes between the quotes) into a Go
// string. It works byte-at-a-time into a byte buffer rather than rune-at-a-time
// because octal escapes are *bytes*: gdb writes a UTF-8 é as \303\251, two
// separate escapes that only mean anything once reassembled. Decoding them
// individually to runes would produce U+00C3 U+00A9 — mojibake that looks like
// a gdb bug and isn't.
//
// The result is forced to valid UTF-8 (invalid bytes become U+FFFD) because
// everything downstream is JSON, and encoding/json silently replaces invalid
// bytes anyway — doing it here means the string in memory matches the string on
// the wire, so a test can assert on either.
func unescape(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return sanitizeUTF8(s)
	}
	buf := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c != '\\' {
			buf = append(buf, c)
			i++
			continue
		}
		i++
		if i >= len(s) {
			// Trailing lone backslash: keep it rather than losing a byte.
			buf = append(buf, '\\')
			break
		}
		switch e := s[i]; e {
		case 'n':
			buf, i = append(buf, '\n'), i+1
		case 'r':
			buf, i = append(buf, '\r'), i+1
		case 't':
			buf, i = append(buf, '\t'), i+1
		case 'a':
			buf, i = append(buf, 0x07), i+1
		case 'b':
			buf, i = append(buf, 0x08), i+1
		case 'f':
			buf, i = append(buf, 0x0c), i+1
		case 'v':
			buf, i = append(buf, 0x0b), i+1
		case 'e':
			buf, i = append(buf, 0x1b), i+1
		case '0', '1', '2', '3', '4', '5', '6', '7':
			// Up to three octal digits, C semantics.
			n, digits := 0, 0
			for digits < 3 && i < len(s) && s[i] >= '0' && s[i] <= '7' {
				n = n*8 + int(s[i]-'0')
				i++
				digits++
			}
			buf = append(buf, byte(n))
		case 'x':
			// gdb emits octal, not hex, but a literal backslash always arrives
			// doubled, so accepting \xHH costs nothing and cannot misfire.
			i++
			n, digits := 0, 0
			for digits < 2 && i < len(s) && isHexDigit(s[i]) {
				n = n*16 + hexVal(s[i])
				i++
				digits++
			}
			if digits == 0 {
				buf = append(buf, 'x')
			} else {
				buf = append(buf, byte(n))
			}
		default:
			// Includes \\ \" \' \? and anything undefined: emit the escaped
			// byte itself.
			buf, i = append(buf, e), i+1
		}
	}
	return sanitizeUTF8(string(buf))
}

// writeEscaped is the inverse of unescape, used by Value.MI. It escapes the
// characters gdb escapes and leaves printable UTF-8 alone.
func writeEscaped(sb *strings.Builder, s string) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		case '\t':
			sb.WriteString(`\t`)
		case 0x07:
			sb.WriteString(`\a`)
		case 0x08:
			sb.WriteString(`\b`)
		case 0x0c:
			sb.WriteString(`\f`)
		case 0x0b:
			sb.WriteString(`\v`)
		default:
			if c < 0x20 || c == 0x7f {
				const octal = "01234567"
				sb.WriteByte('\\')
				sb.WriteByte(octal[(c>>6)&7])
				sb.WriteByte(octal[(c>>3)&7])
				sb.WriteByte(octal[c&7])
				continue
			}
			sb.WriteByte(c)
		}
	}
}

// sanitizeUTF8 replaces invalid UTF-8 with U+FFFD, cheaply in the common case.
func sanitizeUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "�")
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return int(c-'A') + 10
	}
}

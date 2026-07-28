package mi

import (
	"encoding/json"
	"testing"
)

// FuzzParseRecord exists because ParseRecord is a hand-written parser over C
// escaping fed by a stream that provably contains arbitrary bytes (the
// inferior's stdout). Its contract is total: any input produces a Record, never
// a panic, and never a hang.
//
// The seeds are the real corpus plus the malformed shapes a dying gdb produces.
func FuzzParseRecord(f *testing.F) {
	for _, e := range loadCorpus(f) {
		f.Add(e.Line)
	}
	for _, s := range []string{
		"",
		"^",
		"^done,",
		`^done,a=`,
		`~"`,
		`~"\`,
		`^done,s="\303"`,
		`^done,l=[`,
		`^done,t={`,
		"(gdb)",
		"(gdb) (gdb)",
		"12345678901234567890123456789^done",
		`^done,a="\777\000\010"`,
		"\xff\xfe\x00",
		`*stopped,frame={addr="0x0",args=[{name="a",value="1"}]}`,
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, line string) {
		r := ParseRecord(line)

		// Whatever came out must be JSON-encodable: everything the parser
		// produces is destined for a WebSocket, and encoding/json panicking or
		// erroring at that point would take down a live debug session.
		if _, err := json.Marshal(r.Results); err != nil {
			t.Fatalf("results not JSON-encodable: %v (input %q)", err, line)
		}

		// Re-rendering and re-parsing must be stable. This catches escaper and
		// parser disagreeing about a byte, which is exactly the bug class that
		// hand-rolled C-string handling produces.
		if r.Type == RecGarbage {
			return
		}
		again := ParseRecord(r.MI())
		if again.Type != r.Type || again.Class != r.Class || again.Text != r.Text {
			t.Fatalf("round-trip changed the record:\n  in:   %q\n  mi:   %q\n  1: %+v\n  2: %+v",
				line, r.MI(), r, again)
		}
		a, err1 := json.Marshal(r.Results)
		b, err2 := json.Marshal(again.Results)
		if err1 != nil || err2 != nil {
			t.Fatalf("marshal errors: %v %v", err1, err2)
		}
		if string(a) != string(b) {
			t.Fatalf("round-trip changed results:\n  in: %q\n  1:  %s\n  2:  %s", line, a, b)
		}
	})
}

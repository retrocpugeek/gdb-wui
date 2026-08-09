package debugger

import "testing"

// TestSplitFileLine pins what counts as a FILE:LINE.
//
// It is the one piece of the go-to box that decides between two entirely
// different resolutions — a source position or a gdb expression — from the
// shape of a string, so the boundary cases are worth writing down rather than
// discovering.
func TestSplitFileLine(t *testing.T) {
	for _, tc := range []struct {
		in   string
		file string
		line int
		ok   bool
	}{
		{"globals.c:65", "globals.c", 65, true},
		{"src/deep/globals.c:1", "src/deep/globals.c", 1, true},
		{"globals.c: 65", "", 0, false}, // Atoi refuses the space rather than guessing
		// The last colon is the separator, not the first: a path may contain
		// one, and only the tail can be a line number.
		{"odd:name.c:65", "odd:name.c", 65, true},
		// But a file ending in a colon is not a file. This is what "a::65"
		// would otherwise become, and it is a C++ name with a number after it
		// rather than a place in a file.
		{"a::65", "", 0, false},

		// Not locations. Each is something a user will actually type.
		{"main", "", 0, false},
		{"0x401136", "", 0, false},
		{"$pc", "", 0, false},
		{"&head", "", 0, false},
		// A C++ qualified name. The *last* colon is the split point, which
		// leaves "bar" on the right and no number, so "::" survives intact.
		{"Foo::bar", "", 0, false},
		// A line number with nothing in front of it. Ambiguous with a bare
		// address, and the client resolves ":65" against the file on screen
		// without asking the server, so this must not be a location here.
		{":65", "", 0, false},
		{"globals.c:", "", 0, false},
		{"globals.c:0", "", 0, false},
		{"globals.c:-3", "", 0, false},
		{"globals.c:65x", "", 0, false},
		{"", "", 0, false},
	} {
		file, line, ok := splitFileLine(tc.in)
		if ok != tc.ok || file != tc.file || line != tc.line {
			t.Errorf("splitFileLine(%q) = (%q, %d, %v), want (%q, %d, %v)",
				tc.in, file, line, ok, tc.file, tc.line, tc.ok)
		}
	}
}

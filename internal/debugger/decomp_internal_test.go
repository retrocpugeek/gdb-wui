package debugger

import (
	"testing"

	"github.com/retrocpugeek/gdb-wui/internal/ghidra"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// The pure parts of the decompilation bridge, which are the parts that produce
// a confidently wrong answer when they are wrong: an address in the wrong
// coordinate system, a highlight on the wrong line, or an expression that
// reads a plausible but incorrect place on the stack. None of them needs gdb
// or Ghidra to exercise.

// TestVarExprUsesTheMeasuredABIRules pins the two frame-base rules to the
// numbers they were established from — see docs/decompilation.md. Both cases
// are real: taken from build/structs on x86-64 and from vwfw-linux_64 on
// MIPS64, and both were checked against the instruction stream.
func TestVarExprUsesTheMeasuredABIRules(t *testing.T) {
	cases := []struct {
		name    string
		lang    string
		ptr     int
		frame   ghidra.Frame
		v       ghidra.Var
		want    string
		because string
	}{
		{
			name:  "x86-64 stack",
			lang:  "x86:LE:64:default",
			ptr:   8,
			frame: ghidra.Frame{Size: 96},
			v: ghidra.Var{Name: "buf", Type: "char[64]", Size: 64,
				Storage: ghidra.Storage{Kind: ghidra.StorageStack, Offset: -0x58}},
			want: "*(char[64] *)($rbp - 0x50)",
			because: "inspect's buf is at Ghidra -0x58 and the instruction " +
				"stream addresses it as -0x50(%rbp): entry_sp = $rbp + 8",
		},
		{
			name:  "MIPS64 stack, positive result",
			lang:  "MIPS:BE:64:default",
			ptr:   8,
			frame: ghidra.Frame{Size: 352},
			v: ghidra.Var{Name: "local_70", Type: "undefined1 *", Size: 8,
				Storage: ghidra.Storage{Kind: ghidra.StorageStack, Offset: -112}},
			// undefined1 is Ghidra's spelling, not C's, so it is translated:
			// gdb answers "No symbol" for the original.
			want: "*(unsigned char * *)($sp + 0xf0)",
			because: "process_packet opens with daddiu sp,sp,-352 and uses " +
				"240(sp) for this variable: entry_sp = $sp + frame.size",
		},
		{
			name:  "MIPS64 stack, another verified offset",
			lang:  "MIPS:BE:64:default",
			ptr:   8,
			frame: ghidra.Frame{Size: 352},
			v: ghidra.Var{Name: "local_140", Type: "char[2]", Size: 2,
				Storage: ghidra.Storage{Kind: ghidra.StorageStack, Offset: -320}},
			want:    "*(char[2] *)($sp + 0x20)",
			because: "32(sp) appears six times in the instruction stream",
		},
		{
			name:  "register storage",
			lang:  "x86:LE:64:default",
			ptr:   8,
			frame: ghidra.Frame{},
			v: ghidra.Var{Name: "pcVar5",
				Storage: ghidra.Storage{Kind: ghidra.StorageRegister, Register: "RAX"}},
			want:    "$rax",
			because: "gdb spells registers with $ and in lower case",
		},
		{
			name:  "a decompiler temporary has no location at all",
			lang:  "x86:LE:64:default",
			ptr:   8,
			frame: ghidra.Frame{},
			v: ghidra.Var{Name: "lVar1",
				Storage: ghidra.Storage{Kind: ghidra.StorageUnique}},
			want:    "",
			because: "unique storage exists nowhere in the machine",
		},
		{
			// The important negative. A plausible wrong address reads as a
			// value; a blank reads as "not known", which is the truth.
			name:  "an architecture with no established rule gets no guess",
			lang:  "ARM:LE:32:v8",
			ptr:   4,
			frame: ghidra.Frame{Size: 32},
			v: ghidra.Var{Name: "local_8", Type: "int",
				Storage: ghidra.Storage{Kind: ghidra.StorageStack, Offset: -8}},
			want: "",
			because: "a link register means no return address on the stack, so " +
				"neither the x86 nor the MIPS rule applies",
		},
		{
			name:  "a stack variable with no type still gets an expression",
			lang:  "x86:LE:64:default",
			ptr:   8,
			frame: ghidra.Frame{Size: 32},
			v: ghidra.Var{Name: "local_10",
				Storage: ghidra.Storage{Kind: ghidra.StorageStack, Offset: -16}},
			want:    "*(unsigned long *)($rbp - 0x8)",
			because: "an untyped slot of unknown width is read pointer-sized",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := varExpr(c.v, c.frame, c.lang, c.ptr)
			if got != c.want {
				t.Errorf("varExpr = %q, want %q\n  because: %s", got, c.want, c.because)
			}
		})
	}
}

// TestGDBCTypeIsParseable pins the type translation to what gdb 17.1 actually
// accepts, checked at the console:
//
//	p *(config * *)($rbp - 0x58)     -> No symbol "config" in current context.
//	p *(undefined1 * *)($rbp - 0x58) -> No symbol "undefined1" in current context.
//	p *(unsigned long *)($rbp - 0x58) -> 140737488344784
//	p *(char[64] *)($rbp - 0x50)      -> "label=demo count=3..."
//
// Ghidra's type vocabulary is almost entirely its own, so emitting it verbatim
// produces expressions that silently evaluate to nothing.
func TestGDBCTypeIsParseable(t *testing.T) {
	cases := []struct {
		in      string
		size    int
		want    string
		because string
	}{
		{"int", 4, "int", "a real C type passes through"},
		{"char[64]", 64, "char[64]", "gdb parses this array form"},
		{"undefined1", 1, "unsigned char", "Ghidra's undefined1 is a byte"},
		{"undefined4", 4, "unsigned int", ""},
		{"undefined8", 8, "unsigned long", ""},
		{"uint", 4, "unsigned int", ""},
		{"ulong", 8, "unsigned long", ""},
		{"undefined1[16]", 16, "unsigned char[16]", "the array survives the base mapping"},
		{"undefined1 *", 8, "unsigned char *", ""},
		{"char * *", 8, "char * *", "pointer depth is preserved"},
		{
			in: "config *", size: 8, want: "void *",
			because: "gdb has never heard of Ghidra's struct; void * still prints the address",
		},
		{
			in: "config", size: 24, want: "unsigned long",
			because: "an unnameable value is read as bytes rather than not at all",
		},
		{
			in: "undefined1 * *", size: 8, want: "unsigned char * *", because: "",
		},
		{"", 4, "unsigned int", "no type at all still has a width"},
		{"", 0, "unsigned long", "and an unknown width falls back to pointer-sized"},
	}
	for _, c := range cases {
		got := gdbCType(c.in, c.size)
		if got != c.want {
			t.Errorf("gdbCType(%q, %d) = %q, want %q%s",
				c.in, c.size, got, c.want,
				map[bool]string{true: "\n  because: " + c.because}[c.because != ""])
		}
	}
}

// TestPCLineTieBreak. On optimised code about one address in five is claimed
// by two decompiled lines, so "which line is the pc on" needs a rule and the
// ambiguity has to be reported rather than hidden.
func TestPCLineTieBreak(t *testing.T) {
	lines := []wire.DecompLine{
		{N: 10, Addrs: []string{"0x117a", "0x1190", "0x1198"}},
		{N: 11, Addrs: []string{"0x1188"}},
		{N: 14, Addrs: []string{"0x1258"}},
		{N: 15, Addrs: []string{"0x1258"}},
	}

	if n, amb, ap := pcLine(lines, "0x1188"); n != 11 || amb || ap {
		t.Errorf("exact address gave (%d, %v, %v), want (11, false, false)", n, amb, ap)
	}
	// A loop's increment belongs to the loop header, not to the body between.
	if n, amb, ap := pcLine(lines, "0x1190"); n != 10 || amb || ap {
		t.Errorf("loop increment gave (%d, %v, %v), want (10, false, false)", n, amb, ap)
	}
	// Two lines claim 0x1258: lowest wins, and the caller is told.
	n, amb, ap := pcLine(lines, "0x1258")
	if n != 14 {
		t.Errorf("shared address gave line %d, want the lowest (14)", n)
	}
	if !amb {
		t.Error("shared address did not report ambiguity; hiding it is a lie")
	}
	if ap {
		t.Error("an exactly-claimed address was reported as approximate")
	}

	// An address no line claims — a prologue, a spill, an epilogue — falls back
	// to the nearest preceding line and says so. Reporting nothing there is
	// accurate and useless: it makes the marker blink out mid-step.
	// The greatest mapped address below 0x1248 is 0x1198, on line 10 — not
	// line 11's 0x1188, which is lower. "Nearest" means nearest by address,
	// not by line number, and the loop header legitimately owns the tail.
	n, amb, ap = pcLine(lines, "0x1248")
	if n != 10 {
		t.Errorf("unclaimed 0x1248 gave line %d, want 10 (nearest below is 0x1198)", n)
	}
	if !ap {
		t.Error("a fallback was not flagged approximate; that asserts a guess as fact")
	}
	if amb {
		t.Error("a fallback was flagged ambiguous")
	}

	// Below everything mapped, there is genuinely no answer.
	if n, _, _ := pcLine(lines, "0x1000"); n != 0 {
		t.Errorf("address below the whole map gave line %d, want 0", n)
	}
	if n, _, _ := pcLine(lines, ""); n != 0 {
		t.Errorf("empty pc gave line %d, want 0", n)
	}
}

// TestShiftAddr covers the coordinate change. Getting this wrong points the
// whole pane at addresses that do not exist in the running program.
func TestShiftAddr(t *testing.T) {
	// The measured PIE case: Ghidra 0x1011e9, gdb 0x5555555551e9.
	const bias = 0x555555454000
	if got := shiftAddr("0x1011e9", bias); got != "0x5555555551e9" {
		t.Errorf("shiftAddr = %s, want 0x5555555551e9", got)
	}
	// Firmware: a static EXEC loaded where it was linked needs no shift.
	if got := shiftAddr("0x120007ee0", 0); got != "0x120007ee0" {
		t.Errorf("zero bias changed the address: %s", got)
	}
	// Negative bias is the reverse direction and must not wrap.
	if got := shiftAddr("0x5555555551e9", -bias); got != "0x1011e9" {
		t.Errorf("negative bias = %s, want 0x1011e9", got)
	}
	if got := shiftAddr("", bias); got != "" {
		t.Errorf("empty address became %q", got)
	}
	// Unparseable input is passed through rather than silently zeroed.
	if got := shiftAddr("not-an-address", bias); got != "not-an-address" {
		t.Errorf("unparseable address became %q", got)
	}
}

// TestPlausibleSymbol. Ghidra names an unnamed function after its address, and
// gdb has never heard of those. Asking about one is a wasted round trip per
// candidate, and there are 1415 candidates in the firmware.
func TestPlausibleSymbol(t *testing.T) {
	for _, name := range []string{"main", "process_packet", "csum16", "_start"} {
		if !plausibleSymbol(name) {
			t.Errorf("%q rejected; gdb may well know it", name)
		}
	}
	for _, name := range []string{"FUN_00101020", "LAB_00102942", "DAT_0010a000",
		"thunk_FUN_001028c0", ""} {
		if plausibleSymbol(name) {
			t.Errorf("%q accepted; it is a Ghidra invention gdb cannot resolve", name)
		}
	}
}

// TestStorageKindCollapse. A client can act on three cases, and a decompiler
// temporary must arrive as a row with no value rather than not arriving: a
// blank is honest, a missing row is not.
func TestStorageKindCollapse(t *testing.T) {
	cases := map[string]string{
		ghidra.StorageStack:    wire.DecompStorageStack,
		ghidra.StorageRegister: wire.DecompStorageRegister,
		ghidra.StorageUnique:   wire.DecompStorageNone,
		ghidra.StorageOther:    wire.DecompStorageNone,
		"":                     wire.DecompStorageNone,
	}
	for in, want := range cases {
		if got := storageKind(in); got != want {
			t.Errorf("storageKind(%q) = %q, want %q", in, got, want)
		}
	}
}

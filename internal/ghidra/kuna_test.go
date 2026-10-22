package ghidra

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// The document is kuna_wasm's inspect of main in a stripped aarch64 busybox,
// trimmed to the fields kunaToFunction reads.
func TestKunaToFunction(t *testing.T) {
	raw, err := os.ReadFile("testdata/kuna-inspect-aarch64-main.json")
	if err != nil {
		t.Fatal(err)
	}
	var in kunaInspect
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	fn := kunaToFunction(&in, map[string]uint64{})
	if fn.Name != "main" || fn.Entry != "0xd620" || fn.BodyStart != "0xd620" || fn.BodyEnd != "0xd68f" {
		t.Errorf("function %s at %s, body %s-%s", fn.Name, fn.Entry, fn.BodyStart, fn.BodyEnd)
	}
	if fn.Signature != "int main(int argc,char **argv,char **envp)" {
		t.Errorf("signature %q", fn.Signature)
	}
	// Named by kuna, not by the file: a stripped busybox has no main.
	if fn.Source != SourceAnalysis {
		t.Errorf("source %s", fn.Source)
	}
	if fn.Frame.SPDepth == nil || *fn.Frame.SPDepth != -32 {
		t.Errorf("frame %+v", fn.Frame)
	}
	if fn.LineCount != 17 || len(fn.Lines) == 0 || fn.Lines[0].N != 5 {
		t.Errorf("%d lines, mappings %+v", fn.LineCount, fn.Lines)
	}
	want := []Var{
		{Name: "argc", Type: "int", Size: 4, Param: true, PC: "0xd620",
			Storage: Storage{Kind: StorageRegister, Register: "w0"}},
		{Name: "argv", Type: "char **", Size: 8, Param: true, PC: "0xd620",
			Storage: Storage{Kind: StorageRegister, Register: "x1"}},
		{Name: "envp", Type: "char **", Size: 8, Param: true, PC: "0xd620",
			Storage: Storage{Kind: StorageRegister, Register: "x2"}},
		// Its first use, not the entry: x0 is argc until then.
		{Name: "v1", Type: "char *", Size: 8, PC: "0xd654",
			Storage: Storage{Kind: StorageRegister, Register: "x0"}},
	}
	if !reflect.DeepEqual(fn.Variables, want) {
		t.Errorf("variables\n got %+v\nwant %+v", fn.Variables, want)
	}
	wantGlobals := []Global{
		{Name: "dat_c7270", Type: "unsigned long", Size: 8, Address: "0xc7270"},
		{Name: "dat_c78c8", Type: "unsigned int", Size: 4, Address: "0xc78c8"},
	}
	if !reflect.DeepEqual(fn.Globals, wantGlobals) {
		t.Errorf("globals %+v", fn.Globals)
	}
	// The file's own name for it is a symbol, not an inference.
	if fn := kunaToFunction(&in, map[string]uint64{"main": 0xd620}); fn.Source != SourceImported {
		t.Errorf("with a symbol, source %s", fn.Source)
	}
}

func TestKunaDeclarations(t *testing.T) {
	code := "void f(void)\n{\n" +
		"  long v1; // a4 | a5 | stack - 0x58\n" +
		"  char *v2; // stack - 0x30\n" +
		"  unsigned long v3; // stack + 0x8\n" +
		"  void *v4; // tmp\n" +
		"  long v6; // a0 | a5\n" +
		"  long v7; // stack - 0x8 | stack - 0x10 | a0\n" +
		"  unsigned char v5[16]; // s2\n" +
		"  \n" +
		"  sub_fc70(2,v1,v2); // tail-call\n}"
	in := &kunaInspect{}
	in.Target.Processor, in.Target.Bits = "RISCV", 64
	in.Function = kunaFunction{Name: "f", Address: 0x1000, Size: 0x20, Code: &code}
	fn := kunaToFunction(in, nil)
	got := map[string]Storage{}
	for _, v := range fn.Variables {
		got[v.Name] = v.Storage
	}
	want := map[string]Storage{
		// Merged across three places: the stack slot is its home.
		"v1": {Kind: StorageStack, Offset: -0x58},
		"v2": {Kind: StorageStack, Offset: -0x30},
		"v3": {Kind: StorageStack, Offset: 8},
		"v4": {Kind: StorageOther, Text: "tmp"},
		"v5": {Kind: StorageRegister, Register: "s2"},
		// Two registers, or two slots: nothing to pick.
		"v6": {Kind: StorageOther, Text: "a0 | a5"},
		"v7": {Kind: StorageOther, Text: "stack - 0x8 | stack - 0x10 | a0"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("storage\n got %+v\nwant %+v", got, want)
	}
	if fn.Variables[6].Type != "unsigned char[16]" {
		t.Errorf("array type %q", fn.Variables[6].Type)
	}
}

func TestKunaParams(t *testing.T) {
	for sig, want := range map[string][]kunaParam{
		"int main(int argc,char **argv,char **envp)": {{"int", "argc"}, {"char **", "argv"}, {"char **", "envp"}},
		"void sub_ce70(unsigned long a0)":            {{"unsigned long", "a0"}},
		"int f(void)":                                nil,
		"int printf(char *fmt,...)":                  {{"char *", "fmt"}},
		"void g(void (*cb)(int),long n)":             {{"long", "n"}},
	} {
		if got := kunaParams(sig); !reflect.DeepEqual(got, want) {
			t.Errorf("kunaParams(%q) = %+v, want %+v", sig, got, want)
		}
	}
}

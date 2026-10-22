package ghidra

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A small export as ExportDecomp writes it: main (decompiled), helper
// (decompiled, its body split in two around a piece of main's), a thunk, and
// one function the decompiler refused.
const testExport = `{
  "schema": 2,
  "generator": {"tool": "ghidra", "version": "12.1.3", "script": "ExportDecomp"},
  "program": {"name": "demo", "path": "/x/demo", "format": "ELF", "sha256": "ABCDEF0123", "languageId": "RISCV:LE:64:RV64GC", "compilerSpec": "gcc", "pointerSize": 8, "imageBase": "0x100000"},
  "index": [
    {"name":"puts","entry":"0x100500","thunk":true,"signature":"int puts(char * s)","ranges":[["0x100500","0x10050f"]]},
    {"name":"main","entry":"0x101000","thunk":false,"signature":"int main(void)","ranges":[["0x101000","0x10103f"],["0x101080","0x10108f"]]},
    {"name":"helper","entry":"0x101040","thunk":false,"signature":"void helper(int x)","ranges":[["0x101040","0x10107f"]]},
    {"name":"FUN_00102000","entry":"0x102000","thunk":false,"signature":"undefined FUN_00102000()","ranges":[["0x102000","0x10200f"]]}
  ],
  "data": [
    {"name":"counter","address":"0x104000","type":"int","length":4},
    {"name":"DAT_00104010","address":"0x104010","type":"","length":0}
  ],
  "functions": [
    {"name":"main","entry":"0x101000","bodyStart":"0x101000","bodyEnd":"0x10108f","signature":"int main(void)","lineCount":1,"text":"int main(void)\n{\n  helper(1);\n}\n"},
    {"name":"helper","entry":"0x101040","bodyStart":"0x101040","bodyEnd":"0x10107f","signature":"void helper(int x)","lineCount":1,"text":"void helper(int x)\n{\n}\n"}
  ]
}
`

func startTestExport(t *testing.T) (*Client, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "demo.json")
	if err := os.WriteFile(path, []byte(testExport), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := StartExport(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, dir
}

func TestExportGreeting(t *testing.T) {
	c, _ := startTestExport(t)
	r := c.Ready()
	if r.Schema != 2 || r.Program.Name != "demo" || r.Program.SHA256 != "ABCDEF0123" || r.FunctionCount != 4 {
		t.Fatalf("greeting: %+v", r)
	}
}

func TestExportDecompileByAddressAndName(t *testing.T) {
	c, _ := startTestExport(t)
	ctx := context.Background()
	for which, want := range map[string]string{
		"0x101000": "main",   // an entry
		"0x101010": "main",   // inside
		"0x101088": "main",   // main's second range, after helper's start
		"0x101050": "helper", // inside helper, between main's ranges
		"helper":   "helper", // by name
	} {
		fn, err := c.Decompile(ctx, which)
		if err != nil {
			t.Fatalf("Decompile(%s): %v", which, err)
		}
		if fn.Name != want || !strings.Contains(fn.Text, want+"(") {
			t.Errorf("Decompile(%s) = %s %q, want %s", which, fn.Name, fn.Text, want)
		}
	}
	// In the program but not in the export: say which and why
	for which, want := range map[string]string{
		"0x100504":     "thunk",
		"FUN_00102000": "not decompiled",
		"0x103000":     "no function",
		"nonesuch":     "no function",
	} {
		_, err := c.Decompile(ctx, which)
		var ge *Error
		if !errors.As(err, &ge) || !strings.Contains(ge.Msg, want) {
			t.Errorf("Decompile(%s): %v, want an error saying %q", which, err, want)
		}
	}
}

func TestExportLists(t *testing.T) {
	c, _ := startTestExport(t)
	ctx := context.Background()
	fl, err := c.Functions(ctx, 0, 500, "")
	if err != nil {
		t.Fatal(err)
	}
	if fl.Total != 4 || len(fl.Functions) != 4 || fl.Functions[0].Name != "puts" || !fl.Functions[0].Thunk {
		t.Errorf("functions: %+v", fl)
	}
	fl, err = c.Functions(ctx, 1, 1, "")
	if err != nil || fl.Total != 4 || fl.Offset != 1 || len(fl.Functions) != 1 || fl.Functions[0].Name != "main" {
		t.Errorf("functions, second page: %+v %v", fl, err)
	}
	fl, err = c.Functions(ctx, 0, 500, "HELP")
	if err != nil || fl.Total != 1 || fl.Functions[0].Name != "helper" {
		t.Errorf("functions filtered: %+v %v", fl, err)
	}
	dl, err := c.Data(ctx, 0, 500, "count")
	if err != nil || dl.Total != 1 || dl.Data[0].Name != "counter" || dl.Data[0].Length != 4 {
		t.Errorf("data: %+v %v", dl, err)
	}
}

func TestExportNames(t *testing.T) {
	c, _ := startTestExport(t)
	names, err := c.Names(context.Background(), []string{"0x101088", "0x100508", "0x103000"})
	if err != nil {
		t.Fatal(err)
	}
	// The address in no function is left out, as the server does
	if len(names) != 2 || names[0].Name != "main" || names[0].Addr != "0x101088" ||
		names[1].Name != "puts" || !names[1].Thunk || names[1].Signature != "int puts(char * s)" {
		t.Errorf("names: %+v", names)
	}
}

func TestExportIsReadOnly(t *testing.T) {
	c, _ := startTestExport(t)
	_, err := c.Rename(context.Background(), Edit{Function: "0x101000", Kind: "function", Name: "main", Value: "start"})
	var ge *Error
	if !errors.As(err, &ge) || !strings.Contains(ge.Msg, "cannot be changed") {
		t.Errorf("rename: %v", err)
	}
}

func TestFindExport(t *testing.T) {
	_, dir := startTestExport(t)
	if err := os.WriteFile(filepath.Join(dir, "notes.json"), []byte(`{"other": 1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := FindExport(dir, "abcdef0123")
	if err != nil || filepath.Base(got) != "demo.json" {
		t.Errorf("FindExport = %q, %v", got, err)
	}
	if got, _ := FindExport(dir, "0000"); got != "" {
		t.Errorf("FindExport of an unknown hash = %q", got)
	}
}

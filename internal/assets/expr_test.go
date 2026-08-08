package assets_test

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The hover evaluator's parser, driven through node.
//
// This is the one piece of the frontend with logic rather than wiring, and it
// is the piece that decides what gets sent to gdb. Getting it wrong is not a
// cosmetic bug: `expressionAt` is what stands between a mouse drifting across
// source and gdb being asked to *call a function*, which is a thing a pointer
// must never do by accident.
//
// core/expr.js imports nothing and touches no DOM precisely so this test can
// exist without a browser or a test framework.

// runExpr evaluates a snippet against js/core/expr.js and returns what it
// printed, decoded from JSON.
func runExpr(t *testing.T, body string, out any) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; skipping the expression tests")
	}
	mod := filepath.Join(repoRoot(t), "internal", "assets", "web", "js", "core", "expr.js")
	script := "import * as expr from " + jsString(mod) + ";\n" + body

	cmd := exec.Command(node, "--input-type=module", "-")
	cmd.Stdin = strings.NewReader(script)
	stdout, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("node failed: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("running node: %v", err)
	}
	if err := json.Unmarshal(stdout, out); err != nil {
		t.Fatalf("decoding node's output %q: %v", stdout, err)
	}
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestExpressionAt covers the postfix-chain grammar. The offset in each case is
// marked with a caret in the want-column comment rather than being computed, so
// a reader can see what is being hovered.
func TestExpressionAt(t *testing.T) {
	cases := []struct {
		Name   string `json:"name"`
		Line   string `json:"line"`
		Offset int    `json:"offset"`
		Want   string `json:"want"`
	}{
		{"plain identifier", "  int count = 0;", 7, "count"},
		{"first character", "count = 0;", 0, "count"},
		// The caret offset is an insertion point, so the far edge of a word
		// reports the position after it. Losing the last character of every
		// identifier would be a bug nobody could describe.
		{"past the last character", "count;", 5, "count"},
		{"member access", "  cfg.count = 1;", 7, "cfg.count"},
		{"member access on the base", "  cfg.count = 1;", 3, "cfg"},
		{"arrow", "  p->next = 0;", 6, "p->next"},
		{"chained", "a.b->c.d;", 7, "a.b->c.d"},
		{"subscript in the middle", "cfg.items[2].name;", 13, "cfg.items[2].name"},
		{"nested subscripts", "grid[1][2].v;", 11, "grid[1][2].v"},

		// Nothing to ask about.
		{"whitespace", "  count;", 0, ""},
		// Stepping back one character is bounded: it only rescues the far edge
		// of a word, and does not make free-standing punctuation hoverable.
		{"punctuation", "  x = a;", 4, ""},
		{"keyword", "  return x;", 3, ""},
		{"type name", "  int x;", 3, ""},
		{"decimal literal", "  x = 42;", 7, ""},
		{"hex literal", "  x = 0x1f;", 8, ""},
		// `1.5` must not read as member access on a struct named 1.
		{"float literal", "  x = 1.5;", 9, ""},

		// The safety property. A subscript containing a call stops the chain
		// rather than handing gdb something it would run.
		{"call in a subscript", "a[f(1)].b;", 8, "b"},
		{"call to the left", "f(x).b;", 5, "b"},

		// gdb's own spellings survive: `$rax` and `$1` are expressions.
		{"convenience variable", "  $rax + 1", 3, "$rax"},
	}

	// One node process for the whole table: starting node costs far more than
	// the parsing does.
	var body strings.Builder
	body.WriteString("const cases = ")
	enc, err := json.Marshal(cases)
	if err != nil {
		t.Fatal(err)
	}
	body.Write(enc)
	body.WriteString(`;
const out = cases.map((c) => {
  const got = expr.expressionAt(c.line, c.offset);
  return got ? got.expr : "";
});
console.log(JSON.stringify(out));
`)

	var got []string
	runExpr(t, body.String(), &got)
	if len(got) != len(cases) {
		t.Fatalf("got %d results for %d cases", len(got), len(cases))
	}
	for i, c := range cases {
		if got[i] != c.Want {
			t.Errorf("%s: expressionAt(%q, %d) = %q, want %q",
				c.Name, c.Line, c.Offset, got[i], c.Want)
		}
	}
}

// TestExpressionAtSpansTheChain checks the reported span, which is what the
// tooltip is anchored to. A span covering only the hovered word would point the
// tooltip at `name` while describing `cfg.items[2].name`.
func TestExpressionAtSpansTheChain(t *testing.T) {
	var got struct {
		Expr  string `json:"expr"`
		Start int    `json:"start"`
		End   int    `json:"end"`
	}
	runExpr(t, `
const line = "  cfg.items[2].name = 0;";
console.log(JSON.stringify(expr.expressionAt(line, 17)));
`, &got)

	const line = "  cfg.items[2].name = 0;"
	if got.Expr != "cfg.items[2].name" {
		t.Errorf("expr = %q", got.Expr)
	}
	if line[got.Start:got.End] != got.Expr {
		t.Errorf("span [%d,%d) is %q, which is not the expression %q",
			got.Start, got.End, line[got.Start:got.End], got.Expr)
	}
}

// TestOperandExpressions covers the disassembly side: turning a printed operand
// into the way gdb spells the same thing.
func TestOperandExpressions(t *testing.T) {
	var got map[string]string
	runExpr(t, `
const out = {
  "reg:%rax":        expr.registerExpr("%rax"),
  "reg:%eax":        expr.registerExpr("%eax"),
  "reg:%r13d":       expr.registerExpr("%r13d"),
  "reg:not-a-reg":   expr.registerExpr("0x10"),
  "reg:immediate":   expr.registerExpr("$0x10"),
  "bare:r0":         expr.bareRegisterExpr("r0"),
  "bare:sp":         expr.bareRegisterExpr("sp"),
  "bare:toolong":    expr.bareRegisterExpr("movprfx"),
  "bare:punct":      expr.bareRegisterExpr(","),
  "sym:<add>":       expr.symbolExpr("<add>"),
  "sym:<add+4>":     expr.symbolExpr("<add+4>"),
  "sym:<snprintf@plt>": expr.symbolExpr("<snprintf@plt>"),
  "sym:<f@plt+6>":   expr.symbolExpr("<f@plt+6>"),
  "sym:<a.cold>":    expr.symbolExpr("<a.cold>"),
  "sym:bare":        expr.symbolExpr("add"),
};
console.log(JSON.stringify(out));
`, &got)

	want := map[string]string{
		"reg:%rax":      "$rax",
		"reg:%eax":      "$eax",
		"reg:%r13d":     "$r13d",
		"reg:not-a-reg": "",
		"reg:immediate": "",
		"bare:r0":       "$r0",
		"bare:sp":       "$sp",
		"bare:toolong":  "",
		"bare:punct":    "",
		"sym:<add>":     "add",
		"sym:<add+4>":   "add",
		// The PLT trampoline's name is not an expression: `@` is gdb's
		// artificial-array operator, so the name in front of it is what gets
		// asked about.
		"sym:<snprintf@plt>": "snprintf",
		"sym:<f@plt+6>":      "f",
		"sym:<a.cold>":       "a.cold",
		"sym:bare":           "",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
}

// TestAlternateBase guards the reason this uses BigInt. A register value is a
// 64-bit quantity, and float64 silently rounds the top of one: 0x7ffffffde000
// through Number is fine, but a pointer with bits set above 2^53 is not, and a
// tooltip would then show an address that is wrong in a way nobody notices.
func TestAlternateBase(t *testing.T) {
	var got map[string]string
	runExpr(t, `
const out = {
  "140737488347136":       expr.alternateBase("140737488347136"),
  "18446744073709551615":  expr.alternateBase("18446744073709551615"),
  "-4096":                 expr.alternateBase("-4096"),
  "7":                     expr.alternateBase("7"),
  "0x1f":                  expr.alternateBase("0x1f"),
  "struct":                expr.alternateBase("{count = 3, label = 0x4006 \"demo\"}"),
  "empty":                 expr.alternateBase(""),
};
console.log(JSON.stringify(out));
`, &got)

	want := map[string]string{
		"140737488347136":      "0x7fffffffe000",
		"18446744073709551615": "0xffffffffffffffff",
		"-4096":                "-0x1000",
		// Below ten there is nothing to convert.
		"7": "",
		// Already hex, or not a number at all.
		"0x1f":   "",
		"struct": "",
		"empty":  "",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("alternateBase(%s) = %q, want %q", k, got[k], w)
		}
	}
}

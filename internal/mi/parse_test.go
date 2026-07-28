package mi

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/records.golden.json")

const goldenPath = "testdata/records.golden.json"

// goldenRecord is the reviewable JSON projection of a parsed record.
type goldenRecord struct {
	Name    string          `json:"name"`
	Raw     string          `json:"raw"`
	Type    string          `json:"type"`
	Token   *uint64         `json:"token,omitempty"`
	Class   string          `json:"class,omitempty"`
	Text    string          `json:"text,omitempty"`
	Results json.RawMessage `json:"results,omitempty"`
	Err     string          `json:"err,omitempty"`
}

func project(e corpusEntry) goldenRecord {
	r := e.parse()
	g := goldenRecord{
		Name:  e.Name,
		Raw:   r.Raw,
		Type:  r.Type.String(),
		Class: r.Class,
		Text:  r.Text,
	}
	if r.HasToken {
		tok := r.Token
		g.Token = &tok
	}
	if len(r.Results) > 0 {
		b, err := json.Marshal(r.Results)
		if err != nil {
			panic(err)
		}
		g.Results = json.RawMessage(b)
	}
	if r.Err != nil {
		g.Err = r.Err.Error()
	}
	return g
}

// TestCorpusGolden is the parser's main regression net: every captured record,
// rendered to canonical JSON, compared against a reviewed file. Run with
// -update to rewrite it, then read the diff — that diff is the review.
func TestCorpusGolden(t *testing.T) {
	corpus := loadCorpus(t)
	got := make([]goldenRecord, 0, len(corpus))
	for _, e := range corpus {
		got = append(got, project(e))
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(got); err != nil {
		t.Fatalf("encode: %v", err)
	}

	if *update {
		if err := os.WriteFile(goldenPath, buf.Bytes(), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("wrote %s (%d records)", goldenPath, len(got))
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(buf.Bytes())) {
		var wantRecs []goldenRecord
		if err := json.Unmarshal(want, &wantRecs); err != nil {
			t.Fatalf("golden file is not valid JSON: %v", err)
		}
		byName := map[string]goldenRecord{}
		for _, w := range wantRecs {
			byName[w.Name] = w
		}
		for _, g := range got {
			w, ok := byName[g.Name]
			if !ok {
				t.Errorf("%s: new record not in golden", g.Name)
				continue
			}
			if !sameGolden(g, w) {
				t.Errorf("%s:\n  got  %s\n  want %s", g.Name, mustJSON(g), mustJSON(w))
			}
			delete(byName, g.Name)
		}
		for name := range byName {
			t.Errorf("%s: golden record no longer produced by the corpus", name)
		}
		t.Errorf("golden mismatch; rerun with -update and review the diff")
	}
}

func sameGolden(a, b goldenRecord) bool { return string(mustJSON(a)) == string(mustJSON(b)) }

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// TestCorpusNeverPanicsOrErrors checks the two invariants every caller relies
// on: parsing terminates without panicking, and only lines that are genuinely
// not MI come back as garbage.
func TestCorpusNeverPanicsOrErrors(t *testing.T) {
	expectGarbage := map[string]bool{"garbage-inferior-line": true}
	for _, e := range loadCorpus(t) {
		r := e.parse()
		if r.Err != nil {
			t.Errorf("%s: unexpected parse error: %v (line %q)", e.Name, r.Err, e.Line)
		}
		if got := r.Type == RecGarbage; got != expectGarbage[e.Name] {
			t.Errorf("%s: garbage=%v, want %v", e.Name, got, expectGarbage[e.Name])
		}
	}
}

// TestRoundTrip asserts parse -> MI -> parse is a fixed point. It is the
// cheapest way to know the parser and the escaper agree about every byte, and
// it is what makes the fuzz target meaningful.
func TestRoundTrip(t *testing.T) {
	for _, e := range loadCorpus(t) {
		first := e.parse()
		second := ParseRecord(first.MI())
		if first.Type != second.Type || first.Class != second.Class ||
			first.Text != second.Text || first.HasToken != second.HasToken ||
			first.Token != second.Token {
			t.Errorf("%s: header changed across round-trip:\n  1: %+v\n  2: %+v",
				e.Name, first, second)
			continue
		}
		if a, b := string(mustJSON(first.Results)), string(mustJSON(second.Results)); a != b {
			t.Errorf("%s: results changed across round-trip:\n  1: %s\n  2: %s", e.Name, a, b)
		}
	}
}

// TestDuplicateKeysPreserved is finding 8: a map-based parser silently keeps
// only the last frame. Every stack listing in the UI depends on this not
// happening.
func TestDuplicateKeysPreserved(t *testing.T) {
	r := corpusRecord(t, "result-stack-frames")
	stack, ok := r.Results.Get("stack")
	if !ok {
		t.Fatal("no stack result")
	}
	frames := stack.All("frame")
	if len(frames) < 2 {
		t.Fatalf("got %d frames, want at least 2 — duplicate keys were dropped", len(frames))
	}
	for i, f := range frames {
		if lvl, ok := f.Int("level"); !ok || lvl != i {
			t.Errorf("frame %d has level %v (ok=%v), want %d", i, lvl, ok, i)
		}
	}

	// The same shape via a differently-named repeated key.
	bt := corpusRecord(t, "result-breakpoint-table")
	table, ok := bt.Results.Get("BreakpointTable")
	if !ok {
		t.Fatal("no BreakpointTable")
	}
	body, ok := table.Get("body")
	if !ok {
		t.Fatal("no body")
	}
	if n := len(body.All("bkpt")); n < 2 {
		t.Errorf("got %d bkpt entries, want at least 2", n)
	}
}

// TestEmptyStringsInList is finding 7: register numbers are indices into a list
// that contains empty names, so the empties must survive parsing or every
// register after the first gap is mislabelled.
func TestEmptyStringsInList(t *testing.T) {
	r := corpusRecord(t, "result-register-names")
	names, ok := r.Results.List("register-names")
	if !ok {
		t.Fatal("no register-names list")
	}
	var empties int
	for _, n := range names {
		if n.Kind != KindConst {
			t.Fatalf("register name is %s, want const", n.Kind)
		}
		if n.Str == "" {
			empties++
		}
	}
	if empties == 0 {
		t.Error("no empty register names survived parsing; indices will be wrong")
	}
	if got := names[0].Str; got != "rax" {
		t.Errorf("register 0 is %q, want rax", got)
	}
}

func TestEscapedQuotesInErrorMessage(t *testing.T) {
	r := corpusRecord(t, "result-error-escaped-quotes")
	if !r.IsError() {
		t.Fatalf("type=%v class=%q, want a ^error", r.Type, r.Class)
	}
	if got, want := r.ErrorMessage(), `Function "main" not defined.`; got != want {
		t.Errorf("msg = %q, want %q", got, want)
	}
}

func TestErrorCode(t *testing.T) {
	r := corpusRecord(t, "result-error-code")
	if got, want := r.ErrorCode(), "undefined-command"; got != want {
		t.Errorf("code = %q, want %q", got, want)
	}
}

// TestGarbageLine is finding 3: inferior stdout lands in the MI stream. It must
// come back as garbage with no error, because it is not a parse failure — it is
// the debuggee talking.
func TestGarbageLine(t *testing.T) {
	r := corpusRecord(t, "garbage-inferior-line")
	if r.Type != RecGarbage {
		t.Fatalf("type = %v, want garbage", r.Type)
	}
	if r.Err != nil {
		t.Errorf("Err = %v, want nil: inferior output is not a parse failure", r.Err)
	}
	if r.Text != "total=3 argc=1" {
		t.Errorf("Text = %q, want the line verbatim", r.Text)
	}
}

// TestPromptGluedToRecord covers gdb writing "(gdb) " with no newline, which
// concatenates the prompt onto the front of the next record.
func TestPromptGluedToRecord(t *testing.T) {
	r := corpusRecord(t, "prompt-glued-to-record")
	if r.Type != RecResult || r.Class != ClassDone {
		t.Fatalf("got type=%v class=%q, want ^done", r.Type, r.Class)
	}
	if got := r.Results.Str("value"); got != "off" {
		t.Errorf("value = %q, want off", got)
	}

	for _, line := range []string{"(gdb)", "(gdb) ", "  (gdb)  "} {
		if got := ParseRecord(line); got.Type != RecPrompt {
			t.Errorf("ParseRecord(%q).Type = %v, want prompt", line, got.Type)
		}
	}
	// Two prompts glued together, then a record.
	if got := ParseRecord(`(gdb) (gdb) *stopped,reason="exited-normally"`); got.Type != RecExec {
		t.Errorf("doubled prompt: type = %v, want exec", got.Type)
	}
}

// TestOctalEscapes is the multi-byte case: \303\251 is one é, not two runes.
func TestOctalEscapes(t *testing.T) {
	r := corpusRecord(t, "log-octal-and-tab")
	if r.Type != RecLog {
		t.Fatalf("type = %v, want log", r.Type)
	}
	if got, want := r.Text, "warning: 78\té\n"; got != want {
		t.Errorf("Text = %q, want %q", got, want)
	}
}

func TestUnescape(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`plain`, "plain"},
		{`a\nb`, "a\nb"},
		{`a\tb`, "a\tb"},
		{`a\\b`, `a\b`},
		{`a\"b`, `a"b`},
		{`\a\b\f\v\r`, "\a\b\f\v\r"},
		{`\303\251`, "é"},
		{`\0`, "\x00"},
		{`\101\102`, "AB"},
		{`\1011`, "A1"},            // exactly three octal digits, then a literal '1'
		{`\x41`, "A"},              // hex tolerated even though gdb emits octal
		{`\z`, "z"},                // undefined escape: drop the backslash
		{`trailing\`, `trailing\`}, // lone backslash at end is kept, not lost
		// An invalid *run* collapses to a single U+FFFD, per strings.ToValidUTF8.
		{"\xff\xfe", "�"},
		{"a\xffb\xffc", "a�b�c"},
	} {
		if got := unescape(tc.in); got != tc.want {
			t.Errorf("unescape(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMalformedRecordsAreGarbageWithErr(t *testing.T) {
	for _, tc := range []struct {
		name, line string
	}{
		{"unterminated string", `^done,msg="never closed`},
		{"unclosed tuple", `^done,frame={level="0"`},
		{"unclosed list", `^done,stack=[frame={level="0"}`},
		{"missing equals", `^done,frame`},
		{"truncated mid-escape", `~"abc\`},
		{"empty class", `^,value="x"`},
		{"trailing junk after stream", `~"text" extra`},
	} {
		r := ParseRecord(tc.line)
		if r.Type != RecGarbage {
			t.Errorf("%s: type = %v, want garbage", tc.name, r.Type)
		}
		if r.Err == nil {
			t.Errorf("%s: Err is nil; malformed MI must be distinguishable from inferior output", tc.name)
		}
		if r.Text != tc.line {
			t.Errorf("%s: Text = %q, want the line preserved verbatim", tc.name, r.Text)
		}
	}
}

// TestAnonymousTopLevelResultIsTolerated pins a deliberate leniency: the
// grammar says a result record carries name=value pairs, but the parser accepts
// a bare value rather than discarding the whole line. gdb is not observed to
// emit this; tolerating it is cheaper than a special case that throws data away.
func TestAnonymousTopLevelResultIsTolerated(t *testing.T) {
	r := ParseRecord(`^done,"loose"`)
	if r.Type != RecResult || r.Class != ClassDone {
		t.Fatalf("type=%v class=%q, want ^done", r.Type, r.Class)
	}
	if len(r.Results) != 1 || r.Results[0].Name != "" || r.Results[0].Value.Str != "loose" {
		t.Errorf("results = %+v, want one anonymous const %q", r.Results, "loose")
	}
}

func TestTokenParsing(t *testing.T) {
	r := ParseRecord(`0000000017^done,value="42"`)
	if !r.HasToken || r.Token != 17 {
		t.Errorf("token = %d (has=%v), want 17", r.Token, r.HasToken)
	}
	r = ParseRecord(`^done`)
	if r.HasToken {
		t.Error("bare ^done should have no token")
	}
	// Token 0 is real and distinct from absent: gdb uses it for results caused
	// by console-originated activity.
	r = ParseRecord(`0^done`)
	if !r.HasToken || r.Token != 0 {
		t.Errorf("token = %d (has=%v), want 0 present", r.Token, r.HasToken)
	}
	// A digit run that is not a token, followed by no classifier.
	if got := ParseRecord(`12345`); got.Type != RecGarbage {
		t.Errorf("bare digits: type = %v, want garbage", got.Type)
	}
}

func TestEmptyCollections(t *testing.T) {
	r := corpusRecord(t, "result-empty-files-list")
	files, ok := r.Results.List("files")
	if !ok {
		t.Fatal("files is not a list")
	}
	if len(files) != 0 {
		t.Errorf("len(files) = %d, want 0", len(files))
	}
	// An empty list must not be indistinguishable from an absent one.
	if !r.Results.Has("files") {
		t.Error("Has(files) = false for an empty list")
	}
	if got := ParseRecord(`^done,t={}`); len(got.Results) != 1 {
		t.Errorf("empty tuple: %d results, want 1", len(got.Results))
	}
}

// TestSimpleValuesAbsenceSignal is the varobj decision from the plan: with
// --simple-values, a missing "value" is what marks an aggregate as expandable.
func TestSimpleValuesAbsenceSignal(t *testing.T) {
	r := corpusRecord(t, "result-variables")
	vars, ok := r.Results.List("variables")
	if !ok {
		t.Fatal("no variables list")
	}
	var expandable, simple int
	for _, v := range vars {
		if v.Has("value") {
			simple++
		} else {
			expandable++
		}
	}
	if expandable == 0 || simple == 0 {
		t.Errorf("got %d expandable and %d simple; the fixture should have both",
			expandable, simple)
	}
}

func TestU64AcceptsHexAndDecimal(t *testing.T) {
	r := ParseRecord(`^done,frame={addr="0x00000000004af4a0",level="3"}`)
	f, _ := r.Results.Get("frame")
	if got, ok := f.U64("addr"); !ok || got != 0x4af4a0 {
		t.Errorf("addr = %#x (ok=%v), want 0x4af4a0", got, ok)
	}
	if got, ok := f.U64("level"); !ok || got != 3 {
		t.Errorf("level = %d (ok=%v), want 3", got, ok)
	}
	if _, ok := f.U64("nope"); ok {
		t.Error("U64 of a missing field reported ok")
	}
}

// TestHugeSingleRecord is finding 9: MI records get big, and bufio.Scanner's
// 64 KiB default token cap fails in a way that looks like a hang. The parser
// itself must have no size ceiling.
func TestHugeSingleRecord(t *testing.T) {
	const target = 4 << 20
	var sb strings.Builder
	sb.WriteString(`^done,memory=[{begin="0x0",contents="`)
	for sb.Len() < target {
		sb.WriteString("00112233445566778899aabbccddeeff")
	}
	sb.WriteString(`"}]`)
	line := sb.String()

	r := ParseRecord(line)
	if r.Type != RecResult || r.Class != ClassDone {
		t.Fatalf("type=%v class=%q, want ^done for a %d-byte record", r.Type, r.Class, len(line))
	}
	mem, ok := r.Results.List("memory")
	if !ok || len(mem) != 1 {
		t.Fatalf("memory list = %v (ok=%v)", len(mem), ok)
	}
	contents, _ := mem[0].StrOK("contents")
	if len(contents) < target-100 {
		t.Errorf("contents truncated to %d bytes", len(contents))
	}
}

func TestMarshalJSONShapes(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		{`^done,value="off"`, `{"value":"off"}`},
		{`^done,stack=[frame={level="0"},frame={level="1"}]`,
			`{"stack":[{"frame":{"level":"0"}},{"frame":{"level":"1"}}]}`},
		{`^done,variables=[{name="i"},{name="j"}]`,
			`{"variables":[{"name":"i"},{"name":"j"}]}`},
		{`^done,files=[]`, `{"files":[]}`},
		{`^done,t={}`, `{"t":{}}`},
		{`^done,msg="a\"b"`, `{"msg":"a\"b"}`},
	} {
		got, err := json.Marshal(ParseRecord(tc.line).Results)
		if err != nil {
			t.Errorf("%s: marshal: %v", tc.line, err)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("%s:\n  got  %s\n  want %s", tc.line, got, tc.want)
		}
	}
}

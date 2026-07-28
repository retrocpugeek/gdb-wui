package mi

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// corpusEntry is one named record from testdata/records.mi.
type corpusEntry struct {
	Name string
	Line string
}

// loadCorpus reads the curated corpus. Every line in it is byte-identical to
// output captured from gdb 17.1, apart from three entries marked synthetic in
// the file header, so a test failure here means the parser disagrees with a
// real debugger rather than with somebody's idea of one.
func loadCorpus(t testing.TB) []corpusEntry {
	t.Helper()
	f, err := os.Open("testdata/records.mi")
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	defer f.Close()

	var out []corpusEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	var pending string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "#"):
			continue
		case strings.HasPrefix(line, "@"):
			if pending != "" {
				t.Fatalf("corpus entry %q has no record line", pending)
			}
			pending = line[1:]
		default:
			if pending == "" {
				t.Fatalf("corpus record with no preceding @name: %q", line)
			}
			out = append(out, corpusEntry{Name: pending, Line: line})
			pending = ""
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	if pending != "" {
		t.Fatalf("corpus entry %q has no record line", pending)
	}
	if len(out) == 0 {
		t.Fatal("corpus is empty")
	}
	return out
}

func (c corpusEntry) parse() Record { return ParseRecord(c.Line) }

// byName finds one corpus entry, for tests that assert on a specific shape.
func corpusRecord(t testing.TB, name string) Record {
	t.Helper()
	for _, e := range loadCorpus(t) {
		if e.Name == name {
			return e.parse()
		}
	}
	t.Fatalf("no corpus entry named %q", name)
	return Record{}
}

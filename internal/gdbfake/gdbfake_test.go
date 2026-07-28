package gdbfake

import (
	"bufio"
	"io"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	steps, err := Parse(`
# a comment
> -exec-run
< ^running
< *stopped,reason="exited-normally"
! prompt
! delay 5ms
! eof
`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []StepKind{StepExpect, StepSend, StepSend, StepPrompt, StepDelay, StepEOF}
	if len(steps) != len(want) {
		t.Fatalf("got %d steps, want %d", len(steps), len(want))
	}
	for i, k := range want {
		if steps[i].Kind != k {
			t.Errorf("step %d kind = %v, want %v", i, steps[i].Kind, k)
		}
	}
	if steps[4].Dur != 5*time.Millisecond {
		t.Errorf("delay = %v, want 5ms", steps[4].Dur)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, tc := range []string{"nonsense", "! nosuchdirective", "! delay notaduration"} {
		if _, err := Parse(tc); err == nil {
			t.Errorf("Parse(%q) succeeded, want an error", tc)
		}
	}
}

func TestTokenIsEchoedOnResults(t *testing.T) {
	f := Start([]Step{
		Expect("-probe"),
		Send(`^done,value="x"`),
		Send(`*stopped`),
		Send(`0^done,value="orphan"`),
	})
	defer f.Close()

	if _, err := io.WriteString(f.ClientStdin, "42-probe\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	br := bufio.NewReader(f.ClientStdout)
	for _, want := range []string{`42^done,value="x"`, `*stopped`, `0^done,value="orphan"`} {
		got, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if strings.TrimRight(got, "\n") != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

// TestOverlapDetectorFires guards the guard: a detector that cannot fire is
// worse than no detector, because it reads as proof.
func TestOverlapDetectorFires(t *testing.T) {
	f := Start([]Step{
		Expect("-one"),
		Send("^done"),
		Expect("-two"),
		Send("^done"),
	}, WithStrictSerialisation())
	defer f.Close()

	// Both commands written before either is answered — what a client without
	// the command semaphore would do.
	if _, err := io.WriteString(f.ClientStdin, "1-one\n2-two\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	br := bufio.NewReader(f.ClientStdout)
	for range 2 {
		if _, err := br.ReadString('\n'); err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	failures := f.Failures()
	if len(failures) == 0 {
		t.Fatal("overlap detector did not fire on two in-flight commands")
	}
	if !strings.Contains(failures[0], "in flight") {
		t.Errorf("failure = %q", failures[0])
	}
}

// TestOverlapDetectorQuietWhenSerialised is the other half: it must not fire on
// a well-behaved client.
func TestOverlapDetectorQuietWhenSerialised(t *testing.T) {
	f := Start([]Step{
		Expect("-one"),
		Send("^done"),
		Expect("-two"),
		Send("^done"),
	}, WithStrictSerialisation())
	defer f.Close()

	br := bufio.NewReader(f.ClientStdout)
	for i, cmd := range []string{"1-one\n", "2-two\n"} {
		if _, err := io.WriteString(f.ClientStdin, cmd); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if _, err := br.ReadString('\n'); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	if got := f.Failures(); len(got) != 0 {
		t.Errorf("detector fired on a serialised client: %v", got)
	}
}

func TestMismatchIsRecorded(t *testing.T) {
	f := Start([]Step{Expect("-expected"), Send("^done")})
	defer f.Close()

	if _, err := io.WriteString(f.ClientStdin, "1-something-else\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	br := bufio.NewReader(f.ClientStdout)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := f.Failures(); len(got) == 0 {
		t.Error("a mismatched command was not recorded as a failure")
	}
}

func TestBigList(t *testing.T) {
	s := BigList("values", 3)
	want := `^done,values=[{index="0",value="0x0"},{index="1",value="0x1"},{index="2",value="0x2"}]`
	if s != want {
		t.Errorf("got %s, want %s", s, want)
	}
}

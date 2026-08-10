//go:build integration

package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/mcp"
	"github.com/retrocpugeek/gdb-wui/internal/runfile"
)

// The bridge, against a real gdb-wui driving a real gdb with a real Ghidra
// behind it.
//
// None of this needs a model, and that is the point of testing it here: the
// thing worth proving is the loop an agent runs — break, run, read what is
// actually in the variable, write down what that showed — and every step of it
// is deterministic. What a model would do with the answers is a different
// question from whether the answers are right.

// TestAnAgentTestsAGuessAgainstTheRunningProgram is the differentiator in one
// test. Static analysis of decompiled text is available against any Ghidra
// project; this is the part that needs a debugger attached.
func TestAnAgentTestsAGuessAgainstTheRunningProgram(t *testing.T) {
	k := bridgeHarness(t, mcp.Options{Annotate: true, Run: true})

	// What is there. An agent's first call, and the one that tells it whether
	// the decompiler is up.
	status := k.tool(t, "status", nil)
	if state := dig(status, "session", "runState"); state == "" {
		t.Fatalf("status says nothing about the run state: %s", status)
	}

	// Ghidra has to finish before it can name anything. One call, not a poll.
	ready := k.tool(t, "wait_for_decompiler", nil)
	if got := dig(ready, "state"); got != "ready" {
		t.Fatalf("the decompiler is %q, so there is nothing to read: %s", got, ready)
	}

	// Break somewhere inside the program. printf survives stripping in the
	// dynamic symbol table, and it is the only way to stop with the program's
	// own code above it on the stack.
	k.tool(t, "set_breakpoint", map[string]any{"location": "printf"})

	// Run — and get the stop back, not an acknowledgement. Nothing to poll.
	stopped := k.tool(t, "run", nil)
	if got := dig(stopped, "state"); got != "stopped" {
		t.Fatalf("run answered %s, want a stop", stopped)
	}
	if dig(stopped, "reason") == "" {
		t.Errorf("the stop does not say why: %s", stopped)
	}

	// Walk out to the program's own frame. gdb has no name for it — that is
	// what stripping did — so the decompiler is asked.
	stack := k.tool(t, "stack", nil)
	frames := arrayOf(t, stack, "frames")
	addresses := make([]any, 0, len(frames))
	for _, f := range frames {
		addresses = append(addresses, f["address"])
	}
	named := k.toolJSON(t, "name_addresses", map[string]any{"addresses": addresses})
	// Only the program's own frames come back: an address in libc is in no
	// function Ghidra was given, and is left alone rather than guessed at.
	inProgram := mapsOf(named["names"])
	if len(inProgram) == 0 {
		t.Fatalf("the decompiler named no frame of %s", stack)
	}
	var inside map[string]any
	for _, f := range frames {
		for _, n := range inProgram {
			if n["addr"] == f["address"] && !strings.HasPrefix(fmt.Sprint(n["name"]), "printf") {
				inside = f
			}
		}
		if inside != nil {
			break
		}
	}
	if inside == nil {
		t.Fatalf("no frame in the program itself: %s", stack)
	}

	fn := k.toolJSON(t, "decompile_function",
		map[string]any{"target": inside["address"]})
	text, _ := fn["text"].(string)
	if text == "" {
		t.Fatalf("no recovered C: %v", fn)
	}

	// The frame the expressions will be read in. A decompiler expression is
	// relative to a frame base, so this is not optional.
	k.tool(t, "select_frame", map[string]any{"frame": inside["level"]})

	// The step that only a debugger can take: read what the recovered variable
	// actually holds, right now, in this process.
	var read int
	for _, v := range mapsOf(fn["vars"]) {
		expr, _ := v["expr"].(string)
		if expr == "" {
			continue
		}
		out := k.tool(t, "evaluate", map[string]any{"expr": expr})
		if strings.Contains(out, `"value"`) {
			read++
		}
	}
	if read == 0 {
		t.Fatalf("not one recovered variable could be read in the live frame: %v", fn["vars"])
	}

	// And write the conclusion where it will still be tomorrow.
	line := firstMappedLine(t, fn)
	entry, _ := fn["entry"].(string)
	commented := k.toolJSON(t, "comment", map[string]any{
		"kind":     "line",
		"function": entry,
		"address":  line,
		"text":     "reached with the accumulator part-filled",
	})
	body, _ := commented["function"].(map[string]any)
	if body == nil || !strings.Contains(fmt.Sprint(body["text"]),
		"reached with the accumulator part-filled") {
		t.Fatalf("the comment is not in the decompiled text: %v", commented)
	}
	// Marked as an agent's. A note it guessed at and a note a person concluded
	// must not read alike.
	marked := false
	for _, c := range mapsOf(body["comments"]) {
		if c["author"] == "agent" {
			marked = true
		}
	}
	if !marked {
		t.Errorf("the comment is not marked as the agent's: %v", body["comments"])
	}

	// A run, undone whole.
	renamed := k.toolJSON(t, "rename", map[string]any{
		"kind": "function", "function": entry,
		"name": fn["name"], "new_name": "reads_the_list",
	})
	run, _ := renamed["run"].(map[string]any)
	if run == nil || run["author"] != "agent" {
		t.Fatalf("the edit reports no agent run: %v", renamed)
	}
	back := k.toolJSON(t, "undo", map[string]any{"run": run["id"]})
	after, _ := back["function"].(map[string]any)
	if after["name"] == "reads_the_list" {
		t.Errorf("the rename survived undoing its run: %v", after["name"])
	}
	if strings.Contains(fmt.Sprint(after["text"]), "reached with the accumulator") {
		t.Errorf("the comment survived undoing its run")
	}
}

// TestConsentIsEnforcedTwice. A tool the user did not agree to is absent from
// the list *and* refused when called: the first stops a model spending a turn
// discovering it is forbidden, and the second is what actually enforces it,
// since a client may hold a list from before the flags were what they are.
func TestConsentIsEnforcedTwice(t *testing.T) {
	k := bridgeHarness(t, mcp.Options{})

	listed := k.list(t)
	for _, forbidden := range []string{"run", "set_breakpoint", "step_instruction",
		"comment", "rename", "set_type", "undo"} {
		if listed[forbidden] {
			t.Errorf("%s is offered without the flag that permits it", forbidden)
		}
	}
	for _, allowed := range []string{"status", "decompile_function", "stack", "evaluate"} {
		if !listed[allowed] {
			t.Errorf("%s is not offered, and reading needs no flag", allowed)
		}
	}

	// Called anyway.
	for _, forbidden := range []string{"set_breakpoint", "comment"} {
		out, isErr := k.call(t, forbidden, map[string]any{
			"location": "main", "kind": "function", "function": "0x1000",
		})
		if !isErr {
			t.Fatalf("%s ran without the flag that permits it: %s", forbidden, out)
		}
		if !strings.Contains(out, "-mcp-") {
			t.Errorf("the refusal for %s does not say which flag is missing: %s",
				forbidden, out)
		}
	}
}

// --- harness ---------------------------------------------------------------

type kit struct {
	in  io.WriteCloser
	out *json.Decoder
	id  int
}

// bridgeHarness starts a gdb-wui on a stripped fixture and runs the bridge
// against it, in this process, over pipes.
func bridgeHarness(t *testing.T, opts mcp.Options) *kit {
	t.Helper()
	requireTools(t)

	dir := t.TempDir()
	buildFixture(t, dir)

	server := exec.Command(binary(t),
		"-project", dir,
		"-exe", "demo",
		"-open=false",
		"-addr", "127.0.0.1:0",
		"-ghidra", ghidraDir(t),
		"-decomp-dir", filepath.Join(dir, "decomp"),
	)
	server.Stdout = &prefixWriter{t: t, tag: "server"}
	server.Stderr = server.Stdout
	if err := server.Start(); err != nil {
		t.Fatalf("starting gdb-wui: %v", err)
	}
	t.Cleanup(func() {
		_ = server.Process.Kill()
		_, _ = server.Process.Wait()
	})

	// Find it the way the bridge does — through the run file — but by project,
	// so a gdb-wui the developer happens to have running is not disturbed and
	// not mistaken for this one.
	addr := waitForRunFile(t, dir)

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	opts.In = inR
	opts.Out = outW
	opts.Version = "test"
	opts.Addr = addr
	opts.Logf = func(f string, a ...any) { t.Logf("bridge: "+f, a...) }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mcp.Run(ctx, opts) }()
	t.Cleanup(func() {
		cancel()
		_ = inW.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})

	k := &kit{in: inW, out: json.NewDecoder(outR)}
	k.rpc(t, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	return k
}

// rpc sends one JSON-RPC request and returns its result.
func (k *kit) rpc(t *testing.T, method string, params any) map[string]any {
	t.Helper()
	k.id++
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": k.id, "method": method, "params": params,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.in.Write(append(body, '\n')); err != nil {
		t.Fatalf("writing %s: %v", method, err)
	}
	var res struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := k.out.Decode(&res); err != nil {
		t.Fatalf("reading the reply to %s: %v", method, err)
	}
	if res.Error != nil {
		t.Fatalf("%s: %s", method, res.Error.Message)
	}
	return res.Result
}

// call runs a tool and reports whether it came back as an error.
func (k *kit) call(t *testing.T, name string, args map[string]any) (string, bool) {
	t.Helper()
	res := k.rpc(t, "tools/call", map[string]any{"name": name, "arguments": args})
	content, _ := res["content"].([]any)
	text := ""
	if len(content) > 0 {
		first, _ := content[0].(map[string]any)
		text, _ = first["text"].(string)
	}
	isErr, _ := res["isError"].(bool)
	return text, isErr
}

func (k *kit) tool(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	text, isErr := k.call(t, name, args)
	if isErr {
		t.Fatalf("%s: %s", name, text)
	}
	return text
}

func (k *kit) toolJSON(t *testing.T, name string, args map[string]any) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(k.tool(t, name, args)), &out); err != nil {
		t.Fatalf("%s did not answer with an object: %v", name, err)
	}
	return out
}

func (k *kit) list(t *testing.T) map[string]bool {
	t.Helper()
	res := k.rpc(t, "tools/list", map[string]any{})
	out := map[string]bool{}
	for _, raw := range res["tools"].([]any) {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		out[name] = true
	}
	return out
}

// --- fixtures --------------------------------------------------------------

var (
	buildOnce sync.Once
	builtPath string
	buildErr  error
)

func binary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		out := filepath.Join(os.TempDir(), fmt.Sprintf("gdb-wui-mcp-test-%d", os.Getpid()))
		cmd := exec.Command("go", "build", "-o", out, "./cmd/gdb-wui")
		cmd.Dir = repoRoot(t)
		if msg, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("building gdb-wui: %v: %s", err, msg)
			return
		}
		builtPath = out
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return builtPath
}

// buildFixture compiles the program the agent will read: no debug info and
// stripped, which is the case this whole tool set exists for.
func buildFixture(t *testing.T, dir string) {
	t.Helper()
	src := filepath.Join(dir, "demo.c")
	const body = `
#include <stdio.h>
static int accumulate(int n) {
	int total = 0;
	for (int i = 0; i < n; i++) total += i * 3;
	return total;
}
int main(void) { printf("acc=%d\n", accumulate(5)); return 0; }
`
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "demo")
	if out, err := exec.Command("gcc", "-O0", "-o", exe, src).CombinedOutput(); err != nil {
		t.Fatalf("gcc: %v: %s", err, out)
	}
	if out, err := exec.Command("strip", exe).CombinedOutput(); err != nil {
		t.Fatalf("strip: %v: %s", err, out)
	}
}

func requireTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"gdb", "gcc", "strip", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
}

func ghidraDir(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv("GHIDRA_INSTALL_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory to look for Ghidra in")
	}
	dir := filepath.Join(home, "ghidra")
	if _, err := os.Stat(dir); err != nil {
		t.Skip("no Ghidra installation")
	}
	return dir
}

// waitForRunFile finds this test's own server, by the project it is serving.
func waitForRunFile(t *testing.T, project string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := runfile.List()
		if err == nil {
			for _, e := range entries {
				if sameDir(e.Project, project) {
					return e.Addr
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("gdb-wui never announced itself for %s", project)
	return ""
}

func sameDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return a == b
	}
	return ra == rb
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

type prefixWriter struct {
	t   *testing.T
	tag string
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line != "" {
			w.t.Logf("%s: %s", w.tag, line)
		}
	}
	return len(p), nil
}

// --- reading replies -------------------------------------------------------

func dig(body string, keys ...string) string {
	var cur any
	if json.Unmarshal([]byte(body), &cur) != nil {
		return ""
	}
	for _, key := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[key]
	}
	if s, ok := cur.(string); ok {
		return s
	}
	if cur == nil {
		return ""
	}
	return fmt.Sprint(cur)
}

func arrayOf(t *testing.T, body, key string) []map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("not an object: %s", body)
	}
	return mapsOf(out[key])
}

func mapsOf(v any) []map[string]any {
	raw, _ := v.([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// firstMappedLine is an address a comment can hang on: one a decompiled line
// came from. A brace or a declaration came from none.
func firstMappedLine(t *testing.T, fn map[string]any) string {
	t.Helper()
	for _, l := range mapsOf(fn["lines"]) {
		addrs, _ := l["addrs"].([]any)
		if len(addrs) > 0 {
			s, _ := addrs[0].(string)
			return s
		}
	}
	t.Fatal("the function has no line with an address")
	return ""
}

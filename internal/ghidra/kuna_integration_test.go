//go:build integration

package ghidra_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/ghidra"
)

// kunaEngine is a native kuna_wasm on the fixture, or a skip: KUNA_WASM names
// the binary (kuna's decompiler/target/release/kuna_wasm) and KUNA_SPECS its
// specs (kuna's specs/, after `make specs`).
func kunaEngine(t *testing.T, binary string) ghidra.KunaEngine {
	t.Helper()
	bin, specs := os.Getenv("KUNA_WASM"), os.Getenv("KUNA_SPECS")
	if bin == "" || specs == "" {
		t.Skip("KUNA_WASM and KUNA_SPECS are not set")
	}
	return ghidra.KunaCommand(bin, specs, binary)
}

func kunaBinary(t *testing.T) string {
	if b := os.Getenv("KUNA_BINARY"); b != "" {
		return b
	}
	return fixture(t)
}

func TestKunaDecompilesTheFixture(t *testing.T) {
	bin := kunaBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, err := ghidra.StartKuna(ctx, kunaEngine(t, bin), bin, ghidra.Options{Timeout: time.Minute})
	if err != nil {
		t.Fatalf("StartKuna: %v", err)
	}
	defer c.Close()
	checkKuna(t, ctx, c)
}

func checkKuna(t *testing.T, ctx context.Context, c *ghidra.Client) {
	t.Helper()
	r := c.Ready()
	if r.Sidecar != ghidra.KunaGenerator || r.Program.SHA256 == "" || r.Program.LanguageID == "" || r.FunctionCount == 0 {
		t.Fatalf("greeting %+v", r)
	}
	main, err := c.Decompile(ctx, "main")
	if err != nil {
		t.Fatalf("Decompile main: %v", err)
	}
	t.Logf("%s %s\n%s", main.Name, main.Signature, main.Text)
	b, _ := json.MarshalIndent(main.Variables, "", " ")
	t.Logf("vars %s frame %+v depth %v globals %+v", b, main.Frame, spDepth(main.Frame.SPDepth), main.Globals)
	if len(main.Lines) == 0 || main.Text == "" || main.BodyEnd <= main.BodyStart {
		t.Errorf("main: %d lines, body %s-%s", len(main.Lines), main.BodyStart, main.BodyEnd)
	}
	names, err := c.Names(ctx, []string{main.Entry, "0x1"})
	if err != nil || len(names) != 1 || names[0].Name != "main" {
		t.Errorf("Names = %+v, %v", names, err)
	}
	list, err := c.Functions(ctx, 0, 5000, "")
	if err != nil || list.Total != r.FunctionCount {
		t.Errorf("Functions: total %d of %d, %v", list.Total, r.FunctionCount, err)
	}
	if _, err := c.Rename(ctx, ghidra.Edit{Kind: ghidra.EditFunction, Function: main.Entry,
		Name: "main", Value: "x"}); err == nil || !strings.Contains(err.Error(), "kuna") {
		t.Errorf("Rename = %v, want kuna's refusal", err)
	}
	// A decompilation names the functions it calls; the ones kuna's
	// inventory missed are still reachable by those names.
	for _, w := range strings.Fields(strings.NewReplacer("(", " ", ")", " ", ",", " ", ";", " ").Replace(main.Text)) {
		if strings.HasPrefix(w, "sub_") {
			f, err := c.Decompile(ctx, w)
			if err != nil || f.Name != w {
				t.Errorf("Decompile %s = %v, %v", w, f, err)
			}
			break
		}
	}
}

func spDepth(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestKunaOverTCP serves a native kuna_wasm the way the emulator pages serve
// their WebAssembly one, and checks the client of that protocol.
func TestKunaOverTCP(t *testing.T) {
	bin := kunaBinary(t)
	native := kunaEngine(t, bin)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	uploads := 0
	held := map[string][]byte{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				r := bufio.NewReader(conn)
				for {
					line, err := r.ReadBytes('\n')
					if err != nil {
						return
					}
					var req struct {
						Op   string   `json:"op"`
						SHA  string   `json:"sha256"`
						Size int      `json:"size"`
						Args []string `json:"args"`
					}
					json.Unmarshal(line, &req)
					var reply any
					switch req.Op {
					case "load":
						_, have := held[req.SHA]
						reply = map[string]any{"ok": true, "have": have, "version": "test"}
					case "upload":
						buf := make([]byte, req.Size)
						if _, err := io.ReadFull(r, buf); err != nil {
							return
						}
						held[req.SHA] = buf
						uploads++
						reply = map[string]any{"ok": true}
					case "run":
						out, err := native.Run(context.Background(), req.Args...)
						if err != nil {
							reply = map[string]any{"ok": false, "error": err.Error()}
						} else {
							reply = map[string]any{"ok": true, "result": json.RawMessage(out)}
						}
					}
					b, _ := json.Marshal(reply)
					conn.Write(append(b, '\n'))
				}
			}()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for i := 0; i < 2; i++ {
		engine, err := ghidra.DialKuna(ctx, ln.Addr().String(), bin)
		if err != nil {
			t.Fatalf("DialKuna: %v", err)
		}
		c, err := ghidra.StartKuna(ctx, engine, bin, ghidra.Options{Timeout: time.Minute})
		if err != nil {
			t.Fatalf("StartKuna: %v", err)
		}
		if c.Ready().SidecarVersion != "test" {
			t.Errorf("version %q", c.Ready().SidecarVersion)
		}
		if _, err := c.Decompile(ctx, "main"); err != nil {
			t.Errorf("Decompile: %v", err)
		}
		c.Close()
	}
	if uploads != 1 {
		t.Errorf("%d uploads, want the binary sent once", uploads)
	}
}

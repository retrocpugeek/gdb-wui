package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// The tools an agent is given.
//
// Almost every one is a single request on the protocol the browser speaks, and
// its reply is passed through unchanged. That is deliberate twice over: the
// server's guards apply because there is no other road into it, and
// docs/protocol.md documents what these tools answer with, because it is the
// same document.
//
// The exceptions are the ones that run the program. Those are synchronous to
// the next stop rather than to gdb's acknowledgement — see runToStop.

// tier is what a tool needs permission for. Three, because "read this binary",
// "write into my Ghidra project" and "run my program with my privileges" are
// three different conversations to have with a user, and bundling them would
// mean the cautious answer to any one is no to all three.
type tier int

const (
	// tierRead reads the program, the process and the decompiler's database.
	// It changes nothing.
	tierRead tier = iota
	// tierAnnotate writes names, types and comments into the Ghidra project
	// gdb-wui imported. Never the user's own project: the server refuses that
	// one whatever this says.
	tierAnnotate
	// tierRun sets breakpoints and starts, steps and stops the program. The
	// program runs with the user's privileges, which is what a debugger is for
	// and is why this is asked for separately.
	tierRun
)

// Deliberately absent at every tier: mem.write, vars.assign, regs.write and
// console.exec. Writing into a running program's memory is a different feature
// with a different consent conversation, and a gdb console would make every
// restriction here decorative — `set var` reaches everything the others deny.

type tool struct {
	name   string
	tier   tier
	desc   string
	schema map[string]any
	call   func(ctx context.Context, s *session, a args) (any, error)
}

// args is one tool call's arguments, with the accessors a hand-written schema
// needs. Missing and wrong-typed are the same thing here — the zero value —
// because the server validates properly and reporting two kinds of "you did not
// give me an address" from two places helps nobody.
type args map[string]any

func (a args) str(key string) string {
	s, _ := a[key].(string)
	return s
}

func (a args) num(key string, dflt int) int {
	if n, ok := a[key].(float64); ok {
		return int(n)
	}
	return dflt
}

func (a args) boolean(key string) bool {
	b, _ := a[key].(bool)
	return b
}

func (a args) strings(key string) []string {
	raw, _ := a[key].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// seconds reads a timeout in the unit a model thinks in, bounded so that one
// tool call cannot hang a conversation for an hour.
func (a args) seconds(key string, dflt, max time.Duration) time.Duration {
	d := time.Duration(a.num(key, int(dflt/time.Second))) * time.Second
	if d <= 0 {
		return dflt
	}
	if d > max {
		return max
	}
	return d
}

// object builds a JSON Schema for a tool's arguments.
//
// A tool with no arguments still needs "properties": {} rather than null: the
// schema is a JSON Schema object, and clients that validate it reject a null
// where a record belongs — one such tool makes the whole tools/list unusable.
func object(props map[string]any, required ...string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	out := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func prop(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}

// pass sends one request and hands the server's reply straight back.
func pass(typ string, build func(a args) any) func(context.Context, *session, args) (any, error) {
	return func(ctx context.Context, s *session, a args) (any, error) {
		raw, err := s.call(ctx, typ, build(a))
		if err != nil {
			return nil, err
		}
		return json.RawMessage(raw), nil
	}
}

const defaultStopWait = 30 * time.Second
const maxStopWait = 10 * time.Minute

// tools is the whole surface, in the order they are listed to a client.
func tools() []tool {
	return []tool{{
		name: "status",
		tier: tierRead,
		desc: "What gdb has loaded, whether the program is running or stopped, " +
			"whether the decompiler is ready, and which of these tools are " +
			"permitted. Worth calling first: everything else depends on it.",
		schema: object(nil),
		call: func(ctx context.Context, s *session, a args) (any, error) {
			hello, err := s.call(ctx, wire.TypeSessionInfo, struct{}{})
			if err != nil {
				return nil, err
			}
			decomp, err := s.call(ctx, wire.TypeDecompStatus, struct{}{})
			if err != nil {
				// Not fatal: a session with no Ghidra is an ordinary one, and
				// the rest of the answer is still worth having.
				decomp = json.RawMessage("null")
			}
			return map[string]any{
				"session":    json.RawMessage(hello),
				"decompiler": json.RawMessage(decomp),
			}, nil
		},
	}, {
		name: "wait_for_decompiler",
		tier: tierRead,
		desc: "Wait until Ghidra has finished importing and analysing the " +
			"binary, which is seconds for a small program and minutes for " +
			"firmware. Until it has, decompile_function and name_addresses " +
			"answer nothing. Waiting here costs one call; polling status costs " +
			"one per attempt.",
		schema: object(map[string]any{
			"timeout_seconds": prop("integer", "120 by default"),
		}),
		call: func(ctx context.Context, s *session, a args) (any, error) {
			return s.waitForDecompiler(ctx, a.seconds("timeout_seconds", 2*time.Minute, maxStopWait))
		},
	}, {
		name: "list_symbols",
		tier: tierRead,
		desc: "Symbols from the binary's own table, matched on a substring. " +
			"A stripped binary has few or none — that is the case this whole " +
			"tool set is for — so start from the stack and follow calls in the " +
			"decompiled text instead.",
		schema: object(map[string]any{
			"filter": prop("string", "case-insensitive substring of the name"),
			"kind":   prop("string", `"function" or "variable"; both if omitted`),
			"limit":  prop("integer", "how many to return"),
		}),
		call: pass(wire.TypeSymbolsList, func(a args) any {
			return wire.SymbolsListRequest{
				Filter: a.str("filter"), Kind: a.str("kind"), Limit: a.num("limit", 0),
			}
		}),
	}, {
		name: "decompile_function",
		tier: tierRead,
		desc: "Ghidra's recovered C for a function, with a map from each line " +
			"to the addresses it came from, the local variables and their " +
			"storage, and any comments already on it. The target is a function " +
			"name or any runtime address inside one; empty follows the selected " +
			"frame. Names like FUN_00401154, local_10 and undefined8 are the " +
			"decompiler's guesses, not the program's.",
		schema: object(map[string]any{
			"target": prop("string", "function name, or a runtime address in hex"),
		}),
		call: pass(wire.TypeDecompFunction, func(a args) any {
			return wire.DecompFunctionRequest{Target: a.str("target")}
		}),
	}, {
		name: "disassemble",
		tier: tierRead,
		desc: "Instructions for the function containing an address, or for an " +
			"explicit range. The ground truth the recovered C is a model of.",
		schema: object(map[string]any{
			"target": prop("string", "runtime address in the function; empty means the selected frame"),
			"start":  prop("string", "start of an explicit range, hex"),
			"end":    prop("string", "end of an explicit range, hex"),
		}),
		call: func(ctx context.Context, s *session, a args) (any, error) {
			if start := a.str("start"); start != "" {
				raw, err := s.call(ctx, wire.TypeDisasmRange,
					wire.DisasmRangeRequest{Start: start, End: a.str("end")})
				if err != nil {
					return nil, err
				}
				return json.RawMessage(raw), nil
			}
			raw, err := s.call(ctx, wire.TypeDisasmFunction,
				wire.DisasmFunctionRequest{Address: a.str("target")})
			if err != nil {
				return nil, err
			}
			return json.RawMessage(raw), nil
		},
	}, {
		name: "name_addresses",
		tier: tierRead,
		desc: "Which function each runtime address falls in, according to the " +
			"decompiler, without decompiling any of them. An address in no " +
			"function is left out rather than guessed at.",
		schema: object(map[string]any{
			"addresses": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "runtime addresses in hex",
			},
		}, "addresses"),
		call: pass(wire.TypeDecompNames, func(a args) any {
			return wire.DecompNamesRequest{Addresses: a.strings("addresses")}
		}),
	}, {
		name: "stack",
		tier: tierRead,
		desc: "The call stack of the current thread. Needs the program stopped. " +
			"Frames inside a stripped binary read as ?? from gdb; " +
			"name_addresses says what the decompiler calls them.",
		schema: object(map[string]any{
			"thread": prop("integer", "thread id; the current one if omitted"),
		}),
		call: pass(wire.TypeStackList, func(a args) any {
			return wire.StackListRequest{Thread: a.num("thread", 0)}
		}),
	}, {
		name:   "select_frame",
		tier:   tierRead,
		desc:   "Choose which stack frame locals, registers and expressions are read in.",
		schema: object(map[string]any{"frame": prop("integer", "frame number, 0 is innermost")}, "frame"),
		call: pass(wire.TypeFrameSelect, func(a args) any {
			return wire.FrameSelectRequest{Frame: a.num("frame", 0), Thread: a.num("thread", 0)}
		}),
	}, {
		name: "locals",
		tier: tierRead,
		desc: "The selected frame's locals and arguments, with values. Needs " +
			"the program stopped. A binary with no debug info has none of " +
			"these — read the decompiler's variables through evaluate instead.",
		schema: object(map[string]any{
			"frame": prop("integer", "frame number; the selected one if omitted"),
		}),
		call: pass(wire.TypeVarsLocals, func(a args) any {
			return wire.VarsLocalsRequest{Frame: a.num("frame", 0), Thread: a.num("thread", 0)}
		}),
	}, {
		name: "registers",
		tier: tierRead,
		desc: "Register values for the current thread. Needs the program stopped.",
		schema: object(map[string]any{
			"format": prop("string", "x, d, o, t or N (natural); x by default"),
		}),
		call: pass(wire.TypeRegsValues, func(a args) any {
			return wire.RegsValuesRequest{Format: a.str("format"), Thread: a.num("thread", 0)}
		}),
	}, {
		name: "evaluate",
		tier: tierRead,
		desc: "Evaluate a gdb expression in the selected frame — this is how a " +
			"guess about the program gets tested against what it is actually " +
			"doing. decompile_function gives each recovered variable an `expr` " +
			"that can be passed here; the decompiler's own names cannot, " +
			"because gdb has never heard of them. Needs the program stopped.",
		schema: object(map[string]any{
			"expr":  prop("string", "a gdb expression, e.g. *(int *)0x404050 or $rdi"),
			"frame": prop("integer", "frame number; the selected one if omitted"),
		}, "expr"),
		call: pass(wire.TypeEvalExpr, func(a args) any {
			return wire.EvalExprRequest{Expr: a.str("expr"), Frame: a.num("frame", 0)}
		}),
	}, {
		name: "read_memory",
		tier: tierRead,
		desc: "Raw bytes at an address or expression, as hex. Needs the program " +
			"stopped. Capped server-side.",
		schema: object(map[string]any{
			"address": prop("string", "address in hex, or an expression that yields one"),
			"count":   prop("integer", "how many bytes"),
		}, "address", "count"),
		call: pass(wire.TypeMemRead, func(a args) any {
			return wire.MemReadRequest{Address: a.str("address"), Count: a.num("count", 64)}
		}),
	}, {
		name:   "list_breakpoints",
		tier:   tierRead,
		desc:   "Every breakpoint gdb knows about, however it was set.",
		schema: object(nil),
		call:   pass(wire.TypeBpList, func(a args) any { return struct{}{} }),
	}, {
		name: "set_breakpoint",
		tier: tierRun,
		desc: "Break at a function name or a runtime address. A name is better " +
			"where one exists: gdb skips the prologue for a named function, " +
			"which is where the interesting state has been set up.",
		schema: object(map[string]any{
			"location":  prop("string", "function name or address in hex"),
			"temporary": prop("boolean", "delete it once hit"),
			"condition": prop("string", "a gdb expression that must be true to stop"),
		}, "location"),
		call: pass(wire.TypeBpSetAddress, func(a args) any {
			return wire.BreakpointAddressRequest{
				Location:  a.str("location"),
				Temporary: a.boolean("temporary"),
				Condition: a.str("condition"),
			}
		}),
	}, {
		name:   "delete_breakpoint",
		tier:   tierRun,
		desc:   "Remove a breakpoint by number.",
		schema: object(map[string]any{"number": prop("integer", "gdb's breakpoint number")}, "number"),
		call: pass(wire.TypeBpDelete, func(a args) any {
			return wire.BreakpointIDRequest{Number: a.num("number", 0)}
		}),
	}, {
		name: "run",
		tier: tierRun,
		desc: "Start the program and wait for it to stop. Returns where it " +
			"stopped and why, not an acknowledgement — there is nothing to poll " +
			"for. Set breakpoints first, or it will run to completion. The " +
			"program runs with your user's privileges.",
		schema: object(map[string]any{
			"stop_at_entry":   prop("boolean", "stop at the first instruction; the only way to catch a stripped binary before main"),
			"stop_at_main":    prop("boolean", "stop at main, if the binary has one"),
			"timeout_seconds": prop("integer", "how long to wait for a stop; 30 by default"),
		}),
		call: func(ctx context.Context, s *session, a args) (any, error) {
			return s.runToStop(ctx, wire.TypeExecRun, wire.ExecRequest{
				StopAtEntry: a.boolean("stop_at_entry"),
				StopAtMain:  a.boolean("stop_at_main"),
			}, a.seconds("timeout_seconds", defaultStopWait, maxStopWait))
		},
	}, {
		name:   "continue",
		tier:   tierRun,
		desc:   "Resume, and wait for the next stop.",
		schema: object(map[string]any{"timeout_seconds": prop("integer", "30 by default")}),
		call: func(ctx context.Context, s *session, a args) (any, error) {
			return s.runToStop(ctx, wire.TypeExecContinue, wire.ExecRequest{},
				a.seconds("timeout_seconds", defaultStopWait, maxStopWait))
		},
	}, {
		name: "step_instruction",
		tier: tierRun,
		desc: "One machine instruction, stepping into a call or over it. " +
			"Instructions rather than lines, because a binary with no debug " +
			"info has no line table and gdb's own step would run to the end of " +
			"the function.",
		schema: object(map[string]any{
			"over":            prop("boolean", "step over a call rather than into it"),
			"timeout_seconds": prop("integer", "30 by default"),
		}),
		call: func(ctx context.Context, s *session, a args) (any, error) {
			typ := wire.TypeExecStepI
			if a.boolean("over") {
				typ = wire.TypeExecNextI
			}
			return s.runToStop(ctx, typ, wire.ExecRequest{},
				a.seconds("timeout_seconds", defaultStopWait, maxStopWait))
		},
	}, {
		name:   "finish",
		tier:   tierRun,
		desc:   "Run until the selected frame returns, and wait for the stop.",
		schema: object(map[string]any{"timeout_seconds": prop("integer", "30 by default")}),
		call: func(ctx context.Context, s *session, a args) (any, error) {
			return s.runToStop(ctx, wire.TypeExecFinish, wire.ExecRequest{},
				a.seconds("timeout_seconds", defaultStopWait, maxStopWait))
		},
	}, {
		name: "pause",
		tier: tierRun,
		desc: "Interrupt a running program. What to reach for when run or " +
			"continue came back still running.",
		schema: object(map[string]any{"timeout_seconds": prop("integer", "10 by default")}),
		call: func(ctx context.Context, s *session, a args) (any, error) {
			return s.runToStop(ctx, wire.TypeExecPause, wire.ExecRequest{},
				a.seconds("timeout_seconds", 10*time.Second, maxStopWait))
		},
	}, {
		name: "wait_for_stop",
		tier: tierRead,
		desc: "Wait for the program to stop without resuming it — for when " +
			"somebody at the browser started it, or a previous call timed out.",
		schema: object(map[string]any{"timeout_seconds": prop("integer", "30 by default")}),
		call: func(ctx context.Context, s *session, a args) (any, error) {
			return s.waitForStop(ctx, a.seconds("timeout_seconds", defaultStopWait, maxStopWait))
		},
	}, {
		name: "rename",
		tier: tierAnnotate,
		desc: "Give a decompiler name of your own. kind is \"function\", " +
			"\"variable\" (a local, addressed by the id decompile_function " +
			"reported) or \"global\" (addressed by its address). The name is " +
			"recorded as inferred rather than as something a person stated, and " +
			"it goes into the Ghidra project, so it is there next time and every " +
			"open browser tab repaints.",
		schema: object(map[string]any{
			"kind":     prop("string", `"function", "variable" or "global"`),
			"function": prop("string", "entry address of the function on screen, runtime hex"),
			"symbol":   prop("string", "the variable's id from decompile_function"),
			"name":     prop("string", "its current name, as a check that this is the right one"),
			"address":  prop("string", "a global's address, runtime hex"),
			"new_name": prop("string", "the new name"),
		}, "kind", "function", "new_name"),
		call: pass(wire.TypeDecompRename, func(a args) any {
			return editRequest(a, a.str("new_name"))
		}),
	}, {
		name: "set_type",
		tier: tierAnnotate,
		desc: "Set a local's C type, a global's when kind is \"global\" " +
			"(addressed by its address, like rename), or a whole function " +
			"prototype when kind is \"function\" — a prototype carries the name " +
			"too, so applying one renames the function. Getting a type right " +
			"often reshapes the decompiled body, which is the point: typing a " +
			"global as an array turns *(char **)(&tbl + i * 8) into tbl[i].",
		schema: object(map[string]any{
			"kind":     prop("string", `"variable", "global" or "function"`),
			"function": prop("string", "entry address of the function, runtime hex"),
			"symbol":   prop("string", "the variable's id from decompile_function"),
			"name":     prop("string", "its current name"),
			"address":  prop("string", "a global's address, runtime hex"),
			"type":     prop("string", `a C type, or a whole prototype: "long parse(char *, int)"`),
		}, "kind", "function", "type"),
		call: pass(wire.TypeDecompRetype, func(a args) any {
			return editRequest(a, a.str("type"))
		}),
	}, {
		name: "comment",
		tier: tierAnnotate,
		desc: "Write a note into the decompiled C: above one line when kind is " +
			"\"line\" (with the address that line came from), or above the " +
			"function when kind is \"function\". This is where an observation " +
			"belongs — there is no source file to write it in. Empty text " +
			"removes the comment. Notes written here are marked as an agent's.",
		schema: object(map[string]any{
			"kind":     prop("string", `"line" or "function"`),
			"function": prop("string", "entry address of the function, runtime hex"),
			"address":  prop("string", "for a line: an address that line came from, runtime hex"),
			"text":     prop("string", "the note; empty removes it"),
		}, "kind", "function"),
		call: pass(wire.TypeDecompComment, func(a args) any {
			return editRequest(a, a.str("text"))
		}),
	}, {
		name: "undo",
		tier: tierAnnotate,
		desc: "Reverse the last annotation, or a whole run of them by id — " +
			"every reply carries the run its edit landed in. Only the most " +
			"recent run can be undone.",
		schema: object(map[string]any{
			"run": prop("string", "run id; without it, one edit is undone"),
		}),
		call: pass(wire.TypeDecompUndo, func(a args) any {
			return wire.DecompUndoRequest{Run: a.str("run")}
		}),
	}}
}

// editRequest is the shape all three annotation tools send. The author is set
// here and nowhere else: it is the one claim this bridge makes about itself.
func editRequest(a args, value string) wire.DecompEditRequest {
	return wire.DecompEditRequest{
		Kind:     a.str("kind"),
		Function: a.str("function"),
		Symbol:   a.str("symbol"),
		Name:     a.str("name"),
		Address:  a.str("address"),
		Value:    value,
		Author:   wire.DecompAuthorAgent,
	}
}

// permitted filters the table to what the flags allow.
//
// A tool the user did not consent to is not listed at all, as well as refused
// if called anyway. Both, because they answer different questions: a model that
// cannot see a tool does not spend a turn discovering it is forbidden, and a
// model that has cached an older list does not get to act on it.
func permitted(annotate, run bool) []tool {
	var out []tool
	for _, t := range tools() {
		if t.tier == tierAnnotate && !annotate {
			continue
		}
		if t.tier == tierRun && !run {
			continue
		}
		out = append(out, t)
	}
	return out
}

// refusal explains a tool that exists but was not consented to.
func refusal(t tool) error {
	switch t.tier {
	case tierAnnotate:
		return fmt.Errorf("%s is not permitted: gdb-wui was started without "+
			"-mcp-annotate, so this bridge may read the program but not write "+
			"names, types or comments into the decompiler", t.name)
	case tierRun:
		return fmt.Errorf("%s is not permitted: gdb-wui was started without "+
			"-mcp-run, so this bridge may not start, step or stop the program",
			t.name)
	}
	return nil
}

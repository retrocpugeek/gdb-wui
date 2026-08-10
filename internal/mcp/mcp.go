// Package mcp bridges a running gdb-wui to an MCP client, so an agent can read
// a binary, drive the debugger and write down what it worked out.
//
// The whole design is that the bridge is a *client*. It joins the same
// WebSocket a browser does, with a credential it takes from the same 0600 run
// file `-print-url` reads, and every tool is one request on the protocol
// documented in docs/protocol.md. Nothing is added to the server, so nothing
// can be added to the server's threat model; and every guard already there —
// the read-only-project refusal, the run-state gate, address translation, the
// undo journal, the broadcast that repaints open tabs — applies to an agent
// because there is no other road in.
//
// What makes it worth doing at all is the debugger. Static analysis of
// decompiled text is what an agent can do against any Ghidra project; here it
// can set a breakpoint, run to it, read the bytes actually in the buffer, and
// write the conclusion into the decompilation. That loop is the feature, and
// the tools are arranged around making it cheap to run.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
)

// Options configures one bridge process.
type Options struct {
	// Addr picks the server when several are running, the same way -print-url
	// does. Empty means the only one.
	Addr string
	// Annotate permits writing names, types and comments. Off by default: an
	// agent that may edit the decompiler's database is a different thing to
	// consent to than one that may read it.
	Annotate bool
	// Run permits setting breakpoints and running the program. Off by default,
	// and separately, because the program runs with the user's privileges.
	Run bool
	// Version is reported to the client.
	Version string

	// In and Out are the MCP transport. Both default to the process's stdio.
	//
	// Nothing else may write to Out. The protocol is one JSON object per line,
	// and a stray println in the middle of it is a client that disconnects with
	// a parse error nobody can place.
	In  io.Reader
	Out io.Writer
	// Logf receives diagnostics. They go to stderr, never to Out.
	Logf func(string, ...any)
}

// protocolVersion is what this speaks when a client does not say.
const protocolVersion = "2025-06-18"

// Run serves MCP on stdio until the input ends or the context is cancelled.
func Run(ctx context.Context, opts Options) error {
	if opts.In == nil {
		opts.In = os.Stdin
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}

	s, err := connect(ctx, opts.Addr)
	if err != nil {
		return err
	}
	defer s.close()
	opts.Logf("connected; tools: read%s%s",
		map[bool]string{true: ", annotate"}[opts.Annotate],
		map[bool]string{true: ", run"}[opts.Run])

	b := &bridge{opts: opts, session: s, out: opts.Out}
	return b.serve(ctx)
}

type bridge struct {
	opts    Options
	session *session

	// mu serialises writes. Tool calls are answered on the reading goroutine,
	// so this guards against nothing today — but a JSON-RPC writer that is not
	// safe to share is a trap laid for whoever adds the first notification.
	mu  sync.Mutex
	out io.Writer
}

// JSON-RPC 2.0, one object per line.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInternal       = -32603
)

func (b *bridge) serve(ctx context.Context) error {
	in := bufio.NewScanner(b.opts.In)
	// A tool result can carry a whole decompiled function back, and a client
	// may send a long argument; neither fits the scanner's default 64k.
	in.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)

	for in.Scan() {
		line := in.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			b.fail(nil, codeParse, "could not parse that as JSON-RPC")
			continue
		}
		// A notification has no id and takes no reply, ever — answering one is
		// a protocol violation rather than a harmless extra.
		if len(req.ID) == 0 {
			continue
		}
		b.dispatch(ctx, req)

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	return in.Err()
}

func (b *bridge) dispatch(ctx context.Context, req rpcRequest) {
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		version := params.ProtocolVersion
		if version == "" {
			version = protocolVersion
		}
		b.reply(req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{
				"name":    "gdb-wui",
				"version": b.opts.Version,
			},
			"instructions": instructions,
		})
	case "ping":
		b.reply(req.ID, map[string]any{})
	case "tools/list":
		listed := permitted(b.opts.Annotate, b.opts.Run)
		out := make([]map[string]any, 0, len(listed))
		for _, t := range listed {
			out = append(out, map[string]any{
				"name":        t.name,
				"description": t.desc,
				"inputSchema": t.schema,
			})
		}
		b.reply(req.ID, map[string]any{"tools": out})
	case "tools/call":
		b.callTool(ctx, req)
	default:
		b.fail(req.ID, codeMethodNotFound, fmt.Sprintf("no method %q", req.Method))
	}
}

func (b *bridge) callTool(ctx context.Context, req rpcRequest) {
	var params struct {
		Name      string `json:"name"`
		Arguments args   `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.fail(req.ID, codeInvalidRequest, "could not read the tool call")
		return
	}

	var found *tool
	for _, t := range tools() {
		if t.name == params.Name {
			candidate := t
			found = &candidate
			break
		}
	}
	if found == nil {
		b.fail(req.ID, codeMethodNotFound, fmt.Sprintf("no tool %q", params.Name))
		return
	}
	// The refusal as well as the omission from tools/list. A client holding a
	// list from before the flags changed must not be able to act on it, and a
	// consent enforced only by what was advertised is not enforced.
	if (found.tier == tierAnnotate && !b.opts.Annotate) ||
		(found.tier == tierRun && !b.opts.Run) {
		b.toolError(req.ID, refusal(*found))
		return
	}

	out, err := found.call(ctx, b.session, params.Arguments)
	if err != nil {
		// A tool that failed is a result, not a transport error: the model has
		// to see the message to correct itself, and a JSON-RPC error would end
		// the turn instead.
		b.toolError(req.ID, err)
		return
	}
	body, err := json.Marshal(out)
	if err != nil {
		b.fail(req.ID, codeInternal, err.Error())
		return
	}
	b.reply(req.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(body)}},
	})
}

func (b *bridge) toolError(id json.RawMessage, err error) {
	b.opts.Logf("tool error: %v", err)
	b.reply(id, map[string]any{
		"isError": true,
		"content": []map[string]any{{"type": "text", "text": err.Error()}},
	})
}

func (b *bridge) reply(id json.RawMessage, result any) {
	b.write(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (b *bridge) fail(id json.RawMessage, code int, message string) {
	if id == nil {
		id = json.RawMessage("null")
	}
	b.write(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}})
}

func (b *bridge) write(res rpcResponse) {
	body, err := json.Marshal(res)
	if err != nil {
		b.opts.Logf("encoding a reply: %v", err)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	_, _ = b.out.Write(append(body, '\n'))
}

// instructions is what the client shows the model before it starts. It says
// the two things that are not obvious from any single tool description: where
// to begin on a binary with no symbols, and that the execution tools answer
// with the stop rather than with an acknowledgement.
const instructions = `gdb-wui drives a real gdb over a real program, with Ghidra decompiling it.

On a stripped binary there are no symbols to list: start with status, then the
stack, then decompile_function on a frame's address, and follow the FUN_ names
in the recovered C.

The tools that run the program return where it stopped — there is nothing to
poll for, and no read works while it runs.

What makes this different from reading a decompilation is that you can test a
guess: break on the function, run, and evaluate the expression decompile_function
gives for a variable, or read_memory at the address it names. Write what you
established with comment, so it is there next time.`

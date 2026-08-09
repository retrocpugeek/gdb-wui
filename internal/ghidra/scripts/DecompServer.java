// A resident decompilation server for gdb-wui.
//
// analyzeHeadless keeps the JVM alive for as long as a postScript runs, so a
// script that blocks reading requests *is* a server. That is the whole trick,
// and it is what makes per-function decompilation viable: measured on a 2 MB
// MIPS64 image, a fresh analyzeHeadless costs 3.5s per function of which 3.4s
// is JVM startup and project open, while the same request against a resident
// process is 100-200ms.
//
// Usage, from gdb-wui:
//   analyzeHeadless <projDir> <projName> -process '<program>' -noanalysis \
//       -readOnly -scriptPath <dir> -postScript DecompServer.java <socketPath>
//
// The transport is a unix domain socket that gdb-wui creates and this connects
// out to. Not stdout: Ghidra's logging owns that, and interleaving a protocol
// with log4j output is a parser nobody should have to write. Not TCP: a port
// is reachable by anything on the machine, and this process will decompile
// whatever it is asked to. A socket in a private directory is bounded by
// filesystem permissions instead.
//
// Requests and responses are one JSON object per line:
//
//   -> {"id":1,"op":"info"}
//   <- {"id":1,"ok":true,"program":{...},"functionCount":1415}
//   -> {"id":2,"op":"decompile","function":"0x120007ee0"}
//   <- {"id":2,"ok":true,"function":{...}}
//   -> {"id":3,"op":"functions","offset":0,"limit":500}
//   <- {"id":3,"ok":true,"functions":[{...}],"total":1415}
//   -> {"id":4,"op":"names","addresses":"0x10d2b0,0x101040"}
//   <- {"id":4,"ok":true,"names":[{"addr":"0x10d2b0","name":"FUN_0010d2b0",...}]}
//
// The addresses of "names" are one comma-separated string rather than a JSON
// array, because the parser below is deliberately hand-rolled and a list of
// hex numbers needs nothing more.
//
// A failed request is {"id":n,"ok":false,"error":"..."} and never closes the
// connection: one undecompilable function must not take down the session.
//
//@category gdb-wui

import java.io.BufferedReader;
import java.io.BufferedWriter;
import java.io.InputStreamReader;
import java.io.OutputStreamWriter;
import java.net.StandardProtocolFamily;
import java.net.UnixDomainSocketAddress;
import java.nio.channels.Channels;
import java.nio.channels.SocketChannel;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;

import ghidra.app.decompiler.DecompInterface;
import ghidra.app.decompiler.DecompileOptions;
import ghidra.app.decompiler.DecompileResults;
import ghidra.app.script.GhidraScript;
import ghidra.program.model.address.Address;
import ghidra.program.model.listing.Function;

public class DecompServer extends GhidraScript {

	private static final int DECOMPILE_TIMEOUT_SECS = 60;

	private DecompInterface decomp;
	private BufferedWriter out;

	@Override
	public void run() throws Exception {
		String[] args = getScriptArgs();
		if (args.length < 1) {
			println("DecompServer: usage: DecompServer.java <socket-path>");
			return;
		}
		if (currentProgram == null) {
			println("DecompServer: no program; -process must name one");
			return;
		}

		decomp = new DecompInterface();
		decomp.setOptions(new DecompileOptions());
		decomp.toggleCCode(true);
		decomp.toggleSyntaxTree(true);
		decomp.setSimplificationStyle("decompile");
		if (!decomp.openProgram(currentProgram)) {
			println("DecompServer: cannot open program: " + decomp.getLastMessage());
			return;
		}

		UnixDomainSocketAddress addr = UnixDomainSocketAddress.of(Path.of(args[0]));
		try (SocketChannel ch = SocketChannel.open(StandardProtocolFamily.UNIX)) {
			ch.connect(addr);
			BufferedReader in =
				new BufferedReader(new InputStreamReader(Channels.newInputStream(ch)));
			out = new BufferedWriter(new OutputStreamWriter(Channels.newOutputStream(ch)));

			// The greeting is unsolicited and carries the program identity, so
			// gdb-wui can check the sha256 against the binary gdb has loaded
			// before showing anything. Reading a decompilation of a different
			// build than the one being debugged is the failure this guards.
			send("{\"event\":\"ready\",\"schema\":" + DecompJson.SCHEMA +
				",\"program\":" + DecompJson.program(currentProgram) +
				",\"functionCount\":" + countFunctions() + "}");

			String line;
			while ((line = in.readLine()) != null) {
				if (monitor.isCancelled()) {
					break;
				}
				String trimmed = line.trim();
				if (trimmed.isEmpty()) {
					continue;
				}
				if (!handle(trimmed)) {
					break;
				}
			}
		}
		finally {
			decomp.dispose();
		}
		println("DecompServer: connection closed, exiting");
	}

	// handle answers one request. Returns false only for an explicit quit or a
	// write failure — never for a bad request, because the caller is a UI and
	// one mistyped symbol must not end the session.
	private boolean handle(String req) {
		long id = num(req, "id", -1);
		String op = field(req, "op");
		try {
			switch (op == null ? "" : op) {
				case "quit":
					return false;
				case "info":
					send("{\"id\":" + id + ",\"ok\":true,\"program\":" +
						DecompJson.program(currentProgram) +
						",\"functionCount\":" + countFunctions() + "}");
					return true;
				case "functions":
					return listFunctions(id, (int) num(req, "offset", 0),
						(int) num(req, "limit", 500), field(req, "filter"));
				case "decompile":
					return decompileOne(id, field(req, "function"));
				case "names":
					return nameFunctions(id, field(req, "addresses"));
				default:
					return fail(id, "unknown op " + op);
			}
		}
		catch (Exception e) {
			// An exception from one request is reported and the loop continues.
			return fail(id, e.getClass().getSimpleName() + ": " + e.getMessage());
		}
	}

	private boolean decompileOne(long id, String which) {
		if (which == null || which.isEmpty()) {
			return fail(id, "decompile needs a function name or address");
		}
		Function f = resolve(which);
		if (f == null) {
			return fail(id, "no function " + which);
		}
		DecompileResults res = decomp.decompileFunction(f, DECOMPILE_TIMEOUT_SECS, monitor);
		if (res == null || !res.decompileCompleted() || res.getCCodeMarkup() == null) {
			return fail(id, res == null ? "no result" : res.getErrorMessage());
		}
		return send("{\"id\":" + id + ",\"ok\":true,\"function\":" +
			DecompJson.function(f, res) + "}");
	}

	// resolve accepts an address or a name, and prefers the address reading.
	// A stripped image names functions FUN_00101020 after their address, so a
	// caller holding a program counter should not have to know which it is.
	private Function resolve(String which) {
		try {
			Address a = currentProgram.getAddressFactory().getAddress(which);
			if (a != null) {
				Function at = currentProgram.getFunctionManager().getFunctionAt(a);
				if (at == null) {
					// Not an entry point: the containing function is what a
					// program counter means.
					at = currentProgram.getFunctionManager().getFunctionContaining(a);
				}
				if (at != null) {
					return at;
				}
			}
		}
		catch (Exception ignored) {
			// Not an address; fall through to the name lookup.
		}
		for (Function f : currentProgram.getFunctionManager().getFunctions(true)) {
			if (f.getName().equals(which)) {
				return f;
			}
		}
		return null;
	}

	// MAX_NAMES is an outer bound on one naming request, well above the 128 its
	// caller sends and the 64 frames gdb will report. It is here because this
	// is a socket: the caller is trusted, but a bound that lives only in the
	// caller is not a bound.
	private static final int MAX_NAMES = 256;

	// nameFunctions answers "which function is each of these addresses in".
	//
	// The call stack of a stripped binary is a column of "?? ()", and this is
	// the only thing in the system that can fill it in. Deliberately not a
	// decompilation: it is one function-manager lookup per address, so naming a
	// whole stack costs about what writing the reply costs.
	//
	// An address in no function is omitted rather than reported. Padding
	// between functions, a PLT stub and a data address are all ordinary things
	// to find on a stack, and inventing a name for them would be worse than
	// leaving gdb's "??" in place.
	private boolean nameFunctions(long id, String addresses) {
		StringBuilder b = new StringBuilder();
		b.append("{\"id\":").append(id).append(",\"ok\":true,\"names\":[");
		int n = 0;
		if (addresses != null) {
			for (String raw : addresses.split(",")) {
				String want = raw.trim();
				if (want.isEmpty() || n >= MAX_NAMES) {
					continue;
				}
				Function f = functionAt(want);
				if (f == null) {
					continue;
				}
				if (n > 0) {
					b.append(",");
				}
				n++;
				b.append("{\"addr\":").append(DecompJson.str(want));
				b.append(",\"name\":").append(DecompJson.str(f.getName()));
				b.append(",\"entry\":").append(DecompJson.str(DecompJson.hex(f.getEntryPoint())));
				b.append(",\"signature\":").append(DecompJson.str(f.getPrototypeString(true, false)));
				b.append(",\"thunk\":").append(f.isThunk());
				b.append("}");
			}
		}
		b.append("]}");
		return send(b.toString());
	}

	// functionAt is the address half of resolve, without the name fallback: a
	// caller naming stack frames has addresses, and the fallback is a linear
	// scan of every function in the program for each one that misses.
	private Function functionAt(String which) {
		try {
			Address a = currentProgram.getAddressFactory().getAddress(which);
			if (a == null) {
				return null;
			}
			Function at = currentProgram.getFunctionManager().getFunctionAt(a);
			if (at == null) {
				at = currentProgram.getFunctionManager().getFunctionContaining(a);
			}
			return at;
		}
		catch (Exception ignored) {
			return null;
		}
	}

	private boolean listFunctions(long id, int offset, int limit, String filter) {
		if (limit <= 0 || limit > 5000) {
			limit = 500;
		}
		List<Function> all = new ArrayList<>();
		for (Function f : currentProgram.getFunctionManager().getFunctions(true)) {
			if (f.isExternal()) {
				continue;
			}
			if (filter != null && !filter.isEmpty() &&
				!f.getName().toLowerCase().contains(filter.toLowerCase())) {
				continue;
			}
			all.add(f);
		}
		StringBuilder b = new StringBuilder();
		b.append("{\"id\":").append(id).append(",\"ok\":true,\"total\":").append(all.size());
		b.append(",\"offset\":").append(offset).append(",\"functions\":[");
		for (int i = offset; i < Math.min(all.size(), offset + limit); i++) {
			Function f = all.get(i);
			if (i > offset) {
				b.append(",");
			}
			// Deliberately not decompiled: this is the index a symbol pane
			// browses, and decompiling 1415 functions to draw a list would be
			// two minutes of work nobody asked for.
			b.append("{\"name\":").append(DecompJson.str(f.getName()));
			b.append(",\"entry\":").append(DecompJson.str(DecompJson.hex(f.getEntryPoint())));
			b.append(",\"thunk\":").append(f.isThunk());
			b.append("}");
		}
		b.append("]}");
		return send(b.toString());
	}

	private int countFunctions() {
		int n = 0;
		for (Function f : currentProgram.getFunctionManager().getFunctions(true)) {
			if (!f.isExternal()) {
				n++;
			}
		}
		return n;
	}

	private boolean fail(long id, String msg) {
		return send("{\"id\":" + id + ",\"ok\":false,\"error\":" + DecompJson.str(msg) + "}");
	}

	private boolean send(String json) {
		try {
			out.write(json);
			out.write("\n");
			out.flush();
			return true;
		}
		catch (Exception e) {
			// The far end is gone. Stop rather than spin writing to a dead pipe.
			return false;
		}
	}

	// Minimal field extraction. The requests this accepts are generated by one
	// known caller and have flat, unnested string and integer fields, so a
	// scanner is enough — and it avoids a JSON dependency that would have to
	// track whatever Ghidra ships.
	private static String field(String json, String key) {
		String needle = "\"" + key + "\":";
		int i = json.indexOf(needle);
		if (i < 0) {
			return null;
		}
		int j = i + needle.length();
		while (j < json.length() && Character.isWhitespace(json.charAt(j))) {
			j++;
		}
		if (j >= json.length() || json.charAt(j) != '"') {
			return null;
		}
		StringBuilder b = new StringBuilder();
		for (j++; j < json.length(); j++) {
			char c = json.charAt(j);
			if (c == '\\' && j + 1 < json.length()) {
				char n = json.charAt(++j);
				switch (n) {
					case 'n': b.append('\n'); break;
					case 't': b.append('\t'); break;
					case 'r': b.append('\r'); break;
					case 'u':
						if (j + 4 < json.length()) {
							b.append((char) Integer.parseInt(json.substring(j + 1, j + 5), 16));
							j += 4;
						}
						break;
					default: b.append(n);
				}
			}
			else if (c == '"') {
				return b.toString();
			}
			else {
				b.append(c);
			}
		}
		return null;
	}

	private static long num(String json, String key, long dflt) {
		String needle = "\"" + key + "\":";
		int i = json.indexOf(needle);
		if (i < 0) {
			return dflt;
		}
		int j = i + needle.length();
		while (j < json.length() && Character.isWhitespace(json.charAt(j))) {
			j++;
		}
		int start = j;
		if (j < json.length() && (json.charAt(j) == '-' || json.charAt(j) == '+')) {
			j++;
		}
		while (j < json.length() && Character.isDigit(json.charAt(j))) {
			j++;
		}
		try {
			return Long.parseLong(json.substring(start, j));
		}
		catch (NumberFormatException e) {
			return dflt;
		}
	}
}

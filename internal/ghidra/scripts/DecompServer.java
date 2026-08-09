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
//       -readOnly -scriptPath <dir> -postScript DecompServer.java <socketPath> \
//       [writable]
//
// The second argument decides whether the edit ops are answered at all. It is
// not a performance switch: -readOnly does *not* protect a project. Under it
// this script can rename a function and save the file, and the change is there
// on the next open (finding 32). The only thing standing between a user's own
// Ghidra project and gdb-wui is that the caller does not pass "writable" for
// one, and that this refuses to write without it.
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
//   -> {"id":5,"op":"rename","kind":"variable","function":"0x10d2b0",
//       "symbol":"57","name":"local_10","newName":"count"}
//   <- {"id":5,"ok":true,"function":{...}}
//
// An edit replies with the whole re-decompiled function rather than an
// acknowledgement. It has to: renaming one symbol renumbers the others
// (finding 34), and retyping one reshapes the body, so a caller that patched
// its own copy would be holding stale keys for everything it did not touch.
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
import java.util.Iterator;
import java.util.List;

import ghidra.app.cmd.function.ApplyFunctionSignatureCmd;
import ghidra.app.cmd.function.FunctionRenameOption;
import ghidra.app.decompiler.DecompInterface;
import ghidra.app.decompiler.DecompileOptions;
import ghidra.app.decompiler.DecompileResults;
import ghidra.app.script.GhidraScript;
import ghidra.app.util.cparser.C.CParserUtils;
import ghidra.program.model.address.Address;
import ghidra.program.model.data.DataType;
import ghidra.program.model.data.DataTypeConflictHandler;
import ghidra.program.model.data.FunctionDefinitionDataType;
import ghidra.program.model.listing.Function;
import ghidra.program.model.pcode.HighFunctionDBUtil;
import ghidra.program.model.pcode.HighSymbol;
import ghidra.program.model.symbol.SourceType;
import ghidra.program.model.symbol.Symbol;
import ghidra.util.data.DataTypeParser;
import ghidra.util.data.DataTypeParser.AllowedDataTypes;

public class DecompServer extends GhidraScript {

	private static final int DECOMPILE_TIMEOUT_SECS = 60;

	private DecompInterface decomp;
	private BufferedWriter out;
	private boolean writable;

	@Override
	public void run() throws Exception {
		String[] args = getScriptArgs();
		if (args.length < 1) {
			println("DecompServer: usage: DecompServer.java <socket-path> [writable]");
			return;
		}
		if (currentProgram == null) {
			println("DecompServer: no program; -process must name one");
			return;
		}
		writable = args.length > 1 && "writable".equals(args[1]);
		println("DecompServer: writable=" + writable);

		// analyzeHeadless holds a transaction, named after this script, for as
		// long as the script runs — and while it is open every save fails with
		// "Unable to lock due to active transaction". A server that never
		// returns therefore has to hand that transaction back before it can
		// save anything at all. Finding 31.
		end(true);

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
			// Leave the framework holding a transaction, as it was before
			// end(true) above. Returning without one makes analyzeHeadless end
			// a transaction that is not there.
			start();
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
				case "rename":
					return rename(id, req);
				case "retype":
					return retype(id, req);
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

	// ---- editing ----
	//
	// Renaming FUN_0010d2b0 to what it actually does is most of what makes a
	// stripped binary readable, and the three things a user points at need
	// three different Ghidra APIs:
	//
	//   a function   Function.setName, or a whole prototype through
	//                ApplyFunctionSignatureCmd — which renames it as well.
	//   a local      HighFunctionDBUtil.updateDBVariable, through the *high*
	//                symbol: a decompiler local frequently has no database
	//                variable at all, and this is what creates one. Finding 38.
	//   a global     Symbol.setName on the label at its address.
	//
	// Every one of them runs in its own transaction and is saved immediately.
	// Without the save the new name lives only in this process and any crash
	// loses an afternoon's work.

	private static final String READ_ONLY =
		"this project is opened read-only: gdb-wui edits only the project it " +
			"imported itself, never one you named with -ghidra-project";

	private boolean rename(long id, String req) {
		if (!writable) {
			return fail(id, READ_ONLY);
		}
		String kind = field(req, "kind");
		String target = field(req, "function");
		Function f = functionAt(target);
		if (f == null) {
			return fail(id, "no function at " + target);
		}
		String want = field(req, "newName");
		want = want == null ? "" : want.trim();
		if (want.isEmpty()) {
			return fail(id, "a new name is required");
		}

		String err = null;
		boolean ok = false;
		// was is what the caller needs to undo this. It is captured here rather
		// than asked for beforehand because only this side can see the symbol
		// the edit actually landed on.
		String was = null;
		int tx = currentProgram.startTransaction("rename to " + want);
		try {
			if ("function".equals(kind)) {
				was = f.getName();
				f.setName(want, SourceType.USER_DEFINED);
			}
			else if ("variable".equals(kind)) {
				HighSymbol sym = symbolFor(f, field(req, "symbol"), field(req, "name"));
				if (sym == null) {
					err = stale(field(req, "name"), f);
				}
				else {
					was = sym.getName();
					HighFunctionDBUtil.updateDBVariable(sym, want, null, SourceType.USER_DEFINED);
				}
			}
			else if ("global".equals(kind)) {
				Symbol sym = globalAt(field(req, "address"), field(req, "name"));
				if (sym == null) {
					err = "no symbol named " + field(req, "name") + " at "
						+ field(req, "address");
				}
				else {
					was = sym.getName();
					sym.setName(want, SourceType.USER_DEFINED);
				}
			}
			else {
				err = "cannot rename a " + kind;
			}
			ok = err == null;
		}
		catch (Exception e) {
			err = e.getClass().getSimpleName() + ": " + e.getMessage();
		}
		finally {
			currentProgram.endTransaction(tx, ok);
		}
		if (!ok) {
			return fail(id, err);
		}
		return edited(id, f, duplicateWarning(kind, want), was, want);
	}

	private boolean retype(long id, String req) {
		if (!writable) {
			return fail(id, READ_ONLY);
		}
		String kind = field(req, "kind");
		String target = field(req, "function");
		Function f = functionAt(target);
		if (f == null) {
			return fail(id, "no function at " + target);
		}
		String text = field(req, "type");
		text = text == null ? "" : text.trim();
		if (text.isEmpty()) {
			return fail(id, "a type is required");
		}

		String err = null;
		boolean ok = false;
		String was = null;
		String now = null;
		int tx = currentProgram.startTransaction("retype to " + text);
		try {
			if ("function".equals(kind)) {
				// The whole prototype, which is what an inverse needs: it
				// carries the return type, the parameters and the name.
				was = f.getPrototypeString(true, false);
				// parseSignature answers a bad prototype with null rather than
				// an exception, even through the overload that declares one
				// (finding 36). An unchecked null here would report success and
				// change nothing, which is the worst of the three outcomes.
				FunctionDefinitionDataType def = CParserUtils.parseSignature(
					(ghidra.app.services.DataTypeManagerService) null, currentProgram, text,
					true);
				if (def == null) {
					err = "cannot read \"" + text + "\" as a C prototype";
				}
				// RENAME rather than the default RENAME_IF_DEFAULT, which
				// applies the types and quietly drops the name unless the
				// function is still called FUN_something. The user typed a
				// whole prototype; obeying half of it is worse than refusing
				// it. The calling convention is preserved because a parsed
				// prototype does not carry one and Ghidra's guess is better
				// than the default.
				else if (!new ApplyFunctionSignatureCmd(f.getEntryPoint(), def,
					SourceType.USER_DEFINED, true, false,
					DataTypeConflictHandler.DEFAULT_HANDLER, FunctionRenameOption.RENAME)
						.applyTo(currentProgram, monitor)) {
					err = "the prototype parsed but could not be applied";
				}
			}
			else if ("variable".equals(kind)) {
				HighSymbol sym = symbolFor(f, field(req, "symbol"), field(req, "name"));
				if (sym == null) {
					err = stale(field(req, "name"), f);
				}
				else {
					was = sym.getDataType() == null ? "" : sym.getDataType().getDisplayName();
					now = sym.getName();
					// The name is passed back unchanged rather than left null,
					// so a retype is only ever a retype.
					HighFunctionDBUtil.updateDBVariable(sym, sym.getName(), parseType(text),
						SourceType.USER_DEFINED);
				}
			}
			else {
				err = "cannot retype a " + kind + " yet";
			}
			ok = err == null;
		}
		catch (Exception e) {
			// InvalidDataTypeException carries the only useful description of
			// what is wrong with a type string, so it is passed through whole.
			// Finding 37.
			err = e.getMessage() == null ? e.getClass().getSimpleName() : e.getMessage();
		}
		finally {
			currentProgram.endTransaction(tx, ok);
		}
		if (!ok) {
			return fail(id, err);
		}
		// A signature carries a name, so applying one renames the function too.
		return edited(id, f, duplicateWarning(kind, f.getName()), was,
			now == null ? f.getName() : now);
	}

	private DataType parseType(String text) throws Exception {
		return new DataTypeParser(currentProgram.getDataTypeManager(),
			currentProgram.getDataTypeManager(), null, AllowedDataTypes.ALL).parse(text);
	}

	// edited saves and answers with the function decompiled afresh.
	//
	// The whole function, because an edit is not local to what was edited: a
	// rename renumbers the other symbols (finding 34) and a retype reshapes the
	// body around it. A caller that patched its own copy would hold keys that
	// no longer address anything.
	//
	// was and now are what an undo needs: the value before the edit, and the
	// name the symbol answers to afterwards. Only this side can report them —
	// the caller does not know which symbol the edit landed on when its id was
	// stale and the name matched instead.
	private boolean edited(long id, Function f, String warning, String was, String now) {
		String saveErr = save();
		if (saveErr != null) {
			warning = warning == null ? saveErr : warning + "; " + saveErr;
		}
		DecompileResults res = decomp.decompileFunction(f, DECOMPILE_TIMEOUT_SECS, monitor);
		if (res == null || !res.decompileCompleted() || res.getCCodeMarkup() == null) {
			return fail(id, "the edit was made, but the function no longer decompiles: "
				+ (res == null ? "no result" : res.getErrorMessage()));
		}
		StringBuilder b = new StringBuilder();
		b.append("{\"id\":").append(id).append(",\"ok\":true,\"function\":")
			.append(DecompJson.function(f, res));
		if (warning != null) {
			b.append(",\"warning\":").append(DecompJson.str(warning));
		}
		if (was != null) {
			b.append(",\"was\":").append(DecompJson.str(was));
		}
		if (now != null) {
			b.append(",\"now\":").append(DecompJson.str(now));
		}
		return send(b.append("}").toString());
	}

	// save is what makes an edit outlive this process. A failure is reported as
	// a warning rather than an error: the rename did happen, the user can see
	// it, and calling that a failure would be the wrong half of the truth.
	private String save() {
		try {
			currentProgram.save("gdb-wui edit", monitor);
			return null;
		}
		catch (Exception e) {
			return "the edit was made but could not be saved: " + e.getMessage();
		}
	}

	// symbolFor finds the symbol an edit names.
	//
	// The id first and the name second, because an edit renumbers the ids of
	// the symbols it did not touch (finding 34), so a client's id is routinely
	// one edit out of date while its name is still right. Finding nothing is an
	// error and never a nearest match: renaming the wrong variable is worse
	// than refusing to rename anything.
	private HighSymbol symbolFor(Function f, String symbolID, String name) {
		DecompileResults res = decomp.decompileFunction(f, DECOMPILE_TIMEOUT_SECS, monitor);
		if (res == null || res.getHighFunction() == null) {
			return null;
		}
		HighSymbol byName = null;
		Iterator<HighSymbol> it = res.getHighFunction().getLocalSymbolMap().getSymbols();
		while (it.hasNext()) {
			HighSymbol sym = it.next();
			if (symbolID != null && !symbolID.isEmpty()
				&& symbolID.equals(Long.toString(sym.getId()))) {
				return sym;
			}
			if (name != null && name.equals(sym.getName())) {
				byName = sym;
			}
		}
		return byName;
	}

	private String stale(String name, Function f) {
		return "no variable " + name + " in " + f.getName()
			+ " any more; the decompiled view is out of date";
	}

	private Symbol globalAt(String address, String name) {
		try {
			Address a = currentProgram.getAddressFactory().getAddress(address);
			if (a == null) {
				return null;
			}
			for (Symbol sym : currentProgram.getSymbolTable().getSymbols(a)) {
				if (name == null || name.isEmpty() || name.equals(sym.getName())) {
					return sym;
				}
			}
		}
		catch (Exception ignored) {
			// Not an address, so not a global this knows about.
		}
		return null;
	}

	// duplicateWarning exists because Ghidra accepts two functions with the
	// same name without a word (finding 35). It is not refused here either —
	// two thunks to one routine really do share a name — but a user who has
	// just made a name ambiguous is owed the news.
	private String duplicateWarning(String kind, String name) {
		if (!"function".equals(kind)) {
			return null;
		}
		int n = currentProgram.getSymbolTable().getGlobalSymbols(name).size();
		if (n <= 1) {
			return null;
		}
		return n + " symbols are now named " + name;
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

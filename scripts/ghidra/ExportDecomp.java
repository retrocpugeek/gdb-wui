// Export decompiled C, with an address map, for gdb-wui to show beside a live
// gdb session.
//
// gdb-wui never links or embeds Ghidra: it runs analyzeHeadless as a separate
// process, exactly as it runs gdb, and reads the JSON this leaves behind. So
// the whole integration surface is this file and the schema it writes, which is
// documented in docs/decompilation.md.
//
// Usage:
//   analyzeHeadless <projdir> <proj> -import <binary> \
//       -scriptPath scripts/ghidra -postScript ExportDecomp.java <out.json>
//
// Optional second argument: a regular expression, and only functions whose
// names match it are decompiled. A firmware image has thousands of functions
// and decompiling all of them is minutes of work, so being able to ask for a
// subset is the difference between a usable prototype and a batch job.
//
// The two things that make the output useful, and the reasons they are done
// this way:
//
//   Text and line numbers come from the same PrettyPrinter.getLines() call.
//   Emitting getC() and separately walking the token tree would be two
//   renderings of one function, and any disagreement between them puts the
//   highlight on the wrong line — a failure that looks like a decompiler bug
//   rather than an export bug.
//
//   Each line records every address its tokens carry, not just a min and a max.
//   A decompiled line's addresses are frequently disjoint, and consecutive
//   lines interleave: a range would claim instructions belonging to a different
//   line, which is exactly the case where a user is trying to work out what the
//   compiler did and can least afford to be lied to.
//
//@category gdb-wui

import java.io.BufferedWriter;
import java.io.FileWriter;
import java.io.PrintWriter;
import java.util.ArrayList;
import java.util.Iterator;
import java.util.List;
import java.util.TreeSet;
import java.util.regex.Pattern;

import ghidra.app.decompiler.ClangCommentToken;
import ghidra.app.decompiler.ClangLabelToken;
import ghidra.app.decompiler.ClangLine;
import ghidra.app.decompiler.ClangToken;
import ghidra.app.decompiler.DecompInterface;
import ghidra.app.decompiler.DecompileOptions;
import ghidra.app.decompiler.DecompileResults;
import ghidra.app.decompiler.PrettyPrinter;
import ghidra.app.script.GhidraScript;
import ghidra.program.model.address.Address;
import ghidra.program.model.listing.Function;
import ghidra.program.model.listing.StackFrame;
import ghidra.program.model.listing.VariableStorage;
import ghidra.program.model.pcode.HighFunction;
import ghidra.program.model.pcode.HighSymbol;
import ghidra.program.model.symbol.IdentityNameTransformer;

public class ExportDecomp extends GhidraScript {

	// SCHEMA is the version of the emitted document. The consumer refuses a
	// number it does not know rather than guessing at fields, because a cached
	// sidecar outlives the code that wrote it.
	private static final int SCHEMA = 1;

	// One function should not be able to stall a whole image.
	private static final int DECOMPILE_TIMEOUT_SECS = 60;

	@Override
	public void run() throws Exception {
		String[] args = getScriptArgs();
		if (args.length < 1) {
			println("ExportDecomp: usage: ExportDecomp.java <out.json> [nameRegex]");
			return;
		}
		Pattern filter = args.length > 1 ? Pattern.compile(args[1]) : null;

		DecompInterface decomp = new DecompInterface();
		decomp.setOptions(new DecompileOptions());
		decomp.toggleCCode(true);
		decomp.toggleSyntaxTree(true);
		decomp.setSimplificationStyle("decompile");
		if (!decomp.openProgram(currentProgram)) {
			println("ExportDecomp: cannot open program: " + decomp.getLastMessage());
			return;
		}

		List<Function> wanted = new ArrayList<>();
		for (Function f : currentProgram.getFunctionManager().getFunctions(true)) {
			if (f.isExternal() || f.isThunk()) {
				continue;
			}
			if (filter != null && !filter.matcher(f.getName()).find()) {
				continue;
			}
			wanted.add(f);
		}
		println("ExportDecomp: " + wanted.size() + " function(s) to decompile");

		long started = System.currentTimeMillis();
		int done = 0, failed = 0;

		try (PrintWriter out = new PrintWriter(new BufferedWriter(new FileWriter(args[0])))) {
			out.println("{");
			out.println("  \"schema\": " + SCHEMA + ",");
			writeGenerator(out);
			writeProgram(out);
			out.println("  \"functions\": [");

			boolean first = true;
			for (Function f : wanted) {
				if (monitor.isCancelled()) {
					break;
				}
				monitor.setMessage("decompiling " + f.getName());
				DecompileResults res =
					decomp.decompileFunction(f, DECOMPILE_TIMEOUT_SECS, monitor);
				// A function the decompiler refuses is reported and skipped.
				// Dropping it silently would leave the consumer unable to tell
				// "not decompiled" from "not present".
				if (res == null || !res.decompileCompleted() || res.getCCodeMarkup() == null) {
					failed++;
					println("ExportDecomp: " + f.getName() + ": " +
						(res == null ? "no result" : res.getErrorMessage()));
					continue;
				}
				if (!first) {
					out.println(",");
				}
				first = false;
				writeFunction(out, f, res);
				done++;
				if (done % 200 == 0) {
					println("ExportDecomp: " + done + "/" + wanted.size());
				}
			}
			out.println();
			out.println("  ]");
			out.println("}");
		}
		finally {
			decomp.dispose();
		}

		long ms = System.currentTimeMillis() - started;
		println("ExportDecomp: wrote " + args[0] + " — " + done + " function(s), " +
			failed + " failed, " + ms + " ms");
	}

	private void writeGenerator(PrintWriter out) {
		out.println("  \"generator\": {");
		out.println("    \"tool\": \"ghidra\",");
		out.println("    \"version\": " + str(ghidra.framework.Application.getApplicationVersion()) + ",");
		out.println("    \"script\": \"ExportDecomp\"");
		out.println("  },");
	}

	private void writeProgram(PrintWriter out) {
		// imageBase and sha256 are the two fields the consumer cannot do
		// without: the first because Ghidra's addresses are link-time and gdb's
		// are not, the second because a cache keyed on a path would happily
		// serve a stale decompilation of a rebuilt binary.
		out.println("  \"program\": {");
		out.println("    \"name\": " + str(currentProgram.getName()) + ",");
		out.println("    \"path\": " + str(currentProgram.getExecutablePath()) + ",");
		out.println("    \"format\": " + str(currentProgram.getExecutableFormat()) + ",");
		out.println("    \"sha256\": " + str(currentProgram.getExecutableSHA256()) + ",");
		out.println("    \"languageId\": " +
			str(currentProgram.getLanguageID().getIdAsString()) + ",");
		out.println("    \"compilerSpec\": " +
			str(currentProgram.getCompilerSpec().getCompilerSpecID().getIdAsString()) + ",");
		out.println("    \"pointerSize\": " +
			currentProgram.getDefaultPointerSize() + ",");
		out.println("    \"imageBase\": " + str(hex(currentProgram.getImageBase())));
		out.println("  },");
	}

	private void writeFunction(PrintWriter out, Function f, DecompileResults res) {
		PrettyPrinter printer =
			new PrettyPrinter(f, res.getCCodeMarkup(), new IdentityNameTransformer());
		List<ClangLine> lines = printer.getLines();

		out.println("    {");
		out.println("      \"name\": " + str(f.getName()) + ",");
		out.println("      \"entry\": " + str(hex(f.getEntryPoint())) + ",");
		out.println("      \"bodyStart\": " + str(hex(f.getBody().getMinAddress())) + ",");
		out.println("      \"bodyEnd\": " + str(hex(f.getBody().getMaxAddress())) + ",");
		out.println("      \"signature\": " + str(f.getPrototypeString(true, false)) + ",");
		writeFrame(out, f);
		writeVariables(out, res.getHighFunction());

		// The text is rebuilt from the very lines the map indexes, so line N of
		// this string is line N of the map by construction.
		StringBuilder text = new StringBuilder();
		for (ClangLine line : lines) {
			text.append(line.getIndentString()).append(PrettyPrinter.getText(line)).append('\n');
		}
		out.println("      \"lineCount\": " + lines.size() + ",");
		out.println("      \"text\": " + str(text.toString()) + ",");

		// Only lines that carry addresses are emitted; the rest are braces,
		// declarations and blank lines, and a null for each of them is bulk
		// with no information in it.
		out.println("      \"lines\": [");
		boolean firstLine = true;
		for (int i = 0; i < lines.size(); i++) {
			TreeSet<Long> addrs = new TreeSet<>();
			for (ClangToken tok : lines.get(i).getAllTokens()) {
				// A comment carries the address of the statement it annotates,
				// which would make the decompiler's own "WARNING: Subroutine
				// does not return" line claim the call below it. Measured on
				// build/structs: five of the six addresses claimed by two
				// lines were a comment and its statement. A comment is not
				// code and must never be highlighted as the program counter.
				if (tok instanceof ClangCommentToken || tok instanceof ClangLabelToken) {
					continue;
				}
				Address min = tok.getMinAddress();
				if (min != null && min.getAddressSpace().isMemorySpace()) {
					addrs.add(min.getOffset());
				}
				Address max = tok.getMaxAddress();
				if (max != null && max.getAddressSpace().isMemorySpace()) {
					addrs.add(max.getOffset());
				}
			}
			if (addrs.isEmpty()) {
				continue;
			}
			if (!firstLine) {
				out.println(",");
			}
			firstLine = false;
			// n is 1-based, matching how every editor and gdb count lines.
			out.print("        {\"n\": " + (i + 1) + ", \"addrs\": [");
			Iterator<Long> it = addrs.iterator();
			while (it.hasNext()) {
				out.print(str(hex(it.next())));
				if (it.hasNext()) {
					out.print(", ");
				}
			}
			out.print("]}");
		}
		out.println();
		out.println("      ]");
		out.print("    }");
	}

	// writeFrame records what Ghidra believes about the stack layout. A stack
	// variable's offset below is relative to this frame, not to any register
	// gdb knows, so without these numbers the offsets cannot be turned into
	// something gdb can evaluate.
	private void writeFrame(PrintWriter out, Function f) {
		StackFrame frame = f.getStackFrame();
		out.println("      \"frame\": {");
		out.println("        \"size\": " + frame.getFrameSize() + ",");
		out.println("        \"localSize\": " + frame.getLocalSize() + ",");
		out.println("        \"paramOffset\": " + frame.getParameterOffset() + ",");
		out.println("        \"returnAddressOffset\": " + frame.getReturnAddressOffset() + ",");
		out.println("        \"growsNegative\": " + frame.growsNegative());
		out.println("      },");
	}

	// writeVariables is what a later milestone needs to show live values in the
	// decompiled view. Three storage kinds come out of the decompiler and they
	// are not equally useful:
	//
	//   stack     — a real location, once the frame base is reconciled.
	//   register  — a real location, but only near pc: the register is reused.
	//   unique    — a decompiler temporary that exists nowhere in the machine.
	//
	// The last one is the reason this has to be recorded honestly rather than
	// guessed at from the name. `lVar1` looks exactly like `local_58` in the C
	// text, and one of them can be shown while the other never can.
	private void writeVariables(PrintWriter out, HighFunction high) {
		out.println("      \"variables\": [");
		if (high == null) {
			out.println("      ],");
			return;
		}
		boolean first = true;
		Iterator<HighSymbol> syms = high.getLocalSymbolMap().getSymbols();
		while (syms.hasNext()) {
			HighSymbol sym = syms.next();
			if (!first) {
				out.println(",");
			}
			first = false;
			VariableStorage store = sym.getStorage();
			out.print("        {\"name\": " + str(sym.getName()));
			out.print(", \"type\": " +
				str(sym.getDataType() == null ? "" : sym.getDataType().getDisplayName()));
			out.print(", \"size\": " + sym.getSize());
			out.print(", \"param\": " + sym.isParameter());
			Address pc = sym.getPCAddress();
			out.print(", \"pc\": " + (pc == null || !pc.getAddressSpace().isMemorySpace()
				? "null" : str(hex(pc))));
			out.print(", \"storage\": ");
			if (store == null) {
				out.print("{\"kind\": \"none\"}");
			}
			else if (store.isStackStorage()) {
				out.print("{\"kind\": \"stack\", \"offset\": " + store.getStackOffset() + "}");
			}
			else if (store.isRegisterStorage()) {
				out.print("{\"kind\": \"register\", \"register\": " +
					str(store.getRegister().getName()) + "}");
			}
			else if (store.isUniqueStorage()) {
				out.print("{\"kind\": \"unique\"}");
			}
			else {
				// Anything else — a memory global, a multi-piece value — is
				// recorded as its Ghidra spelling rather than dropped, so the
				// consumer can decide and this script does not have to know
				// every case in advance.
				out.print("{\"kind\": \"other\", \"text\": " + str(store.toString()) + "}");
			}
			out.print("}");
		}
		out.println();
		out.println("      ],");
	}

	private static String hex(Address a) {
		return a == null ? "" : hex(a.getOffset());
	}

	private static String hex(long v) {
		return "0x" + Long.toHexString(v);
	}

	// str writes a JSON string. Hand-rolled rather than pulled from a library:
	// this script has to compile against whatever Ghidra ships, and a
	// dependency that moves between releases is a poor trade for twenty lines.
	private static String str(String s) {
		if (s == null) {
			return "null";
		}
		StringBuilder b = new StringBuilder(s.length() + 16);
		b.append('"');
		for (int i = 0; i < s.length(); i++) {
			char c = s.charAt(i);
			switch (c) {
				case '"':
					b.append("\\\"");
					break;
				case '\\':
					b.append("\\\\");
					break;
				case '\n':
					b.append("\\n");
					break;
				case '\r':
					b.append("\\r");
					break;
				case '\t':
					b.append("\\t");
					break;
				default:
					// Decompiled text can contain bytes lifted straight out of
					// the binary, so anything unprintable is escaped rather
					// than emitted raw into a JSON document.
					if (c < 0x20 || c == 0x7f) {
						b.append(String.format("\\u%04x", (int) c));
					}
					else {
						b.append(c);
					}
			}
		}
		b.append('"');
		return b.toString();
	}
}

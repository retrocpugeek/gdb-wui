// JSON emission shared by the batch exporter and the resident server.
//
// Not a GhidraScript — a plain class alongside them, so both spell the schema
// exactly once. Two copies of this would drift, and the failure would be a
// pane that renders correctly against one producer and not the other.
//
// The schema is documented in docs/decompilation.md.

import java.util.Iterator;
import java.util.List;
import java.util.TreeSet;

import ghidra.app.decompiler.ClangCommentToken;
import ghidra.app.decompiler.ClangLabelToken;
import ghidra.app.decompiler.ClangLine;
import ghidra.app.decompiler.ClangToken;
import ghidra.app.decompiler.DecompileResults;
import ghidra.app.decompiler.PrettyPrinter;
import ghidra.program.model.address.Address;
import ghidra.program.model.listing.Function;
import ghidra.program.model.listing.Program;
import ghidra.program.model.listing.StackFrame;
import ghidra.program.model.listing.VariableStorage;
import ghidra.program.model.pcode.HighFunction;
import ghidra.program.model.pcode.HighSymbol;
import ghidra.program.model.symbol.IdentityNameTransformer;

public class DecompJson {

	// SCHEMA is the version of the emitted document. A consumer refuses a
	// number it does not know rather than guessing at fields, because a cached
	// sidecar outlives the code that wrote it.
	public static final int SCHEMA = 1;

	/** The `program` object: what a consumer needs to relate this to a live gdb. */
	public static String program(Program p) {
		StringBuilder b = new StringBuilder();
		b.append("{");
		// sha256 is the cache key and the mismatch guard. Keyed on a path, a
		// cache would happily serve a stale decompilation of a rebuilt binary;
		// worse, it would let a user read one build while debugging another.
		b.append("\"name\":").append(str(p.getName()));
		b.append(",\"path\":").append(str(p.getExecutablePath()));
		b.append(",\"format\":").append(str(p.getExecutableFormat()));
		b.append(",\"sha256\":").append(str(p.getExecutableSHA256()));
		b.append(",\"languageId\":").append(str(p.getLanguageID().getIdAsString()));
		b.append(",\"compilerSpec\":")
			.append(str(p.getCompilerSpec().getCompilerSpecID().getIdAsString()));
		b.append(",\"pointerSize\":").append(p.getDefaultPointerSize());
		// Ghidra's addresses are link-time; gdb's are not. See the relocation
		// section of docs/decompilation.md — the bias is not computed from
		// this, but a consumer cannot even notice the problem without it.
		b.append(",\"imageBase\":").append(str(hex(p.getImageBase())));
		b.append("}");
		return b.toString();
	}

	/** One function: text, the address map, the frame, and variable storage. */
	public static String function(Function f, DecompileResults res) {
		PrettyPrinter printer =
			new PrettyPrinter(f, res.getCCodeMarkup(), new IdentityNameTransformer());
		List<ClangLine> lines = printer.getLines();

		StringBuilder b = new StringBuilder();
		b.append("{");
		b.append("\"name\":").append(str(f.getName()));
		b.append(",\"entry\":").append(str(hex(f.getEntryPoint())));
		b.append(",\"bodyStart\":").append(str(hex(f.getBody().getMinAddress())));
		b.append(",\"bodyEnd\":").append(str(hex(f.getBody().getMaxAddress())));
		b.append(",\"signature\":").append(str(f.getPrototypeString(true, false)));
		b.append(",\"frame\":").append(frame(f));
		b.append(",\"variables\":").append(variables(res.getHighFunction()));
		b.append(",\"globals\":").append(globals(res.getHighFunction()));

		// The text is rebuilt from the very lines the map indexes, so line N of
		// this string is line N of the map by construction. Emitting getC() and
		// separately walking the token tree would be two renderings of one
		// function, and any disagreement puts the highlight on the wrong line.
		StringBuilder text = new StringBuilder();
		for (ClangLine line : lines) {
			text.append(line.getIndentString()).append(PrettyPrinter.getText(line)).append('\n');
		}
		b.append(",\"lineCount\":").append(lines.size());
		b.append(",\"text\":").append(str(text.toString()));
		b.append(",\"lines\":").append(lineMap(lines));
		b.append("}");
		return b.toString();
	}

	// lineMap records, per line, every address its tokens carry — a set, not a
	// range. A decompiled line's addresses are routinely disjoint and
	// consecutive lines interleave, so a min/max range would claim
	// instructions belonging to a different line.
	private static String lineMap(List<ClangLine> lines) {
		StringBuilder b = new StringBuilder("[");
		boolean first = true;
		for (int i = 0; i < lines.size(); i++) {
			TreeSet<Long> addrs = new TreeSet<>();
			for (ClangToken tok : lines.get(i).getAllTokens()) {
				// A comment carries the address of the statement it annotates,
				// and a `goto LAB_x` carries the label's. Both are references,
				// not code generated for this line, and both would put the
				// program counter on a line that is not executing.
				if (tok instanceof ClangCommentToken || tok instanceof ClangLabelToken) {
					continue;
				}
				addAddr(addrs, tok.getMinAddress());
				addAddr(addrs, tok.getMaxAddress());
			}
			if (addrs.isEmpty()) {
				continue;
			}
			if (!first) {
				b.append(",");
			}
			first = false;
			// n is 1-based, matching how every editor and gdb count lines.
			b.append("{\"n\":").append(i + 1).append(",\"addrs\":[");
			Iterator<Long> it = addrs.iterator();
			while (it.hasNext()) {
				b.append(str(hex(it.next())));
				if (it.hasNext()) {
					b.append(",");
				}
			}
			b.append("]}");
		}
		return b.append("]").toString();
	}

	private static void addAddr(TreeSet<Long> into, Address a) {
		if (a != null && a.getAddressSpace().isMemorySpace()) {
			into.add(a.getOffset());
		}
	}

	// frame records what Ghidra believes about the stack layout. A stack
	// variable's offset is relative to this frame, not to any register gdb
	// knows, so without these numbers the offsets cannot be turned into
	// something gdb can evaluate.
	private static String frame(Function f) {
		StackFrame fr = f.getStackFrame();
		return "{\"size\":" + fr.getFrameSize() +
			",\"localSize\":" + fr.getLocalSize() +
			",\"paramOffset\":" + fr.getParameterOffset() +
			",\"returnAddressOffset\":" + fr.getReturnAddressOffset() +
			",\"growsNegative\":" + fr.growsNegative() + "}";
	}

	// variables is what a live decompiled view needs. Three storage kinds come
	// out of the decompiler and they are not equally useful:
	//
	//   stack     — a real location, once the frame base is reconciled.
	//   register  — a real location, but only near pc: the register is reused.
	//   unique    — a decompiler temporary that exists nowhere in the machine.
	//
	// The last is why this is recorded rather than guessed from the name.
	// `lVar1` looks exactly like `local_58` in the C text, and one of them can
	// be shown while the other never can.
	private static String variables(HighFunction high) {
		if (high == null) {
			return "[]";
		}
		StringBuilder b = new StringBuilder("[");
		boolean first = true;
		Iterator<HighSymbol> syms = high.getLocalSymbolMap().getSymbols();
		while (syms.hasNext()) {
			HighSymbol sym = syms.next();
			if (!first) {
				b.append(",");
			}
			first = false;
			b.append("{\"name\":").append(str(sym.getName()));
			b.append(",\"type\":").append(
				str(sym.getDataType() == null ? "" : sym.getDataType().getDisplayName()));
			b.append(",\"size\":").append(sym.getSize());
			b.append(",\"param\":").append(sym.isParameter());
			Address pc = sym.getPCAddress();
			b.append(",\"pc\":").append(
				pc == null || !pc.getAddressSpace().isMemorySpace() ? "null" : str(hex(pc)));
			b.append(",\"storage\":").append(storage(sym.getStorage()));
			b.append("}");
		}
		return b.append("]").toString();
	}

	// globals lists the module-scope symbols this function touches.
	//
	// A separate map from the locals, and easy to miss: getLocalSymbolMap()
	// holds only the frame, so a decompiled body full of counters and flags
	// yields nothing addressable for any of them. They are the *most* readable
	// things in the function — a fixed address, live whether or not the frame
	// is set up, and valid at every pc — so leaving them out gets the value
	// backwards.
	private static String globals(HighFunction high) {
		if (high == null) {
			return "[]";
		}
		StringBuilder b = new StringBuilder("[");
		boolean first = true;
		Iterator<HighSymbol> syms = high.getGlobalSymbolMap().getSymbols();
		while (syms.hasNext()) {
			HighSymbol sym = syms.next();
			VariableStorage store = sym.getStorage();
			// Only a real memory location is useful. Anything else here is not
			// something a debugger can read.
			if (store == null || !store.isMemoryStorage()) {
				continue;
			}
			if (!first) {
				b.append(",");
			}
			first = false;
			b.append("{\"name\":").append(str(sym.getName()));
			b.append(",\"type\":").append(
				str(sym.getDataType() == null ? "" : sym.getDataType().getDisplayName()));
			b.append(",\"size\":").append(sym.getSize());
			b.append(",\"address\":").append(str(hex(store.getMinAddress())));
			b.append("}");
		}
		return b.append("]").toString();
	}

	private static String storage(VariableStorage s) {
		if (s == null) {
			return "{\"kind\":\"none\"}";
		}
		if (s.isStackStorage()) {
			return "{\"kind\":\"stack\",\"offset\":" + s.getStackOffset() + "}";
		}
		if (s.isRegisterStorage()) {
			return "{\"kind\":\"register\",\"register\":" + str(s.getRegister().getName()) + "}";
		}
		if (s.isUniqueStorage()) {
			return "{\"kind\":\"unique\"}";
		}
		// Anything else — a memory global, a multi-piece value — is recorded in
		// Ghidra's own spelling rather than dropped, so the consumer can decide
		// and this class does not have to know every case in advance.
		return "{\"kind\":\"other\",\"text\":" + str(s.toString()) + "}";
	}

	public static String hex(Address a) {
		return a == null ? "" : hex(a.getOffset());
	}

	public static String hex(long v) {
		return "0x" + Long.toHexString(v);
	}

	// str writes a JSON string. Hand-rolled rather than pulled from a library:
	// this has to compile against whatever Ghidra ships, and a dependency that
	// moves between releases is a poor trade for twenty lines.
	public static String str(String s) {
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
		return b.append('"').toString();
	}
}

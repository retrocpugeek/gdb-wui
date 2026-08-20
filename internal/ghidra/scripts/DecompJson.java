// JSON emission shared by the batch exporter and the resident server.
//
// Not a GhidraScript — a plain class alongside them, so both spell the schema
// exactly once. Two copies of this would drift, and the failure would be a
// pane that renders correctly against one producer and not the other.
//
// The schema is documented in docs/decompilation.md.

import java.util.HashMap;
import java.util.Iterator;
import java.util.List;
import java.util.Map;
import java.util.TreeSet;

import ghidra.app.cmd.function.CallDepthChangeInfo;
import ghidra.app.decompiler.ClangCommentToken;
import ghidra.app.decompiler.ClangLabelToken;
import ghidra.app.decompiler.ClangLine;
import ghidra.app.decompiler.ClangToken;
import ghidra.app.decompiler.DecompileResults;
import ghidra.app.decompiler.PrettyPrinter;
import ghidra.program.model.address.Address;
import ghidra.program.model.address.AddressIterator;
import ghidra.program.model.listing.Bookmark;
import ghidra.program.model.listing.CommentType;
import ghidra.program.model.listing.Function;
import ghidra.program.model.listing.Instruction;
import ghidra.program.model.listing.InstructionIterator;
import ghidra.program.model.listing.Listing;
import ghidra.program.model.listing.Program;
import ghidra.program.model.listing.StackFrame;
import ghidra.program.model.listing.VariableStorage;
import ghidra.program.model.mem.MemoryBlock;
import ghidra.program.model.pcode.HighFunction;
import ghidra.program.model.pcode.HighSymbol;
import ghidra.program.model.symbol.IdentityNameTransformer;
import ghidra.program.model.symbol.Symbol;
import ghidra.util.task.TaskMonitor;

public class DecompJson {

	// SCHEMA is the version of the emitted document. A consumer refuses a
	// number it does not know rather than guessing at fields, because a cached
	// sidecar outlives the code that wrote it.
	//
	// 2 added frame.spDepth. An optional field, but the number still moved:
	// absent means "no settled depth" to a reader that knows about it, and a
	// document written before it existed would say that about every function.
	public static final int SCHEMA = 2;

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
		// Where the name came from, in Ghidra's own vocabulary: USER_DEFINED,
		// ANALYSIS, IMPORTED or DEFAULT. A consumer that wants to say "you
		// named this" rather than "something inferred it" has no other way to
		// tell, and inventing the distinction on the client would be guessing.
		b.append(",\"source\":").append(str(source(f.getSymbol())));
		b.append(",\"frame\":").append(frame(f, res.getHighFunction()));
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
		b.append(",\"commentLines\":").append(commentLines(lines));
		b.append(",\"comments\":").append(comments(f));
		b.append("}");
		return b.toString();
	}

	// commentLines says which rendered lines are wholly comment, and which
	// address each of them belongs to.
	//
	// Which, taken from the markup rather than from the text, because the text
	// cannot be trusted to say: `puts("/* not a comment */")` decompiles to a
	// line that any prefix test calls a comment, and a comment longer than the
	// print width is wrapped across several lines of which only the first would
	// match. A token either is a ClangCommentToken or it is not.
	//
	// The address, so that pointing at a comment is a way of editing it. A
	// comment token carries the address of the statement it annotates — which
	// is why lineMap above throws those addresses away, and why they are worth
	// keeping here. A decompiler warning belongs to no address and reports
	// none.
	private static String commentLines(List<ClangLine> lines) {
		StringBuilder b = new StringBuilder("[");
		boolean first = true;
		for (int i = 0; i < lines.size(); i++) {
			boolean any = false;
			boolean all = true;
			Address at = null;
			for (ClangToken tok : lines.get(i).getAllTokens()) {
				// The indentation and the separators between comment tokens are
				// syntax tokens holding nothing but spaces; a line of them is
				// blank rather than commented.
				if (tok.getText() == null || tok.getText().isBlank()) {
					continue;
				}
				any = true;
				if (!(tok instanceof ClangCommentToken)) {
					all = false;
					break;
				}
				Address a = tok.getMinAddress();
				if (a != null && a.getAddressSpace().isMemorySpace()
					&& (at == null || a.compareTo(at) < 0)) {
					at = a;
				}
			}
			if (!any || !all) {
				continue;
			}
			if (!first) {
				b.append(",");
			}
			first = false;
			b.append("{\"n\":").append(i + 1);
			if (at != null) {
				b.append(",\"addr\":").append(str(hex(at)));
			}
			b.append("}");
		}
		return b.append("]").toString();
	}

	// comments is what is stored in the program, as opposed to how it was
	// rendered above.
	//
	// Both are needed and they are different things. The rendering is wrapped
	// to the print width and decorated with /* */, so reconstructing what
	// someone typed from the lines they produced is guesswork; an editor has to
	// be given the text itself. The listing is the only place it exists.
	//
	// PRE comments are the ones the decompiler prints in the body (its
	// PLATE, POST and EOL display options are off by default), and the PLATE
	// comment on the entry point is the one it prints as the function's header.
	// Anything else stored against these addresses is deliberately not offered
	// for editing here, because it would not appear in what the user is
	// reading. Finding 39.
	private static String comments(Function f) {
		Program p = f.getProgram();
		Listing listing = p.getListing();
		StringBuilder b = new StringBuilder("[");
		boolean first = true;
		String plate = listing.getComment(CommentType.PLATE, f.getEntryPoint());
		if (plate != null) {
			first = false;
			b.append(comment(f.getEntryPoint(), "plate", plate, author(p, f.getEntryPoint())));
		}
		AddressIterator it =
			listing.getCommentAddressIterator(CommentType.PRE, f.getBody(), true);
		while (it.hasNext()) {
			Address a = it.next();
			String text = listing.getComment(CommentType.PRE, a);
			if (text == null) {
				continue;
			}
			if (!first) {
				b.append(",");
			}
			first = false;
			b.append(comment(a, "pre", text, author(p, a)));
		}
		return b.append("]").toString();
	}

	private static String comment(Address at, String kind, String text, String author) {
		return "{\"addr\":" + str(hex(at)) + ",\"kind\":" + str(kind) +
			",\"text\":" + str(text) + ",\"author\":" + str(author) + "}";
	}

	// source is where a name came from, or "" when there is no symbol to ask.
	private static String source(Symbol sym) {
		return sym == null || sym.getSource() == null ? "" : sym.getSource().name();
	}

	// author is who wrote a comment. The listing stores text and nothing else,
	// so the authorship is carried beside it as a bookmark the server writes;
	// no bookmark means a person wrote it. Finding 40.
	private static String author(Program p, Address at) {
		Bookmark mark = p.getBookmarkManager().getBookmark(at, "Note", "gdb-wui/agent");
		return mark == null ? "" : "agent";
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
	private static String frame(Function f, HighFunction high) {
		StackFrame fr = f.getStackFrame();
		StringBuilder b = new StringBuilder();
		b.append("{\"size\":").append(fr.getFrameSize());
		b.append(",\"localSize\":").append(fr.getLocalSize());
		b.append(",\"paramOffset\":").append(fr.getParameterOffset());
		b.append(",\"returnAddressOffset\":").append(fr.getReturnAddressOffset());
		b.append(",\"growsNegative\":").append(fr.growsNegative());
		Integer depth = spDepth(f, high);
		if (depth != null) {
			b.append(",\"spDepth\":").append(depth);
		}
		return b.append("}").toString();
	}

	// spDepth is where the stack pointer sits, relative to the frame base, over
	// the body of the function. Negative on a stack that grows down.
	//
	// This is the number that turns a stack offset into an address, and
	// frame.size is not it. Ghidra's frame size is derived from the variables
	// it found rather than from the prologue: for `accumulate`, built for ARM,
	// the prologue moves the stack pointer 24 bytes and the frame size is 20,
	// because the lowest slot any instruction touches is 20 below the base.
	// Four bytes of error reads as a neighbouring variable's value.
	//
	// The depth changes through the prologue and back again in the epilogue, so
	// what is emitted is the depth the body settles at — the most common one
	// across the function's instructions. A function that never settles gets no
	// number rather than a wrong one: `_start` adjusts the stack for a call and
	// puts it back, and no single value is right for both halves.
	//
	// Only for a function that has somewhere to apply it. This costs a symbolic
	// evaluation of the whole function — around a third of what decompiling it
	// costs, measured on glibc — and a function whose locals all live in
	// registers has no use for the answer.
	private static Integer spDepth(Function f, HighFunction high) {
		if (!hasStackStorage(high)) {
			return null;
		}
		Map<Integer, Integer> counts = new HashMap<>();
		int total = 0;
		try {
			CallDepthChangeInfo depths = new CallDepthChangeInfo(f, TaskMonitor.DUMMY);
			InstructionIterator it =
				f.getProgram().getListing().getInstructions(f.getBody(), true);
			while (it.hasNext()) {
				Instruction ins = it.next();
				int d = depths.getSPDepth(ins.getAddress());
				if (d == Function.UNKNOWN_STACK_DEPTH_CHANGE
					|| d == Function.INVALID_STACK_DEPTH_CHANGE) {
					continue;
				}
				counts.merge(d, 1, Integer::sum);
				total++;
			}
		} catch (Exception e) {
			// An analysis that fell over says nothing about the frame, which is
			// the same position as not having run it.
			return null;
		}
		if (total == 0) {
			return null;
		}
		Map.Entry<Integer, Integer> best = null;
		for (Map.Entry<Integer, Integer> e : counts.entrySet()) {
			if (best == null || e.getValue() > best.getValue()) {
				best = e;
			}
		}
		// A majority, not merely a plurality. Two depths at 40% each mean the
		// stack pointer moves through the body, and either answer is wrong for
		// most of it.
		if (best.getValue() * 2 <= total) {
			return null;
		}
		return best.getKey();
	}

	// hasStackStorage says whether any of the decompiler's symbols live on the
	// stack, which is the only reason to work out where the stack is.
	private static boolean hasStackStorage(HighFunction high) {
		if (high == null) {
			return false;
		}
		Iterator<HighSymbol> syms = high.getLocalSymbolMap().getSymbols();
		while (syms.hasNext()) {
			VariableStorage store = syms.next().getStorage();
			if (store != null && store.isStackStorage()) {
				return true;
			}
		}
		return false;
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
			// The id addresses this symbol for an edit, and it is a string
			// rather than a number on purpose: a decompiler-only symbol's id is
			// around 4.6e18, which does not survive a round trip through a
			// JavaScript number. A consumer never does arithmetic on it.
			b.append(",\"id\":").append(str(Long.toString(sym.getId())));
			// Empty when the decompiler invented this symbol for this
			// decompilation and there is no database entry behind it — which
			// is a different thing from a name nobody has touched, and the two
			// must not be shown alike. Finding 38.
			b.append(",\"source\":").append(str(source(sym.getSymbol())));
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
		Program prog = high.getFunction().getProgram();
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
			// And only one that exists. An undefined symbol resolved from a
			// shared library — __stack_chk_guard in a dynamically linked
			// binary — is parked by Ghidra in a synthetic EXTERNAL block past
			// the end of the image. Its address is a fabrication, and biasing
			// it produces a plausible pointer into nothing: measured on an
			// AArch64 busybox, 0x1c9638 against LOAD segments ending at
			// 0xc8938, which gdb answers with "Cannot access memory".
			//
			// A wrong address that reads like a value is the failure this
			// whole schema is arranged to avoid, so these are dropped rather
			// than exported and left for the consumer to notice.
			MemoryBlock block = prog.getMemory().getBlock(store.getMinAddress());
			if (block == null || block.isExternalBlock() || !block.isLoaded()) {
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

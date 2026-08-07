// Export decompiled C, with an address map, as one JSON document.
//
// The batch counterpart to DecompServer.java: same schema, emitted by the same
// DecompJson helper, but written to a file in one pass rather than served on
// demand. It is what "decompile all" runs, and what you use from a shell to
// look at an image without starting gdb-wui at all.
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
// The schema, and the reasoning behind the two decisions that matter — text
// and line numbers from one PrettyPrinter pass, and per-line address *sets*
// rather than ranges — are in docs/decompilation.md and DecompJson.java.
//
//@category gdb-wui

import java.io.BufferedWriter;
import java.io.FileWriter;
import java.io.PrintWriter;
import java.util.ArrayList;
import java.util.List;
import java.util.regex.Pattern;

import ghidra.app.decompiler.DecompInterface;
import ghidra.app.decompiler.DecompileOptions;
import ghidra.app.decompiler.DecompileResults;
import ghidra.app.script.GhidraScript;
import ghidra.program.model.listing.Function;

public class ExportDecomp extends GhidraScript {

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
			out.println("  \"schema\": " + DecompJson.SCHEMA + ",");
			out.println("  \"generator\": {\"tool\": \"ghidra\", \"version\": " +
				DecompJson.str(ghidra.framework.Application.getApplicationVersion()) +
				", \"script\": \"ExportDecomp\"},");
			out.println("  \"program\": " + DecompJson.program(currentProgram) + ",");
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
				out.print("    " + DecompJson.function(f, res));
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
}

// LeanAnalysis turns off the analyzers that cost the most on a large image and
// leaves the ones that find functions.
//
// Run as a -preScript, so it sets the options before the analysis it is
// trimming. For an image with no symbol table: the whole-program analysis is
// the only thing that can find the functions, and the whole-program analysis is
// what does not fit. Measured on a stripped 12 MB MIPS64 kernel, 6.9 MB of it
// code — 89 seconds and 1.28 GB against a 2 GB heap, where the full set
// exhausted the heap and failed the import.
//
// The names are matched against the options the program actually has rather
// than hard-coded, because most are per-processor: the constant propagation
// that ran out of memory is "MIPS Constant Reference Analyzer" here and
// "ARM Constant Reference Analyzer" elsewhere. An analyzer this does not
// recognise is left alone.
//
// What stays on is everything that discovers or names: Disassemble Entry
// Points, Function Start Search, Reference, Subroutine References, the
// non-returning-function passes, Decompiler Switch Analysis (which recovers
// jump tables, and so the rest of a function), Function ID and Demangler GNU.
// The last two are the only analyzers that produce a real name rather than
// FUN_ and an address, so they are worth their cost even here.

import ghidra.app.script.GhidraScript;
import java.util.Map;

public class LeanAnalysis extends GhidraScript {

	// Off by exact name.
	private static final String[] OFF = {
		"Stack",                  // symbolic evaluation of every frame; ran out of heap
		"ASCII Strings",          // threw during string analysis on the kernel
		"Data Reference",
		"Create Address Tables",
		"Embedded Media",
		"Apply Data Archives",
		"Decompiler Parameter ID",
	};

	// Off by what the name contains, for the per-processor ones.
	private static final String[] OFF_CONTAINING = {
		"Constant Reference Analyzer",
	};

	@Override
	public void run() throws Exception {
		if (currentProgram == null) {
			println("LeanAnalysis: no program");
			return;
		}
		int n = 0;
		for (Map.Entry<String, String> e :
			getCurrentAnalysisOptionsAndValues(currentProgram).entrySet()) {
			String name = e.getKey();
			// Sub-options are addressed as "Analyzer.Option" and are not
			// analyzers; setting one to false says something else entirely.
			if (name.contains(".")) {
				continue;
			}
			if (!matches(name)) {
				continue;
			}
			setAnalysisOption(currentProgram, name, "false");
			n++;
		}
		println("LeanAnalysis: disabled " + n + " analyzers");
	}

	private static boolean matches(String name) {
		for (String off : OFF) {
			if (off.equals(name)) {
				return true;
			}
		}
		for (String part : OFF_CONTAINING) {
			if (name.contains(part)) {
				return true;
			}
		}
		return false;
	}
}

// ImportSymbols names the functions in an image that does not name them itself.
//
// The list is one symbol per line, `address [type] name`, which is what both
// nm and /proc/kallsyms produce. A stripped Linux kernel is the case this is
// for: CONFIG_KALLSYMS puts the whole symbol table inside the image as ordinary
// data, for oops traces, and stripping the ELF does not touch it. So a kernel
// with no symbol table can still tell you the names of all 22,563 of its
// functions, and cross-checking them against what Ghidra found on its own is
// how the two were confirmed to agree.
//
// A function is created with a one-byte body, which is exactly what Ghidra's
// ELF loader leaves for a symbol it has not analysed. That is deliberate: the
// sidecar already disassembles a function of that shape when it is first
// opened, so an injected symbol and an ELF one take the same path afterwards.
//
// Run as a -postScript on the import, so the work is saved into the project and
// paid for once.

import ghidra.app.script.GhidraScript;
import ghidra.program.model.address.Address;
import ghidra.program.model.address.AddressSet;
import ghidra.program.model.listing.Function;
import ghidra.program.model.symbol.SourceType;
import java.io.BufferedReader;
import java.io.FileReader;

public class ImportSymbols extends GhidraScript {

	@Override
	public void run() throws Exception {
		String[] args = getScriptArgs();
		if (args.length < 1) {
			println("ImportSymbols: usage: ImportSymbols.java <symbol-file>");
			return;
		}
		if (currentProgram == null) {
			println("ImportSymbols: no program");
			return;
		}
		long started = System.currentTimeMillis();
		int made = 0, named = 0, kept = 0, outside = 0, unreadable = 0;
		int tx = currentProgram.startTransaction("gdb-wui: import symbols");
		try (BufferedReader r = new BufferedReader(new FileReader(args[0]))) {
			String line;
			while ((line = r.readLine()) != null) {
				String[] f = line.trim().split("\\s+");
				if (f.length < 2 || f[0].startsWith("#")) {
					continue;
				}
				// `addr name` or `addr type name`. A one-character middle field
				// is nm's type letter; t, T, w and W are the code ones, and the
				// rest of the table is data this has no business creating
				// functions for.
				String name = f[f.length - 1];
				if (f.length >= 3) {
					String type = f[f.length - 2];
					if (type.length() == 1 && "tTwW".indexOf(type.charAt(0)) < 0) {
						continue;
					}
				}
				Address a = address(f[0]);
				if (a == null) {
					unreadable++;
					continue;
				}
				if (currentProgram.getMemory().getBlock(a) == null) {
					// A symbol for memory this image does not contain: percpu
					// and module addresses in a kernel's table, mostly.
					outside++;
					continue;
				}
				Function at = currentProgram.getFunctionManager().getFunctionAt(a);
				if (at != null) {
					// Something already claimed this address. A name the
					// analysis invented is worth replacing with a real one; a
					// name that came from the image is not, because it came
					// from the same place this did or from somewhere better.
					if (at.getSymbol() != null
						&& at.getSymbol().getSource() == SourceType.DEFAULT) {
						at.setName(name, SourceType.IMPORTED);
						named++;
					}
					else {
						kept++;
					}
					continue;
				}
				try {
					if (currentProgram.getFunctionManager().createFunction(
						name, a, new AddressSet(a, a), SourceType.IMPORTED) != null) {
						made++;
					}
				}
				catch (Exception e) {
					// Overlapping an existing body, or a name the program
					// already holds elsewhere. One symbol, not the run.
					kept++;
				}
			}
		}
		finally {
			currentProgram.endTransaction(tx, true);
		}
		println("ImportSymbols: created " + made + ", renamed " + named + ", left " + kept
			+ ", outside memory " + outside + ", unreadable " + unreadable
			+ ", in " + (System.currentTimeMillis() - started) + "ms");
	}

	private Address address(String text) {
		try {
			// Bare hex, which is what nm and kallsyms write. getAddress takes
			// it either way, but only with the prefix is it unambiguous.
			String hex = text.startsWith("0x") || text.startsWith("0X")
				? text.substring(2) : text;
			return currentProgram.getAddressFactory().getAddress("0x" + hex);
		}
		catch (Exception ignored) {
			return null;
		}
	}
}

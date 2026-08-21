// SeedEntry creates a function where a raw image was loaded.
//
// A raw image has no entry point and no symbols, so nothing tells Ghidra where
// execution begins. The analyzers find functions by pattern and miss the first
// ones: on a 12 MB ARM kernel Image based at 0xc0108000 the lowest they found
// was 0xc02027a8, a megabyte in, so the one address that was known for certain
// — the base, which for a kernel Image is where the bootloader jumps — had no
// function and would not decompile.
//
// Only where nothing is already, and only if the bytes disassemble. A raw image
// that starts with a header rather than code gets nothing, which is right: a
// function invented over data would decompile into fiction.
//
// Run as a -postScript on the import, after any symbols, so that a real name
// for the address wins over this.

import ghidra.app.cmd.disassemble.DisassembleCommand;
import ghidra.app.cmd.function.CreateFunctionCmd;
import ghidra.app.script.GhidraScript;
import ghidra.program.model.address.Address;
import ghidra.program.model.listing.Function;
import ghidra.program.model.symbol.SourceType;

public class SeedEntry extends GhidraScript {

	@Override
	public void run() throws Exception {
		String[] args = getScriptArgs();
		if (args.length < 1) {
			println("SeedEntry: usage: SeedEntry.java <address>");
			return;
		}
		if (currentProgram == null) {
			println("SeedEntry: no program");
			return;
		}
		Address a;
		try {
			a = currentProgram.getAddressFactory().getAddress(args[0]);
		}
		catch (Exception e) {
			println("SeedEntry: " + args[0] + " is not an address");
			return;
		}
		if (a == null || currentProgram.getMemory().getBlock(a) == null) {
			println("SeedEntry: " + args[0] + " is outside the image");
			return;
		}
		Function at = currentProgram.getFunctionManager().getFunctionAt(a);
		if (at != null) {
			println("SeedEntry: " + a + " is already " + at.getName());
			return;
		}
		int tx = currentProgram.startTransaction("gdb-wui: seed entry");
		try {
			if (currentProgram.getListing().getInstructionAt(a) == null) {
				new DisassembleCommand(a, null, true).applyTo(currentProgram, monitor);
			}
			if (currentProgram.getListing().getInstructionAt(a) == null) {
				println("SeedEntry: " + a + " does not disassemble");
				return;
			}
			// A null body makes CreateFunctionCmd follow the flow and work out
			// the real one, which is what every other function in the program
			// has and what the decompiler's frame rule needs.
			CreateFunctionCmd cmd = new CreateFunctionCmd(null, a, null, SourceType.IMPORTED);
			if (!cmd.applyTo(currentProgram, monitor)) {
				println("SeedEntry: " + a + ": " + cmd.getStatusMsg());
				return;
			}
			println("SeedEntry: created a function at " + a);
		}
		finally {
			currentProgram.endTransaction(tx, true);
		}
	}
}

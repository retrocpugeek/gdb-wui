package debugger

// Unexported machinery the tests beside this package need.
//
// varExpr is the frame rule, and the test that checks it against a live
// inferior on another architecture has to build its input by hand — the
// alternative is a Ghidra installation, which CI does not have and which is
// not what that test is about.
var VarExpr = varExpr

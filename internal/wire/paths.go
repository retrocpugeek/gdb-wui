package wire

// Teaching gdb where source lives.

// Request types implemented in M8.
const (
	TypePathSubstitute = "path.substitute"
	TypePathAddDir     = "path.addDir"
	TypePathList       = "path.list"
)

// PathSubstituteRequest maps a prefix gdb reports onto a local one.
//
// A prefix, not a file: teaching gdb the prefix once fixes every later frame in
// that tree, plus `list`, `info line` and whatever the user types at the
// console. Rewriting paths one file at a time in the UI is a losing game,
// because gdb keeps reporting the originals.
type PathSubstituteRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
	// GDBPath and Path are the alternative: give the pair that should match and
	// let the server work out the prefixes. This is what the "locate this file"
	// affordance sends, because it knows the two files, not the prefixes.
	GDBPath string `json:"gdbPath,omitempty"`
	Path    string `json:"path,omitempty"`
}

// PathAddDirRequest adds a directory to gdb's source search path.
type PathAddDirRequest struct {
	// Dir is root-relative.
	Dir string `json:"dir"`
}

// PathList is the reply to the path group: what gdb has been told so far.
type PathList struct {
	Substitutions []Substitution `json:"substitutions"`
	Directories   []string       `json:"directories,omitempty"`
	// Indexed and IndexTruncated describe the basename index, so a failure to
	// find a file can be explained rather than looking arbitrary.
	Indexed        int  `json:"indexed"`
	IndexTruncated bool `json:"indexTruncated,omitempty"`
}

// Substitution is one prefix mapping in effect.
type Substitution struct {
	From string `json:"from"`
	To   string `json:"to"`
}

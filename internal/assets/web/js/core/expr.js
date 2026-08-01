// Turning what the pointer is over into something gdb can evaluate.
//
// Pure string work, deliberately kept out of the panels: this is the only part
// of the hover feature with real logic, and here it can be driven by a test
// through node rather than only by a mouse.
//
// The governing rule is that a hover must never have side effects. gdb will
// happily *call a function* to evaluate `f(x)`, which would be a spectacular
// thing for a mouse to do by accident, so the grammar recognised here is only
// the postfix chain — names, member access, subscripts. A `(` anywhere in the
// chain stops it dead.

const IDENT = /[A-Za-z0-9_$]/;
const NAME_START = /[A-Za-z_$]/;

// Hovering a keyword is the single commonest thing a pointer drifting over
// source does, and every one of them costs a round trip and returns an error.
// Types are in here too: `int` in `int count = 0` is not a variable, and
// asking gdb about it produces a confident-looking "No symbol" that means
// nothing.
const KEYWORDS = new Set([
  "auto", "bool", "break", "case", "catch", "char", "class", "const",
  "constexpr", "continue", "default", "delete", "do", "double", "else",
  "enum", "explicit", "extern", "false", "float", "for", "friend", "goto",
  "if", "inline", "int", "long", "namespace", "new", "nullptr", "operator",
  "private", "protected", "public", "register", "restrict", "return",
  "short", "signed", "sizeof", "static", "struct", "switch", "template",
  "throw", "try", "typedef", "typename", "union", "unsigned", "using",
  "virtual", "void", "volatile", "while",
]);

// expressionAt finds the expression surrounding a character offset in a line.
//
// Returns null when there is nothing worth asking about, and otherwise the
// expression together with the span it occupies, so the caller can anchor a
// tooltip to the whole chain rather than to the one word under the pointer.
export function expressionAt(text, offset) {
  if (typeof text !== "string" || offset < 0 || offset >= text.length) return null;
  // The caret offset is an insertion point, so the right-hand half of the last
  // character of a word reports the position after it. Stepping back one keeps
  // the far edge of a word hoverable, at the cost of half a character of slop.
  if (!IDENT.test(text[offset]) && offset > 0 && IDENT.test(text[offset - 1])) {
    offset--;
  }
  if (!IDENT.test(text[offset])) return null;

  let start = offset;
  while (start > 0 && IDENT.test(text[start - 1])) start--;
  let end = offset + 1;
  while (end < text.length && IDENT.test(text[end])) end++;

  const word = text.slice(start, end);
  // A word starting with a digit is part of a number: `0x1f`, `1e9`, `42`.
  // Rejecting it here is also what stops `1.5` being read as member access.
  if (!NAME_START.test(word[0])) return null;
  if (KEYWORDS.has(word)) return null;

  const head = chainStart(text, start);
  return { expr: text.slice(head, end), start: head, end };
}

// chainStart walks left from the start of a word through the postfix chain
// that qualifies it, so hovering `name` in `cfg.items[2].name` asks about the
// whole path rather than about a field name that means nothing on its own.
//
// head only moves on a complete step, so a chain that runs into something
// unrecognised yields the longest good suffix instead of a broken expression.
function chainStart(text, start) {
  let head = start;
  for (;;) {
    let i = head;
    if (i >= 2 && text[i - 2] === "-" && text[i - 1] === ">") i -= 2;
    else if (i >= 1 && text[i - 1] === ".") i -= 1;
    else return head;

    // Subscripts bind to the name on their left: `items[2].name`.
    while (i > 0 && text[i - 1] === "]") {
      const open = matchBracket(text, i - 1);
      if (open < 0) return head;
      i = open;
    }
    if (i === 0 || !IDENT.test(text[i - 1])) return head;
    while (i > 0 && IDENT.test(text[i - 1])) i--;
    if (!NAME_START.test(text[i])) return head;
    head = i;
  }
}

// matchBracket finds the `[` opening a `]`, refusing any subscript containing a
// call. `a[f(1)].b` stops the chain rather than handing gdb an expression that
// would run f.
function matchBracket(text, close) {
  let depth = 0;
  for (let i = close; i >= 0; i--) {
    const c = text[i];
    if (c === "(" || c === ")") return -1;
    if (c === "]") depth++;
    else if (c === "[") {
      depth--;
      if (depth === 0) return i;
    }
  }
  return -1;
}

// registerExpr converts a disassembly operand into the way gdb spells the same
// register. AT&T writes `%rax`; gdb's expression language wants `$rax`.
export function registerExpr(token) {
  const m = /^%([A-Za-z][A-Za-z0-9_]*)$/.exec(token.trim());
  return m ? "$" + m[1] : "";
}

// bareRegisterExpr is the same thing for the architectures that do not
// decorate register names — ARM's `r0`, `sp`, `x19`, MIPS's `t0`. The
// disassembly tokenizer cannot tell those from any other word, so this is
// deliberately narrow, and a wrong guess is harmless: gdb answers `void` for a
// convenience variable that was never set, and the caller shows nothing.
export function bareRegisterExpr(token) {
  return /^[a-z][a-z0-9]{0,4}$/.test(token.trim()) ? "$" + token.trim() : "";
}

// symbolExpr pulls the name out of the `<add+4>` annotation gdb attaches to a
// branch or a memory reference. The offset is dropped: the useful question is
// what `add` is, not what `add+4` is.
//
// A `@plt` suffix goes too. `snprintf@plt` is the linker's name for the
// trampoline, and it is not an expression — `@` is gdb's artificial-array
// operator, so asking about it verbatim is an error. The name in front of it
// is what the user is pointing at.
export function symbolExpr(token) {
  const m = /^<([A-Za-z_$][A-Za-z0-9_$.]*)(?:@[A-Za-z0-9_.]+)?(?:[+-]\d+)?>$/
    .exec(token.trim());
  return m ? m[1] : "";
}

// alternateBase renders an integer the other way round, because a register
// printed as 140737488347136 is unreadable and the same number as
// 0x7ffffffde000 is a stack address you recognise.
//
// BigInt rather than Number: a 64-bit register does not survive float64, and
// silently rounding the top of an address is worse than not showing it.
export function alternateBase(value) {
  const v = String(value ?? "").trim();
  if (!/^-?\d+$/.test(v)) return "";
  const n = BigInt(v);
  // Below ten there is nothing to convert: `0x7` beside `7` is noise.
  if (n > -10n && n < 10n) return "";
  return n < 0n ? "-0x" + (-n).toString(16) : "0x" + n.toString(16);
}

// Turning what the pointer is over into something gdb can evaluate.
//
// This is pure string work, kept out of the panels so that it can be driven by
// a test through node rather than only by a mouse. It is the only part of the
// hover feature with logic in it.
//
// The rule it enforces is that a hover must have no side effects. gdb evaluates
// `f(x)` by calling `f` in the program being debugged, so the grammar
// recognised here is only the postfix chain: names, member access and
// subscripts. A `(` anywhere in the chain ends it.

const IDENT = /[A-Za-z0-9_$]/;
const NAME_START = /[A-Za-z_$]/;

// Hovering a keyword is the most common thing a pointer drifting over source
// does, and each one would cost a round trip and return an error. Type names
// are included: `int` in `int count = 0` is not a variable, and asking gdb
// about it produces a "No symbol" error that means nothing here.
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
// Returns null when there is nothing worth asking about. Otherwise it returns
// the expression and the span it occupies, so that the caller can anchor a
// tooltip to the whole chain rather than to the single word under the
// pointer.
export function expressionAt(text, offset) {
  if (typeof text !== "string" || offset < 0 || offset >= text.length) return null;
  // The caret offset is an insertion point, so the right-hand half of a word's
  // last character reports the position after it. Stepping back one character
  // keeps the far edge of a word hoverable, at the cost of half a character of
  // imprecision.
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

// chainStart walks left from the start of a word through the postfix chain that
// qualifies it, so that hovering `name` in `cfg.items[2].name` asks about the
// whole path rather than about a field name on its own.
//
// head only moves on a complete step, so a chain that runs into something
// unrecognised yields the longest valid suffix rather than a broken
// expression.
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

// matchBracket finds the `[` that opens a `]`, and refuses any subscript
// containing a call. `a[f(1)].b` ends the chain rather than producing an
// expression that would run f.
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

// bareRegisterExpr does the same for architectures that do not decorate
// register names, such as ARM's `r0`, `sp` and `x19`, or MIPS's `t0`. The
// disassembly tokenizer cannot distinguish those from any other word, so the
// pattern is narrow. A wrong guess is harmless: gdb answers `void` for a
// convenience variable that was never set, and the caller then shows
// nothing.
export function bareRegisterExpr(token) {
  return /^[a-z][a-z0-9]{0,4}$/.test(token.trim()) ? "$" + token.trim() : "";
}

// symbolExpr pulls the name out of the `<add+4>` annotation gdb attaches to a
// branch or a memory reference. The offset is dropped, because the useful
// question is what `add` is rather than what `add+4` is.
//
// A `@plt` suffix is dropped too. `snprintf@plt` is the linker's name for the
// trampoline and is not a valid expression, since `@` is gdb's
// artificial-array operator. The name in front of it is what the user is
// pointing at.
export function symbolExpr(token) {
  const m = /^<([A-Za-z_$][A-Za-z0-9_$.]*)(?:@[A-Za-z0-9_.]+)?(?:[+-]\d+)?>$/
    .exec(token.trim());
  return m ? m[1] : "";
}

// alternateBase renders an integer in the other base, because a register
// printed as 140737488347136 is hard to read while the same number as
// 0x7ffffffde000 is recognisably a stack address.
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

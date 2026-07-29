package shield

import "strings"

// stripCLikeComments removes // and /* */ comments from C-family source
// (c, cpp, java, go) using a byte-level scan that skips over string, char,
// and raw-string literals — Go's backticks and C++11's R"delim(...)delim" —
// so comment-like text inside them survives untouched. A comment is replaced
// by a space, as in C's translation phase 3, so removing one never merges
// the tokens on either side of it.
//
// Preprocessor directives are scanned like any other line rather than being
// copied through verbatim. Exempting them would leave the shield's one job
// undone on the most common language family here: `#define N 100 // payload`
// would survive intact, and a `/*` opened on a directive line would leave
// the scanner with no comment state, so the comment's body on the following
// lines would be emitted as ordinary code. It is also what C itself does —
// comments are replaced by a space in translation phase 3, before directives
// are executed in phase 4 — so `#define PATH http://x` losing its trailing
// `//x` matches the compiler, not just the shield's preference. The
// directive text itself is untouched: `#` has no special meaning to this
// scanner, and an unbalanced quote in `#error don't` cannot swallow the rest
// of the file because skipEscaped stops at the newline.
//
// The language is passed in rather than a single flag because the family's
// members disagree about what happens *before* the lexer runs, and every one
// of those disagreements is a shield boundary (see clikeSyntax).
//
// Java's """...""" text blocks are one of them: in C and C++ a bare """ is the
// adjacency of an empty string and an opening quote (`"" "abc"`), not a block
// opener. Without the distinction a Java text block was scanned as an empty
// string followed by a literal terminated at the newline, so the block's body
// was treated as code — meaning // and /* */ *inside* the text block were
// deleted from CodeAfter, the very text handed to the repair model and
// submitted to the judge.
func stripCLikeComments(code string, lang Language) (string, []string) {
	syn := syntaxFor(lang)
	textBlocks := syn.textBlocks

	var out strings.Builder
	var comments []string
	n := len(code)
	i := 0
	// A preprocessor directive is the one place where the newlines a comment
	// swallowed cannot be re-emitted as newlines: phase 3 replaces the whole
	// comment — spliced or multi-line — with a single space, so the directive
	// continues past it, while a bare newline in the output ends the directive
	// early and turns the rest of its replacement list into a new logical
	// line. See newlinePadding.
	dir := directiveTracker{syn: syn, atLineStart: true}

	for i < n {
		c := code[i]
		// Comment openers are matched on the *effective* characters — what the
		// compiler sees after line splicing (C) or unicode-escape translation
		// (Java) — while everything else keeps reading raw bytes. See
		// clikeSyntax: an opener the compiler recognizes and the shield does
		// not is a comment whose body reaches the model as ordinary code.
		ch, w := syn.effChar(code, i)
		ch2, w2 := syn.effChar(code, i+w)
		switch {
		case ch == '/' && w2 > 0 && ch2 == '/':
			start := i
			end := skipLineComment(code, i, syn)
			comments = append(comments, code[start:end])
			// A // comment spliced across lines by trailing backslashes
			// consumed those newlines; re-emit them so line numbers (and the
			// line-based diff) still line up.
			out.WriteString(dir.newlinePadding(code[start:end]))
			i = end

		case ch == '/' && w2 > 0 && ch2 == '*':
			start := i
			end := blockCommentEnd(code, i+w+w2, syn)
			comments = append(comments, code[start:end])
			// A comment becomes one space, as in C's translation phase 3 —
			// deleting it outright merges the tokens on either side, so
			// `int/**/main()` would be handed to the model (and the judge) as
			// the uncompilable `intmain()`.
			out.WriteString(" ")
			out.WriteString(dir.newlinePadding(code[start:end]))
			i = end

		// Literal delimiters are matched on effective characters for exactly
		// the reason the comment openers are: Java translates \uXXXX before
		// lexing, so a string opened with " is a string to javac. A
		// scanner that only sees the raw bytes never enters it, and any //
		// or /* */ *inside* that string is deleted from the code handed to
		// the model and submitted to the judge.
		case ch == '"':
			var end int
			switch {
			case textBlocks && isEffTripleQuote(code, i, syn):
				end = textBlockEnd(code, i, syn)
			default:
				if raw, ok := rawStringEnd(code, i); ok {
					end = raw
				} else {
					end = skipEscaped(code, i, '"', syn)
				}
			}
			out.WriteString(code[i:end])
			dir.code(code[i:end])
			i = end

		case ch == '\'' && !isDigitSeparator(code, i):
			end := skipEscaped(code, i, '\'', syn)
			out.WriteString(code[i:end])
			dir.code(code[i:end])
			i = end

		case c == '`':
			end := skipRaw(code, i, '`')
			out.WriteString(code[i:end])
			dir.code(code[i:end])
			i = end

		default:
			out.WriteByte(c)
			dir.code(code[i : i+1])
			i++
		}
	}

	return out.String(), comments
}

// directiveTracker follows whether the scanner is inside a preprocessor
// directive, so the newlines a removed comment carried can be re-emitted in a
// form that does not end that directive.
//
// The whole comment — however many newlines it spans, whether they are raw or
// spliced — becomes a single space in translation phase 2/3, before phase 4
// ever sees the directive, so in
//
//	#define X /\
//	* hidden */ 1
//	#define Y 1 /* a
//	b */ + 2
//
// the compiler reads `#define X 1` and `#define Y 1 + 2`. Padding the removed
// comment with bare newlines instead — which is what keeps line numbers and
// the line-based diff aligned everywhere else — cuts both directives short and
// leaves their tails as new logical lines, i.e. hands the model and the judge a
// program the student did not write. Inside a directive the padding is a line
// splice per newline, which the compiler removes again and which still costs
// one line each.
type directiveTracker struct {
	syn         clikeSyntax
	inDirective bool
	atLineStart bool
	backslash   bool
	// spliceAtLineStart records whether the logical line was still empty when
	// the pending trailing backslash was seen. A splice is deleted in phase 2,
	// so a line consisting of nothing but `\` + newline leaves the *next*
	// line's `#` at the start of the logical line, i.e. a real directive.
	spliceAtLineStart bool
}

// code feeds the tracker a span of source that is being emitted as code
// (literals included, byte by byte for everything else). Comment spans are
// deliberately not fed: their newlines are exactly the ones that do not exist
// by the time directives are processed.
func (d *directiveTracker) code(text string) {
	if !d.syn.splices {
		return
	}
	for k := 0; k < len(text); k++ {
		switch b := text[k]; {
		case b == '\r':
			// Part of a CRLF, and not enough to break a trailing backslash.
		case b == '\n':
			if d.backslash {
				// Spliced away in phase 2: the logical line continues, and it
				// still starts here if nothing but the backslash preceded it.
				d.atLineStart = d.spliceAtLineStart
			} else {
				d.inDirective = false
				d.atLineStart = true
			}
			d.backslash = false
		case b == '#' && d.atLineStart:
			d.inDirective = true
			d.atLineStart = false
			d.backslash = false
		case b == ' ' || b == '\t':
			d.backslash = false
		case b == '\\':
			d.spliceAtLineStart = d.atLineStart
			d.atLineStart = false
			d.backslash = true
		default:
			d.atLineStart = false
			d.backslash = false
		}
	}
}

// newlinePadding returns the replacement for the newlines a removed comment
// spanned: bare newlines normally, line splices inside a directive.
func (d *directiveTracker) newlinePadding(comment string) string {
	nl := strings.Count(comment, "\n")
	if nl == 0 {
		return ""
	}
	if d.inDirective {
		return strings.Repeat("\\\n", nl)
	}
	return strings.Repeat("\n", nl)
}

// clikeSyntax is the part of a language's *pre-lexical* translation the
// comment scanner has to see through. Both members of it are shield
// boundaries, not pedantry: the compiler applies them before comments are
// recognized, so an opener the compiler builds this way and the scanner does
// not is a comment whose body is handed to the model as ordinary code — the
// exact bypass the shield exists to close.
//
//   - splices: C and C++ delete a backslash-newline pair in translation phase
//     2, before phase 3 recognizes comments, so `/\` + newline + `/ payload`
//     *is* a // comment and `*\` + newline + `/` closes a block one.
//   - unicodeEsc: Java translates \uXXXX in phase 1, before lexing anything
//     at all, so `// payload` is a line comment to javac.
//
// Go has neither, which is why the flags are per-language rather than always
// on: splicing a Java or Go line comment across a trailing backslash would
// delete the next line of real code from the submission.
type clikeSyntax struct {
	splices    bool
	unicodeEsc bool
	textBlocks bool
}

func syntaxFor(lang Language) clikeSyntax {
	switch lang {
	case LangC, LangCPP:
		return clikeSyntax{splices: true}
	case LangJava:
		return clikeSyntax{unicodeEsc: true, textBlocks: true}
	default:
		return clikeSyntax{}
	}
}

// effChar returns the character the compiler sees at position i and how many
// raw bytes it spans, after skipping any line splices and decoding a unicode
// escape. A zero width means the string ends there. Only ASCII is decoded:
// the scanner's decisions are all about ASCII punctuation, and a wider
// codepoint is passed through as its raw bytes.
func (syn clikeSyntax) effChar(code string, i int) (byte, int) {
	n := len(code)
	if i >= n {
		return 0, 0
	}
	j := i
	if syn.splices {
		for j < n && code[j] == '\\' && lineBreakAt(code, j+1) {
			j++
			if code[j] == '\r' {
				j++
			}
			j++ // the newline
		}
		if j >= n {
			return 0, 0
		}
	}
	if syn.unicodeEsc {
		if c, w, ok := javaUnicodeEscape(code, j); ok {
			return c, j - i + w
		}
	}
	return code[j], j - i + 1
}

// javaUnicodeEscape decodes the \uXXXX escape starting at i, if there is one.
// The JLS allows any number of u's after the backslash, and only treats the
// escape as one when the backslash is preceded by an even number of
// backslashes — `\\u002f` is a literal backslash followed by "u002f", not a
// slash, and reading it as one would delete real code.
func javaUnicodeEscape(code string, i int) (byte, int, bool) {
	if i >= len(code) || code[i] != '\\' {
		return 0, 0, false
	}
	preceding := 0
	for k := i - 1; k >= 0 && code[k] == '\\'; k-- {
		preceding++
	}
	if preceding%2 != 0 {
		return 0, 0, false
	}
	j := i + 1
	for j < len(code) && code[j] == 'u' {
		j++
	}
	if j == i+1 || j+4 > len(code) {
		return 0, 0, false
	}
	var v int
	for k := j; k < j+4; k++ {
		d := hexValue(code[k])
		if d < 0 {
			return 0, 0, false
		}
		v = v*16 + d
	}
	if v > 0x7f {
		return 0, 0, false
	}
	return byte(v), j + 4 - i, true
}

func hexValue(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// blockCommentEnd returns the index just past the closing */ of the block
// comment whose body starts at i, or len(code) if it is never closed. The
// terminator is matched on effective characters for the same reason the
// opener is: `*\` + newline + `/` closes the comment in C, so a scanner that
// insists on the two bytes being adjacent runs past the end of the comment and
// emits the code after it as if it were still commented out.
func blockCommentEnd(code string, i int, syn clikeSyntax) int {
	n := len(code)
	for j := i; j < n; {
		ch, w := syn.effChar(code, j)
		if w == 0 {
			break
		}
		if ch == '*' {
			if ch2, w2 := syn.effChar(code, j+w); w2 > 0 && ch2 == '/' {
				return j + w + w2
			}
		}
		j += w
	}
	return n
}

// isDigitSeparator reports whether the apostrophe at position i is a C++14
// digit separator (1'000'000, 0xFF'FF) rather than the opening quote of a
// char literal.
//
// A separator only ever appears inside a numeric literal, so the test is
// whether the token the quote sits in *starts with a digit*. Testing only
// the immediately adjacent characters is not enough, and the difference is a
// shield bypass rather than a nicety: `8` is a hex digit, so `u8'a'` looked
// like a separator between `8` and `a`, the char literal was never entered,
// and its closing quote then opened a phantom literal that swallowed the
// rest of the line — hiding `u8'a'; // payload` from the shield entirely.
// Widening the rule the other way is just as bad: treating every identifier
// character before the quote as a prefix would reject the real separator in
// `0xFF'FF`, and an odd number of unrecognized separators on a line sends
// skipEscaped hunting for a closing quote that never comes.
//
// Walking to the token start handles both: `u8`, `L`, `u`, `U` start with a
// letter (char-literal prefixes), while `0xFF` and `1'000` start with a
// digit (numeric literals).
func isDigitSeparator(code string, i int) bool {
	if i == 0 || i+1 >= len(code) {
		return false
	}
	if !isHexDigit(code[i-1]) || !isHexDigit(code[i+1]) {
		return false
	}
	start := i
	for start > 0 && (isIdentChar(code[start-1]) || code[start-1] == '\'') {
		start--
	}
	return start < len(code) && code[start] >= '0' && code[start] <= '9'
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') ||
		(c >= 'a' && c <= 'f') ||
		(c >= 'A' && c <= 'F')
}

func isIdentChar(c byte) bool {
	return c == '_' ||
		(c >= '0' && c <= '9') ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z')
}

// rawStringEnd returns the index just past a C++11 raw string literal whose
// opening quote is at i, and whether one starts there at all.
//
// Raw strings process no escapes and are terminated only by the exact
// `)delim"` they opened with, so scanning one as an ordinary string is wrong
// in both directions: `R"(a"b)"` ends early at the inner quote and the
// trailing `"` reopens a literal that swallows the following `// comment`
// (a bypass), while a `//` sitting *inside* a raw string gets deleted as a
// comment, silently changing code that is then submitted to the judge.
//
// A miss returns false so the caller falls back to the ordinary string scan —
// `R` is a legal identifier, and `R"foo"` in Go is an identifier followed by
// a plain string, not a raw literal.
func rawStringEnd(code string, i int) (int, bool) {
	if i == 0 || code[i-1] != 'R' {
		return 0, false
	}
	// Only the encoding prefixes may precede the R; anything else means the
	// R belongs to an identifier that merely ends in R.
	switch {
	case i-1 == 0:
	case !isIdentChar(code[i-2]):
	case strings.HasSuffix(code[:i-1], "u8") && (i-3 == 0 || !isIdentChar(code[i-4])):
	case (code[i-2] == 'L' || code[i-2] == 'u' || code[i-2] == 'U') && (i-2 == 0 || !isIdentChar(code[i-3])):
	default:
		return 0, false
	}

	// d-char-sequence: at most 16 characters, none of them a space, a
	// parenthesis, a backslash or a control character.
	const maxDelim = 16
	j := i + 1
	for j < len(code) && j-(i+1) <= maxDelim && code[j] != '(' {
		if code[j] <= ' ' || code[j] == ')' || code[j] == '\\' || code[j] == '"' {
			return 0, false
		}
		j++
	}
	if j >= len(code) || code[j] != '(' {
		return 0, false
	}

	closing := ")" + code[i+1:j] + `"`
	rest := strings.Index(code[j+1:], closing)
	if rest < 0 {
		return len(code), true // unterminated: the rest of the file is literal
	}
	return j + 1 + rest + len(closing), true
}

// skipLineComment returns the index just past the // comment opening at i.
// C splices a line ending in a backslash with the next one in translation
// phase 2, before comments are recognized, so a comment whose line ends in
// an odd number of backslashes continues onto the following line — text the
// shield must strip rather than emit to the model as ordinary code. Java and
// Go do not splice, and there the same rule would delete the *next line of
// real code* from the submission, so it is gated on the language.
func skipLineComment(code string, i int, syn clikeSyntax) int {
	n := len(code)
	j := i
	if !syn.splices {
		if syn.unicodeEsc {
			// A unicode-escaped newline is a line terminator to javac
			// (phase 1 runs before the comment is recognized, and JLS 3.4
			// forms LineTerminator from the translated characters), so the
			// comment ends there and
			// what follows on the raw line is live code — emitting it as
			// comment would delete real code from the submission.
			for j < n {
				ch, w := syn.effChar(code, j)
				if w == 0 || ch == '\n' || ch == '\r' {
					return j
				}
				j += w
			}
			return j
		}
		for j < n && code[j] != '\n' {
			j++
		}
		return j
	}
	for {
		for j < n && code[j] != '\n' {
			j++
		}
		line := strings.TrimSuffix(code[i:j], "\r")
		trailing := 0
		for trailing < len(line) && line[len(line)-1-trailing] == '\\' {
			trailing++
		}
		if j >= n || trailing%2 == 0 {
			return j
		}
		j++ // splice: the next line is still comment
	}
}

// skipEscaped returns the index just past the literal opened by quote at
// position i, honoring backslash escapes. The scan stops at an unescaped
// newline: no C-family string or char literal spans a raw line break, so a
// literal left unterminated by a stray quote must not swallow the rest of
// the file — that would hide every comment after it from the shield.
//
// It reads effective characters, not raw bytes, for the same reason the
// comment scanner does: a Java literal can be opened *and* closed by a
// unicode escape (javac translates them in phase 1, so an escaped quote
// inside a literal really does end it), and a C literal can carry a line
// splice. Comparing raw bytes
// left the scanner outside a literal javac is inside, so // and /* */
// within it were stripped from the code the model reads and the judge runs.
func skipEscaped(code string, i int, quote byte, syn clikeSyntax) int {
	n := len(code)
	_, w := syn.effChar(code, i) // the opening quote may itself be an escape
	if w == 0 {
		return n
	}
	j := i + w
	for j < n {
		ch, w := syn.effChar(code, j)
		if w == 0 {
			return n
		}
		if ch == quote {
			return j + w
		}
		if ch == '\\' {
			_, w2 := syn.effChar(code, j+w)
			if w2 == 0 {
				return n
			}
			j += w + w2 // backslash escape, incl. a line continuation
			continue
		}
		if ch == '\n' {
			return j // unterminated literal, don't cross the line
		}
		j += w
	}
	return n
}

// isEffTripleQuote reports whether three effective quote characters — the
// opener of a Java text block — start at i.
func isEffTripleQuote(code string, i int, syn clikeSyntax) bool {
	j := i
	for k := 0; k < 3; k++ {
		ch, w := syn.effChar(code, j)
		if w == 0 || ch != '"' {
			return false
		}
		j += w
	}
	return true
}

// skipRaw returns the index just past a raw literal (e.g. a Go backtick
// string) opened at i, with no escape processing.
func skipRaw(code string, i int, quote byte) int {
	n := len(code)
	j := i + 1
	for j < n && code[j] != quote {
		j++
	}
	if j < n {
		j++
	}
	return j
}

// textBlockEnd returns the index just past the closing """ of the Java text
// block opening at i, or len(code) if it is unterminated. A backslash
// escapes the next character, so \""" does not close the block.
func textBlockEnd(code string, i int, syn clikeSyntax) int {
	n := len(code)
	j := i
	for k := 0; k < 3; k++ { // past the opening delimiter
		_, w := syn.effChar(code, j)
		if w == 0 {
			return n
		}
		j += w
	}
	for j < n {
		ch, w := syn.effChar(code, j)
		if w == 0 {
			return n
		}
		if ch == '\\' {
			_, w2 := syn.effChar(code, j+w)
			if w2 == 0 {
				return n
			}
			j += w + w2
			continue
		}
		if ch == '"' && isEffTripleQuote(code, j, syn) {
			e := j
			for k := 0; k < 3; k++ {
				_, w := syn.effChar(code, e)
				e += w
			}
			return e
		}
		j += w
	}
	return n
}

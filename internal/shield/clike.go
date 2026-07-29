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
func stripCLikeComments(code string) (string, []string) {
	var out strings.Builder
	var comments []string
	n := len(code)
	i := 0

	for i < n {
		c := code[i]
		switch {
		case c == '/' && i+1 < n && code[i+1] == '/':
			start := i
			end := skipLineComment(code, i)
			comments = append(comments, code[start:end])
			// A // comment spliced across lines by trailing backslashes
			// consumed those newlines; re-emit them so line numbers (and the
			// line-based diff) still line up.
			out.WriteString(strings.Repeat("\n", strings.Count(code[start:end], "\n")))
			i = end

		case c == '/' && i+1 < n && code[i+1] == '*':
			start := i
			i += 2
			for i < n && (i+1 >= n || code[i] != '*' || code[i+1] != '/') {
				i++
			}
			end := i + 2
			if end > n {
				end = n
			}
			comments = append(comments, code[start:end])
			// A comment becomes one space, as in C's translation phase 3 —
			// deleting it outright merges the tokens on either side, so
			// `int/**/main()` would be handed to the model (and the judge) as
			// the uncompilable `intmain()`.
			out.WriteString(" ")
			out.WriteString(strings.Repeat("\n", strings.Count(code[start:end], "\n")))
			i = end

		case c == '"':
			var end int
			if raw, ok := rawStringEnd(code, i); ok {
				end = raw
			} else {
				end = skipEscaped(code, i, '"')
			}
			out.WriteString(code[i:end])
			i = end

		case c == '\'' && !isDigitSeparator(code, i):
			end := skipEscaped(code, i, '\'')
			out.WriteString(code[i:end])
			i = end

		case c == '`':
			end := skipRaw(code, i, '`')
			out.WriteString(code[i:end])
			i = end

		default:
			out.WriteByte(c)
			i++
		}
	}

	return out.String(), comments
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
// shield must strip rather than emit to the model as ordinary code.
func skipLineComment(code string, i int) int {
	n := len(code)
	j := i
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
func skipEscaped(code string, i int, quote byte) int {
	n := len(code)
	j := i + 1
	for j < n && code[j] != quote {
		if code[j] == '\\' && j+1 < n {
			j += 2 // backslash escape, incl. a line continuation
			continue
		}
		if code[j] == '\n' {
			return j // unterminated literal, don't cross the line
		}
		j++
	}
	if j < n {
		j++
	}
	return j
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

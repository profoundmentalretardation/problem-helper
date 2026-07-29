package shield

import "strings"

// stripPythonComments removes # comments and docstring-position
// triple-quoted string literals from Python source. Single/double-quoted
// string literals are skipped over untouched, including any comment-like
// text inside them.
//
// "Docstring position" means the literal is the entire logical line —
// nothing but whitespace before its opening quotes, nothing but whitespace
// or a # comment after its closing quotes. That covers module/def/class
// docstrings and the free-floating triple-quoted block used to comment code
// out, which is the injection surface this exists for. Stripping *every*
// triple-quoted literal instead (the original behaviour) rewrote
// `msg = """a\nb"""` to `msg = `, a SyntaxError: that mangled text is what
// pipeline.go hands the repair model as UserCode and what it diffs for the
// hint, so the model diagnosed code the student never wrote and the "fix"
// went on to the judge. textwrap.dedent("""...""") and multi-line format
// strings hit the same path.
func stripPythonComments(code string) (string, []string) {
	stripped, comments, depth := stripPythonPass(code, false)
	if depth == 0 {
		return stripped, comments
	}
	// depth is only ever decremented and is clamped at zero, so the *safe*
	// direction (a stray closing bracket) is guarded but the unsafe one is
	// not: one unmatched opener — the single most common Python syntax error,
	// and trivially craftable on purpose — leaves depth > 0 for the rest of
	// the file, so every later triple-quoted literal is read as an expression
	// operand and preserved. A payload in one then reaches the model as
	// ordinary code with Removed.Comments empty: a bypass with no signal.
	//
	// A file whose brackets do not balance cannot compile, so the mangling
	// risk the depth rule exists to prevent does not apply to it — there is no
	// valid program to mangle. Re-run with the join tracking disabled and fail
	// closed instead, stripping every triple-quoted literal alone on its line.
	stripped, comments, _ = stripPythonPass(code, true)
	return stripped, comments
}

// stripPythonPass is one scan of stripPythonComments. ignoreJoins forces the
// bracket/continuation tracking off, so a literal alone on its *physical*
// line counts as a docstring; it also returns the bracket depth left at EOF
// so the caller can tell a balanced file from an uncompilable one.
func stripPythonPass(code string, ignoreJoins bool) (string, []string, int) {
	var out strings.Builder
	var comments []string
	n := len(code)
	i := 0
	// depth tracks open (), [] and {}. Inside them a physical line break is
	// not a logical one, so a triple-quoted literal that happens to sit alone
	// on its own physical line is still an ordinary expression operand — see
	// isDocstringPosition.
	depth := 0
	// continued is set while the previous physical line ended in a backslash
	// outside any literal or comment — Python's other line join, and the other
	// way a literal can look alone on its line without being a statement.
	continued := false

	for i < n {
		c := code[i]
		switch {
		case c == '(' || c == '[' || c == '{':
			depth++
			out.WriteByte(c)
			i++

		case c == ')' || c == ']' || c == '}':
			if depth > 0 {
				depth--
			}
			out.WriteByte(c)
			i++

		case c == '#':
			start := i
			for i < n && code[i] != '\n' {
				i++
			}
			comments = append(comments, code[start:i])

		case isPythonStringPrefixLetter(c) || c == '"' || c == '\'':
			quoteStart, quote, triple, ok := pythonStringLiteralStart(code, i)
			if !ok {
				out.WriteByte(c)
				i++
				continue
			}
			end := pythonStringEnd(code, quoteStart, quote, triple)
			joined := !ignoreJoins && (depth > 0 || continued)
			if triple && !joined && isDocstringPosition(code, i, end) {
				comments = append(comments, code[quoteStart:end])
				// A docstring is an expression *statement*, and it may be the
				// only statement in its suite: `def f():` followed by nothing
				// but a docstring is valid Python that deleting the literal
				// turns into an IndentationError. So a replacement statement
				// is emitted unconditionally rather than only when the suite
				// would otherwise be empty — deciding that needs the
				// indentation stack the scanner does not keep. The leading
				// whitespace of the line has already been written verbatim,
				// so the replacement lands at the docstring's own indentation.
				//
				// The replacement is an empty string literal, not `pass`: a
				// *module* docstring may be followed by `from __future__
				// import ...`, which must be the first statement of the file
				// bar comments and the docstring itself. `pass` there is a
				// SyntaxError in a program that compiled before the shield
				// touched it — the same mangled-input failure the
				// docstring-position rule exists to prevent. An empty string
				// literal is still a docstring to CPython's future-statement
				// scanner, and is legal in every other position `pass` is.
				out.WriteString(`""`)
				out.WriteString(strings.Repeat("\n", strings.Count(code[quoteStart:end], "\n")))
			} else {
				out.WriteString(code[i:end])
			}
			i = end

		case c == '\\' && lineBreakAt(code, i+1):
			continued = true
			end := i + 1
			if code[end] == '\r' {
				end++
			}
			end++ // the newline itself, so it does not reset continued below
			out.WriteString(code[i:end])
			i = end

		case c == '\n':
			continued = false
			out.WriteByte(c)
			i++

		default:
			out.WriteByte(c)
			i++
		}
	}

	return out.String(), comments, depth
}

// isDocstringPosition reports whether the literal spanning code[start:end]
// (start being the first character of the literal, prefix letters included)
// is alone on its logical line: only whitespace before it, only whitespace
// or a # comment after it. See stripPythonComments for why the distinction
// is load-bearing.
//
// "Logical" is the operative word, and the physical line is not enough on its
// own. Both of Python's line-joining rules put a literal alone on a physical
// line that is still the middle of an expression —
//
//	value = (
//	    """text"""
//	)
//	value = \
//	    """text"""
//
// — and deleting the literal there leaves `value = (` / `value = `, a
// SyntaxError in code that the repair model then has to explain and the judge
// has to compile. Both joins are tracked by the caller as it scans (depth and
// continued), because only the scanner knows whether a bracket or a trailing
// backslash was real or merely text inside a string or a comment.
func isDocstringPosition(code string, start, end int) bool {
	for i := start - 1; i >= 0 && code[i] != '\n'; i-- {
		if code[i] != ' ' && code[i] != '\t' && code[i] != '\r' {
			return false
		}
	}
	for i := end; i < len(code) && code[i] != '\n'; i++ {
		if code[i] == '#' {
			return true
		}
		if code[i] != ' ' && code[i] != '\t' && code[i] != '\r' {
			return false
		}
	}
	return true
}

// lineBreakAt reports whether a physical line break starts at i, LF or CRLF.
func lineBreakAt(code string, i int) bool {
	if i >= len(code) {
		return false
	}
	if code[i] == '\n' {
		return true
	}
	return code[i] == '\r' && i+1 < len(code) && code[i+1] == '\n'
}

func isPythonStringPrefixLetter(c byte) bool {
	switch c {
	case 'r', 'R', 'b', 'B', 'f', 'F', 'u', 'U':
		return true
	}
	return false
}

// pythonStringLiteralStart checks whether code[i:] begins a (possibly
// prefixed, e.g. rb"...") Python string literal. It returns the index of
// the opening quote, the quote character, whether it is triple-quoted, and
// whether a literal was found there at all.
func pythonStringLiteralStart(code string, i int) (quoteStart int, quote byte, triple bool, ok bool) {
	n := len(code)
	j := i
	for j < n && j-i < 2 && isPythonStringPrefixLetter(code[j]) {
		j++
	}
	if j >= n || (code[j] != '"' && code[j] != '\'') {
		return 0, 0, false, false
	}
	q := code[j]
	tripleQ := j+2 < n && code[j+1] == q && code[j+2] == q
	return j, q, tripleQ, true
}

func pythonStringEnd(code string, quoteStart int, quote byte, triple bool) int {
	n := len(code)
	if triple {
		i := quoteStart + 3
		for i < n {
			if code[i] == quote && i+2 < n && code[i+1] == quote && code[i+2] == quote {
				return i + 3
			}
			if code[i] == '\\' && i+1 < n {
				i += 2
				continue
			}
			i++
		}
		// Unterminated: fail closed at the opening line, exactly as the
		// C-family scanner does for raw strings, Java text blocks and Go
		// backtick strings (see unterminatedEnd). Running to len(code)
		// instead handed the entire rest of the file to a phantom literal,
		// so every later # comment — the payloads the shield exists to
		// remove — was emitted as ordinary code with Removed.Comments empty,
		// i.e. a bypass with no signal that anything was missed.
		return unterminatedEnd(code, quoteStart)
	}

	i := quoteStart + 1
	for i < n {
		if code[i] == quote {
			return i + 1
		}
		if code[i] == '\\' && i+1 < n {
			i += 2
			continue
		}
		if code[i] == '\n' {
			return i
		}
		i++
	}
	return n
}

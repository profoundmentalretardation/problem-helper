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
	var out strings.Builder
	var comments []string
	n := len(code)
	i := 0

	for i < n {
		c := code[i]
		switch {
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
			if triple && isDocstringPosition(code, i, end) {
				comments = append(comments, code[quoteStart:end])
				out.WriteString(strings.Repeat("\n", strings.Count(code[quoteStart:end], "\n")))
			} else {
				out.WriteString(code[i:end])
			}
			i = end

		default:
			out.WriteByte(c)
			i++
		}
	}

	return out.String(), comments
}

// isDocstringPosition reports whether the literal spanning code[start:end]
// (start being the first character of the literal, prefix letters included)
// is alone on its logical line: only whitespace before it, only whitespace
// or a # comment after it. See stripPythonComments for why the distinction
// is load-bearing.
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
		return n
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

package shield

import "strings"

// stripCLikeComments removes // and /* */ comments from C-family source
// (c, cpp, java, go) using a byte-level scan that skips over string, char,
// and (for go) raw-string literals so comment-like text inside them
// survives untouched. When protectPreprocessor is true, lines that are C/C++
// preprocessor directives (across backslash-newline continuations) are
// copied through unscanned, so #include/#define are never touched.
func stripCLikeComments(code string, protectPreprocessor bool) (string, []string) {
	var out strings.Builder
	var comments []string
	n := len(code)
	i := 0
	atLineStart := true

	for i < n {
		if atLineStart {
			atLineStart = false
			if protectPreprocessor {
				if end, ok := preprocessorLineEnd(code, i); ok {
					out.WriteString(code[i:end])
					i = end
					atLineStart = true
					continue
				}
			}
		}

		c := code[i]
		switch {
		case c == '\n':
			out.WriteByte(c)
			i++
			atLineStart = true

		case c == '/' && i+1 < n && code[i+1] == '/':
			start := i
			for i < n && code[i] != '\n' {
				i++
			}
			comments = append(comments, code[start:i])

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
			out.WriteString(strings.Repeat("\n", strings.Count(code[start:end], "\n")))
			i = end

		case c == '"':
			end := skipEscaped(code, i, '"')
			out.WriteString(code[i:end])
			i = end

		case c == '\'':
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

// preprocessorLineEnd returns the end offset (exclusive) of the C/C++
// preprocessor directive starting at i — leading horizontal whitespace then
// '#', following backslash-newline continuations — or ok=false if the line
// at i is not a directive.
func preprocessorLineEnd(code string, i int) (int, bool) {
	n := len(code)
	j := i
	for j < n && (code[j] == ' ' || code[j] == '\t') {
		j++
	}
	if j >= n || code[j] != '#' {
		return 0, false
	}
	for j < n {
		if code[j] == '\\' && j+1 < n && code[j+1] == '\n' {
			j += 2
			continue
		}
		if code[j] == '\n' {
			return j + 1, true
		}
		j++
	}
	return n, true
}

// skipEscaped returns the index just past the literal opened by quote at
// position i, honoring backslash escapes and never crossing that logical
// boundary — an unterminated literal runs to end of input.
func skipEscaped(code string, i int, quote byte) int {
	n := len(code)
	j := i + 1
	for j < n && code[j] != quote {
		if code[j] == '\\' && j+1 < n {
			j += 2
			continue
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

// Package shield sanitizes student code before it reaches a model: strips
// comments (dispatched by language, preprocessor directives preserved for
// C/C++), normalizes and strips invalid/confusable Unicode, and records a
// diff plus a structured report of what was removed. This is the MVP scope
// from the plan — identifier-level analysis is post-MVP.
package shield

import (
	"errors"
	"fmt"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
)

// Language is a submission language, as reported by the platform.
type Language string

const (
	LangC      Language = "c"
	LangCPP    Language = "cpp"
	LangJava   Language = "java"
	LangGo     Language = "go"
	LangPython Language = "python"
)

// ErrUnsupportedLanguage is returned by Strip for a language the shield does
// not know how to tokenize.
var ErrUnsupportedLanguage = errors.New("shield: unsupported language")

// languageAliases maps the language names platforms actually report onto the
// shield's own vocabulary. Platforms name languages after the compiler they
// invoke, not after the language: ejudge's run list reports its language
// short_name ("gcc", "g++", "python3", "java8"), so a submission's Language
// field almost never equals one of the Lang* constants. Strip consults this
// table before giving up, keeping the platform's original string intact for
// callers that need it back (SubmitAsSystem wants the short_name).
var languageAliases = map[string]Language{
	// C
	"c": LangC, "gcc": LangC, "cc": LangC, "clang": LangC, "c11": LangC,
	"c17": LangC, "gcc-vg": LangC, "c99": LangC,
	// C++
	"cpp": LangCPP, "c++": LangCPP, "g++": LangCPP, "gpp": LangCPP,
	"clang++": LangCPP, "clang-pp": LangCPP, "g++-vg": LangCPP,
	"cpp11": LangCPP, "cpp17": LangCPP, "cpp20": LangCPP,
	// Java
	"java": LangJava, "javac": LangJava, "java7": LangJava, "java8": LangJava,
	"java11": LangJava, "java17": LangJava, "java21": LangJava,
	// Go
	"go": LangGo, "golang": LangGo, "gccgo": LangGo,
	// Python
	"python": LangPython, "python2": LangPython, "python3": LangPython,
	"py": LangPython, "py3": LangPython, "pypy": LangPython,
	"pypy3": LangPython, "cpython": LangPython, "cpython3": LangPython,
}

// Canonical maps a platform-reported language name onto the shield's own
// Language vocabulary, reporting whether the language is supported at all.
func Canonical(language string) (Language, bool) {
	lang, ok := languageAliases[strings.ToLower(strings.TrimSpace(language))]
	return lang, ok
}

// UnicodeRemoval is one invalid/confusable character stripped from the code.
type UnicodeRemoval struct {
	Name      string `json:"name"`
	Codepoint string `json:"codepoint"`
}

// Removed is the structured report of everything Strip took out.
type Removed struct {
	Comments     []string         `json:"comments"`
	Unicode      []UnicodeRemoval `json:"unicode"`
	CommentCount int              `json:"comment_count"`
	UnicodeCount int              `json:"unicode_count"`
}

// Result is the outcome of shielding one submission's code.
type Result struct {
	CodeBefore string
	CodeAfter  string
	Diff       string
	Removed    Removed
}

// Strip removes comments (per language) and invalid/confusable Unicode from
// code, returning the cleaned text alongside a diff and a structured report
// of what was removed.
func Strip(code string, language string) (Result, error) {
	lang, ok := Canonical(language)
	if !ok {
		return Result{}, fmt.Errorf("%w: %q", ErrUnsupportedLanguage, language)
	}

	var withoutComments string
	var comments []string
	switch lang {
	case LangC, LangCPP:
		withoutComments, comments = stripCLikeComments(code, true)
	case LangJava, LangGo:
		withoutComments, comments = stripCLikeComments(code, false)
	case LangPython:
		withoutComments, comments = stripPythonComments(code)
	default:
		return Result{}, fmt.Errorf("%w: %q", ErrUnsupportedLanguage, language)
	}

	after, unicodeRemoved := sanitizeUnicode(withoutComments)

	diff, err := unifiedDiff(code, after)
	if err != nil {
		return Result{}, fmt.Errorf("shield: computing diff: %w", err)
	}

	return Result{
		CodeBefore: code,
		CodeAfter:  after,
		Diff:       diff,
		Removed: Removed{
			Comments:     comments,
			Unicode:      unicodeRemoved,
			CommentCount: len(comments),
			UnicodeCount: len(unicodeRemoved),
		},
	}, nil
}

func unifiedDiff(before, after string) (string, error) {
	if before == after {
		return "", nil
	}
	diff := difflib.UnifiedDiff{
		A:        difflib.SplitLines(before),
		B:        difflib.SplitLines(after),
		FromFile: "before",
		ToFile:   "after",
		Context:  3,
	}
	return difflib.GetUnifiedDiffString(diff)
}

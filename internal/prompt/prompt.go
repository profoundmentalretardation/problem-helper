// Package prompt loads and renders the system prompt templates in
// prompts/*.md. Templates use {{placeholder}} substitution: a placeholder
// absent from the values map at render time is an error, never a silent
// blank; a placeholder present with an empty string renders as the
// explicit text "none" (used for "no previous code" / "no recorded
// mistakes").
package prompt

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var placeholderPattern = regexp.MustCompile(`\{\{(.*?)\}\}`)

var validPlaceholderName = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// noneText is substituted for a placeholder whose value is the empty string.
const noneText = "none"

// Template is one parsed prompts/*.md file.
type Template struct {
	Name         string
	raw          string
	placeholders []string // deduplicated, first-appearance order
}

// MissingPlaceholderError reports placeholders the template requires that
// were absent from the values passed to Render.
type MissingPlaceholderError struct {
	Template string
	Names    []string
}

func (e *MissingPlaceholderError) Error() string {
	return fmt.Sprintf("prompt: template %q: missing value(s) for placeholder(s) %s",
		e.Template, strings.Join(e.Names, ", "))
}

// Parse validates raw as a template named name: every "{{" must close with
// a matching "}}" naming a bare identifier ([a-zA-Z0-9_]+).
func Parse(name, raw string) (Template, error) {
	if strings.Count(raw, "{{") != strings.Count(raw, "}}") {
		return Template{}, fmt.Errorf("prompt: template %q: unbalanced {{ }}", name)
	}

	seen := make(map[string]bool)
	var order []string
	for _, m := range placeholderPattern.FindAllStringSubmatch(raw, -1) {
		token := strings.TrimSpace(m[1])
		if !validPlaceholderName.MatchString(token) {
			return Template{}, fmt.Errorf("prompt: template %q: invalid placeholder %q", name, m[0])
		}
		if !seen[token] {
			seen[token] = true
			order = append(order, token)
		}
	}

	return Template{Name: name, raw: raw, placeholders: order}, nil
}

// Placeholders returns the template's placeholder names, deduplicated, in
// first-appearance order.
func (t Template) Placeholders() []string {
	out := make([]string, len(t.placeholders))
	copy(out, t.placeholders)
	return out
}

// Render substitutes every placeholder with its value from values. A
// placeholder missing from values is an error (never a silent blank); a
// placeholder present with an empty string renders as "none".
func (t Template) Render(values map[string]string) (string, error) {
	missing := make(map[string]bool)
	out := placeholderPattern.ReplaceAllStringFunc(t.raw, func(match string) string {
		name := strings.TrimSpace(match[2 : len(match)-2])
		v, ok := values[name]
		if !ok {
			missing[name] = true
			return match
		}
		if v == "" {
			return noneText
		}
		return v
	})
	if len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for n := range missing {
			names = append(names, n)
		}
		sort.Strings(names)
		return "", &MissingPlaceholderError{Template: t.Name, Names: names}
	}
	return out, nil
}

// LoadDir loads and validates every "*.md" file directly inside dir,
// keyed by filename without extension (e.g. "repair.md" -> "repair").
// Every file is parsed at load time, so a malformed template fails
// startup rather than surfacing later at render time.
func LoadDir(dir string) (map[string]Template, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("prompt: reading %s: %w", dir, err)
	}

	templates := make(map[string]Template)
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("prompt: reading %s: %w", path, err)
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		tmpl, err := Parse(name, string(data))
		if err != nil {
			return nil, err
		}
		templates[name] = tmpl
	}
	return templates, nil
}

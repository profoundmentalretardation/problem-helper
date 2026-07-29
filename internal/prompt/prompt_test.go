package prompt_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/profoundmentalretardation/problem-helper/internal/prompt"
)

func TestRender_SubstitutesPlaceholders(t *testing.T) {
	tmpl, err := prompt.Parse("repair", "Statement:\n{{problem_statement}}\n\nCode:\n{{user_code}}\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got, err := tmpl.Render(map[string]string{
		"problem_statement": "add two numbers",
		"user_code":         "print(1)",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := "Statement:\nadd two numbers\n\nCode:\nprint(1)\n"
	if got != want {
		t.Errorf("Render = %q, want %q", got, want)
	}
}

func TestRender_MissingPlaceholderIsError(t *testing.T) {
	tmpl, err := prompt.Parse("repair", "Code:\n{{user_code}}\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	_, err = tmpl.Render(map[string]string{})
	if err == nil {
		t.Fatal("Render with no values: want error, got nil (never a silent blank)")
	}
	var missingErr *prompt.MissingPlaceholderError
	if !errors.As(err, &missingErr) {
		t.Fatalf("Render error = %v, want *prompt.MissingPlaceholderError", err)
	}
	if len(missingErr.Names) != 1 || missingErr.Names[0] != "user_code" {
		t.Errorf("missingErr.Names = %v, want [user_code]", missingErr.Names)
	}
}

func TestRender_EmptyPreviousCodeAndMistakesRenderAsNone(t *testing.T) {
	tmpl, err := prompt.Parse("repair", "Previous:\n{{previous_code}}\n\nMistakes:\n{{mistakes}}\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got, err := tmpl.Render(map[string]string{
		"previous_code": "",
		"mistakes":      "",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := "Previous:\nnone\n\nMistakes:\nnone\n"
	if got != want {
		t.Errorf("Render = %q, want %q (empty values must render as explicit \"none\", not a blank)", got, want)
	}
}

func TestRender_NonEmptyValueIsUsedVerbatim(t *testing.T) {
	tmpl, err := prompt.Parse("repair", "{{mistakes}}")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got, err := tmpl.Render(map[string]string{"mistakes": "off_by_one x3"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "off_by_one x3" {
		t.Errorf("Render = %q, want %q", got, "off_by_one x3")
	}
}

func TestParse_RepeatedPlaceholderCountsOnce(t *testing.T) {
	tmpl, err := prompt.Parse("hint", "{{diff}} ... {{diff}}")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := tmpl.Placeholders(); len(got) != 1 || got[0] != "diff" {
		t.Errorf("Placeholders() = %v, want [diff]", got)
	}
	out, err := tmpl.Render(map[string]string{"diff": "X"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "X ... X" {
		t.Errorf("Render = %q, want %q", out, "X ... X")
	}
}

func TestParse_UnbalancedBracesIsError(t *testing.T) {
	if _, err := prompt.Parse("broken", "hello {{world"); err == nil {
		t.Fatal("Parse with unbalanced {{ }}: want error, got nil")
	}
}

func TestParse_InvalidPlaceholderNameIsError(t *testing.T) {
	if _, err := prompt.Parse("broken", "hello {{not a valid name}}"); err == nil {
		t.Fatal("Parse with a non-identifier placeholder: want error, got nil")
	}
}

func TestLoadDir_LoadsAndValidatesEveryFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "repair.md", "{{problem_statement}}")
	writeFile(t, dir, "hint.md", "{{diff}}")
	writeFile(t, dir, "notes.txt", "{{ignored, not a .md file")

	templates, err := prompt.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if _, ok := templates["repair"]; !ok {
		t.Errorf("templates = %v, want key %q", templates, "repair")
	}
	if _, ok := templates["hint"]; !ok {
		t.Errorf("templates = %v, want key %q", templates, "hint")
	}
	if _, ok := templates["notes"]; ok {
		t.Errorf("templates = %v, want non-.md file ignored", templates)
	}
}

func TestLoadDir_InvalidFileFailsStartup(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "broken.md", "hello {{world")

	if _, err := prompt.LoadDir(dir); err == nil {
		t.Fatal("LoadDir with an invalid template: want error, got nil (startup must validate every file)")
	}
}

func TestLoadDir_RealPromptsDirectoryValidates(t *testing.T) {
	root, err := filepath.Abs("../../prompts")
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	templates, err := prompt.LoadDir(root)
	if err != nil {
		t.Fatalf("LoadDir(%s): %v", root, err)
	}
	for _, name := range []string{"repair", "hint", "guardrail", "curator"} {
		if _, ok := templates[name]; !ok {
			t.Errorf("prompts/ missing %s.md", name)
		}
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", name, err)
	}
}

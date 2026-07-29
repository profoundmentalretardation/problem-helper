package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/profoundmentalretardation/problem-helper/internal/prompt"
)

// The checked-in prompts directory is the one that ships, so a real edit that
// drops a placeholder fails here rather than at first traffic.
func TestCheckPromptTemplates_RealPromptsDirectory(t *testing.T) {
	templates, err := prompt.LoadDir("../../prompts")
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if err := checkPromptTemplates("../../prompts", templates); err != nil {
		t.Fatalf("the shipped prompts must satisfy every agent's placeholder set: %v", err)
	}
}

// Both directions: a template missing a placeholder its agent renders must be
// a startup error, not a silent degradation. Render only catches the opposite
// direction, so nothing else would notice — and for the guardrail the failure
// is an unreviewed hint delivered on an explicit approval given without ever
// seeing a hint.
func TestCheckPromptTemplates_MissingPlaceholderIsAStartupError(t *testing.T) {
	cases := []struct {
		name, file, drop string
	}{
		{"guardrail without the hint", "guardrail.md", "hint"},
		{"repair without the student's code", "repair.md", "user_code"},
		{"hint without the diff", "hint.md", "diff"},
		{"curator without the raw mistakes", "curator.md", "raw_mistakes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range []string{"repair.md", "hint.md", "guardrail.md", "curator.md"} {
				raw, err := os.ReadFile(filepath.Join("../../prompts", name))
				if err != nil {
					t.Fatalf("reading %s: %v", name, err)
				}
				body := string(raw)
				if name == tc.file {
					body = strings.ReplaceAll(body, "{{"+tc.drop+"}}", "")
				}
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
					t.Fatalf("writing %s: %v", name, err)
				}
			}
			templates, err := prompt.LoadDir(dir)
			if err != nil {
				t.Fatalf("LoadDir: %v", err)
			}
			err = checkPromptTemplates(dir, templates)
			if err == nil {
				t.Fatalf("dropping {{%s}} from %s must fail startup", tc.drop, tc.file)
			}
			if !strings.Contains(err.Error(), tc.drop) {
				t.Errorf("error should name the missing placeholder, got: %v", err)
			}
		})
	}
}

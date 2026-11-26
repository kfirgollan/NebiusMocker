// Package docs checks the docs/ folder that GitHub Pages publishes.
package docs

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	rawOpen  = "<!-- {% raw %} -->"
	rawClose = "<!-- {% endraw %} -->"
)

// TestLiquidSafeDocs fails when a page under docs/ contains Liquid markers
// ("{{" or "{%") without the hidden raw guard. GitHub Pages renders every page
// through Liquid, so Go code such as `Path{{State: "X"}}` breaks the whole site
// build. Wrap such a page in rawOpen (first line) and rawClose (last line):
// Liquid then leaves it untouched, and GitHub's Markdown view hides the HTML
// comments.
func TestLiquidSafeDocs(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..", "docs")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".md" {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(data)
		body := strings.NewReplacer(rawOpen, "", rawClose, "").Replace(text)
		if !strings.Contains(body, "{{") && !strings.Contains(body, "{%") {
			return nil
		}
		trimmed := strings.TrimSpace(text)
		if !strings.HasPrefix(trimmed, rawOpen) || !strings.HasSuffix(trimmed, rawClose) {
			t.Errorf("%s contains Liquid markers ({{ or {%%): make %q its first line and %q its last line",
				path, rawOpen, rawClose)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

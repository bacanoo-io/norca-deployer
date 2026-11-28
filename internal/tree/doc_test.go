// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tree

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// hclBlock is a fenced hcl block of a Markdown file and the line it starts on.
type hclBlock struct {
	line int
	text string
}

// hclBlocks returns the fenced blocks of a Markdown text whose language is
// hcl.
func hclBlocks(text string) []hclBlock {
	var (
		blocks []hclBlock
		cur    *hclBlock
		body   strings.Builder
	)

	for i, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)

		switch {
		case cur == nil && trimmed == "```hcl":
			cur = &hclBlock{line: i + 1}
			body.Reset()
		case cur != nil && trimmed == "```":
			cur.text = body.String()
			blocks = append(blocks, *cur)
			cur = nil
		case cur != nil:
			body.WriteString(line)
			body.WriteByte('\n')
		}
	}

	return blocks
}

// Every hcl example of the design of the tree and of its documentation is
// HCL, so that the documents can't drift from what a loader reads
// (implementation plan T0-4).
func TestDocumentsHaveValidHCL(t *testing.T) {
	t.Parallel()

	files := []string{filepath.Join("..", "..", "design", "platform-management-design.md")}

	docs, err := filepath.Glob(filepath.Join("..", "..", "doc", "tree*.md"))
	if err != nil {
		t.Fatal(err)
	}

	files = append(files, docs...)

	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}

		blocks := hclBlocks(string(data))
		if len(blocks) == 0 {
			t.Errorf("%s: no hcl block found", file)
		}

		for _, b := range blocks {
			name := fmt.Sprintf("%s:%d", filepath.Base(file), b.line)

			_, diags := hclsyntax.ParseConfig([]byte(b.text), name, hcl.Pos{Line: b.line + 1, Column: 1})
			if diags.HasErrors() {
				t.Errorf("%s: %s", name, diags.Error())
			}
		}
	}
}

func TestHCLBlocks(t *testing.T) {
	t.Parallel()

	text := "a\n```hcl\nx = 1\n```\n```text\nno\n```\n  ```hcl\ny = 2\n  ```\n"

	blocks := hclBlocks(text)
	if len(blocks) != 2 || blocks[0].line != 2 || blocks[0].text != "x = 1\n" || blocks[1].text != "y = 2\n" {
		t.Fatalf("hclBlocks = %+v", blocks)
	}
}

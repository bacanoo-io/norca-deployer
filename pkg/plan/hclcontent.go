// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package plan

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// A key of a plan whose value is text in a language of its own (a stack.hcl,
// settings of the agent, an ACL policy) has the struct tag plan:"content".
// In an HCL plan such a text may be written as a block of the key: the body
// of the block is the text.
const (
	contentTag   = "plan"
	contentValue = "content"
)

// isContent reports whether the field holds the text of a content key.
func isContent(f reflect.StructField) bool { return f.Tag.Get(contentTag) == contentValue }

// cutPlan is an HCL plan prepared for the HCL v1 decoder.
type cutPlan struct {
	// src is the plan with every content block replaced by an attribute
	// whose value is a placeholder; the lines of the rest stay where they
	// were, so the errors of the decoder name the lines of the file.
	src []byte
	// texts are the bodies of the content blocks, by their placeholder.
	texts map[string]string
	// notHCL2 is why the plan does not parse as HCL2 syntax. src is then the
	// plan as it is, and a content block in it is no text.
	notHCL2 error
}

// cutContentBlocks takes the content blocks out of an HCL plan. The HCL v1
// decoder cannot read the body of such a block: a stack has expressions of
// HCL2 (module.x.outputs.y, function calls, for, %{ }). The blocks are found
// with the HCL2 parser, at the places the plan types have content keys only,
// and their bodies are taken from the source byte for byte, one level of
// indentation less: comments, heredocs, ${} and %{} stay as written. The text
// never goes through a string of HCL v1, which would keep an escaped $${ as
// it is.
//
// A plan that is no HCL2 syntax (a few things HCL v1 has, or a ${X:-y} of a
// shell in a script) is left as it is, with the reason in notHCL2.
func cutContentBlocks(data []byte, filename string) (cutPlan, error) {
	file, diags := hclsyntax.ParseConfig(data, filename, hcl.InitialPos)
	if diags.HasErrors() {
		return cutPlan{src: data, notHCL2: diags}, nil
	}

	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return cutPlan{src: data}, nil
	}

	c := &contentCutter{src: data, filename: filename}
	if err := c.walk(body, reflect.TypeFor[DeploymentPlan](), ""); err != nil {
		return cutPlan{}, err
	}

	if len(c.blocks) == 0 {
		return cutPlan{src: data}, nil
	}

	return c.result(), nil
}

// contentCutter collects the content blocks of a plan.
type contentCutter struct {
	src      []byte
	filename string
	blocks   []contentBlock
	// heredocs are the heredocs of the plan, lexed when the first block is
	// cut.
	heredocs []heredocLines
	lexed    bool
}

// contentBlock is a content block of the plan and its text.
type contentBlock struct {
	block *hclsyntax.Block
	text  string
}

// walk finds the content blocks of a body whose keys are those of t, and
// goes into the blocks of the keys that are structs.
func (c *contentCutter) walk(body *hclsyntax.Body, t reflect.Type, path string) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	if t.Kind() != reflect.Struct {
		return nil
	}

	seen := map[string]*hclsyntax.Block{}
	count := map[string]int{}

	for _, b := range body.Blocks {
		f, ok := fieldByTag(t, b.Type)
		if !ok {
			continue // the decoder names an unknown key
		}

		at := join(path, b.Type)

		if isContent(f) {
			if err := c.cut(body, b, at, seen[b.Type]); err != nil {
				return err
			}

			seen[b.Type] = b

			continue
		}

		inner := f.Type
		for inner.Kind() == reflect.Pointer {
			inner = inner.Elem()
		}

		if inner.Kind() == reflect.Slice {
			at = fmt.Sprintf("%s[%d]", at, count[b.Type])
			count[b.Type]++
			inner = inner.Elem()
		}

		if err := c.walk(b.Body, inner, at); err != nil {
			return err
		}
	}

	return nil
}

// cut takes the text of a content block; earlier is the block of the same
// key before it in the body, if any.
func (c *contentCutter) cut(body *hclsyntax.Body, b *hclsyntax.Block, at string, earlier *hclsyntax.Block) error {
	switch attr, isAttr := body.Attributes[b.Type]; {
	case len(b.Labels) > 0:
		return c.errorf(b.TypeRange, "%s: a block of text has no labels", at)
	case isAttr:
		return c.errorf(b.TypeRange, "%s: given as a block and as %s = (line %d); write it once",
			at, b.Type, attr.SrcRange.Start.Line)
	case earlier != nil:
		return c.errorf(b.TypeRange, "%s: block given twice (first on line %d)", at, earlier.TypeRange.Start.Line)
	}

	text := c.blockText(b)
	if text == "" {
		return c.errorf(b.TypeRange, "%s: the block is empty", at)
	}

	c.blocks = append(c.blocks, contentBlock{block: b, text: text})

	return nil
}

func (c *contentCutter) errorf(r hcl.Range, format string, args ...any) error {
	return fmt.Errorf("%s:%d: %s", r.Filename, r.Start.Line, fmt.Sprintf(format, args...))
}

// result replaces every block by an attribute of the same key whose value
// is a placeholder, followed by as many newlines as the block had, so that
// the lines after it keep their numbers.
func (c *contentCutter) result() cutPlan {
	// The placeholder is no text a plan would have.
	nonce := "norca-deployer-content-" + rand.Text()
	res := cutPlan{texts: make(map[string]string, len(c.blocks))}
	src := slices.Clone(c.src)

	// From the last block back, so the offsets of the others stay valid.
	for i, cb := range slices.Backward(c.blocks) {
		b := cb.block
		rng := b.Range()
		placeholder := fmt.Sprintf("%s-%d", nonce, i)
		res.texts[placeholder] = cb.text

		attr := fmt.Sprintf("%s = %q%s", b.Type, placeholder, strings.Repeat("\n", rng.End.Line-rng.Start.Line))
		src = slices.Concat(src[:rng.Start.Byte], []byte(attr), src[rng.End.Byte:])
	}

	res.src = src

	return res
}

// heredocLines are the lines of a heredoc after its opening line, up to and
// with its closing marker.
type heredocLines struct {
	first, last int
	// flush is a <<- heredoc, whose lines lose the indentation they share.
	flush bool
}

// lexHeredocs finds the heredocs of the plan. A heredoc inside another one
// (in an interpolation) is part of the outer one, which is then kept as it
// is.
func (c *contentCutter) lexHeredocs() {
	tokens, _ := hclsyntax.LexConfig(c.src, c.filename, hcl.InitialPos)

	var (
		depth  int
		outer  hclsyntax.Token
		nested bool
	)

	for _, tok := range tokens {
		switch tok.Type {
		case hclsyntax.TokenOHeredoc:
			if depth == 0 {
				outer, nested = tok, false
			} else {
				nested = true
			}

			depth++
		case hclsyntax.TokenCHeredoc:
			if depth == 0 {
				continue
			}

			depth--
			if depth == 0 {
				c.heredocs = append(c.heredocs, heredocLines{
					first: outer.Range.Start.Line + 1,
					last:  tok.Range.Start.Line,
					flush: bytes.HasPrefix(outer.Bytes, []byte("<<-")) && !nested,
				})
			}
		}
	}
}

// heredocAt returns the heredoc a line of the plan is in.
func (c *contentCutter) heredocAt(line int) (int, bool) {
	for i, h := range c.heredocs {
		if line >= h.first && line <= h.last {
			return i, true
		}
	}

	return 0, false
}

// blockText is the body of a block, one level of indentation less: the
// leading whitespace its lines share is removed. The lines of a heredoc are
// kept as they are, but for a <<- heredoc whose lines (but blank ones) all
// have that indentation, which loses it too: HCL removes the indentation
// such a heredoc's lines share, so its value stays the same. A comment after
// the opening brace is the first line of the text. Line ends are LF.
func (c *contentCutter) blockText(b *hclsyntax.Block) string {
	if !c.lexed {
		c.lexHeredocs()
		c.lexed = true
	}

	raw := strings.Split(string(c.src[b.OpenBraceRange.End.Byte:b.CloseBraceRange.Start.Byte]), "\n")
	firstLine := b.OpenBraceRange.End.Line

	if len(raw) == 1 {
		// A block of one line: extra { max_kill_timeout = "60s" }
		if text := strings.TrimSpace(raw[0]); text != "" {
			return text + "\n"
		}

		return ""
	}

	lines := make([]string, len(raw))
	inHeredoc := make([]int, len(raw)) // the heredoc of a line, plus one; 0 is none

	var indents []string

	for i, l := range raw {
		lines[i] = strings.TrimSuffix(l, "\r")
		if i == 0 {
			continue
		}

		if h, ok := c.heredocAt(firstLine + i); ok {
			inHeredoc[i] = h + 1

			continue
		}

		if strings.TrimSpace(lines[i]) == "" {
			lines[i] = ""

			continue
		}

		indents = append(indents, lines[i][:len(lines[i])-len(strings.TrimLeft(lines[i], " \t"))])
	}

	prefix := commonPrefix(indents)
	dedent := c.flushHeredocsToDedent(lines, inHeredoc, prefix)

	for i := 1; i < len(lines); i++ {
		// A blank line of a heredoc keeps its whitespace: HCL leaves it
		// in the value as it is.
		switch h := inHeredoc[i]; {
		case h == 0:
			lines[i] = strings.TrimPrefix(lines[i], prefix)
		case dedent[h-1] && strings.TrimSpace(lines[i]) != "":
			lines[i] = strings.TrimPrefix(lines[i], prefix)
		}
	}

	// The line of the opening brace: a comment after it.
	lines[0] = strings.TrimSpace(lines[0])

	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}

	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	if len(lines) == 0 {
		return ""
	}

	return strings.Join(lines, "\n") + "\n"
}

// flushHeredocsToDedent reports for every heredoc of the plan whether its
// lines in this block lose the prefix: a <<- heredoc whose lines all have
// it, but blank ones.
func (c *contentCutter) flushHeredocsToDedent(lines []string, inHeredoc []int, prefix string) []bool {
	dedent := make([]bool, len(c.heredocs))
	for i, h := range c.heredocs {
		dedent[i] = h.flush && prefix != ""
	}

	for i, l := range lines {
		if h := inHeredoc[i]; h != 0 && strings.TrimSpace(l) != "" && !strings.HasPrefix(l, prefix) {
			dedent[h-1] = false
		}
	}

	return dedent
}

// commonPrefix is the longest prefix the strings share.
func commonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}

	prefix := ss[0]
	for _, s := range ss[1:] {
		for !strings.HasPrefix(s, prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}

	return prefix
}

// restoreTexts puts the texts of the content blocks in place of their
// placeholders in a plan decoded by HCL v1.
func restoreTexts(v any, texts map[string]string) {
	switch o := v.(type) {
	case map[string]any:
		for key, val := range o {
			if s, ok := val.(string); ok {
				if text, found := texts[s]; found {
					o[key] = text
				}

				continue
			}

			restoreTexts(val, texts)
		}
	case []map[string]any:
		for _, m := range o {
			restoreTexts(m, texts)
		}
	case []any:
		for _, item := range o {
			restoreTexts(item, texts)
		}
	}
}

// contentFormError is a content key whose value is neither a string nor a
// block of text.
type contentFormError struct {
	path string
}

func (e *contentFormError) Error() string {
	return e.path + ": text: a string, or a block whose body is the text"
}

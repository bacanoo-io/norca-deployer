package step

import (
	"regexp"
	"strings"
)

// funcStart is the first line of a shell function definition at column 0.
var funcStart = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\(\) \{`)

// shellFunc is one function of a set of definitions, with the comment
// lines before it.
type shellFunc struct {
	name string
	text string
}

// parseFunctions splits POSIX sh function definitions: a function starts at
// "name() {" in column 0 and ends on the same line or at "}" in column 0;
// the comment lines right before it belong to it.
func parseFunctions(defs string) []shellFunc {
	var res []shellFunc
	var comments []string
	var cur *shellFunc
	var body []string
	for line := range strings.SplitSeq(defs, "\n") {
		if cur != nil {
			body = append(body, line)
			if line == "}" {
				cur.text = strings.Join(body, "\n")
				res = append(res, *cur)
				cur, body = nil, nil
			}
			continue
		}
		m := funcStart.FindStringSubmatch(line)
		switch {
		case m == nil && strings.HasPrefix(line, "#"):
			comments = append(comments, line)
		case m == nil:
			comments = nil
		case strings.HasSuffix(strings.TrimSpace(line), "}"):
			res = append(res, shellFunc{name: m[1], text: strings.Join(append(comments, line), "\n")})
			comments = nil
		default:
			cur = &shellFunc{name: m[1]}
			body = append(comments, line)
			comments = nil
		}
	}
	return res
}

// wordRe finds the words of a script that can name a function.
var wordRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// UsedFunctions returns the definitions of defs that the bodies call, and
// the ones those call, in the order of defs: a command or a script holds no
// function it does not run.
func UsedFunctions(defs string, bodies ...string) string {
	funcs := parseFunctions(defs)
	byName := make(map[string]string, len(funcs))
	for _, f := range funcs {
		byName[f.name] = f.text
	}
	used := map[string]bool{}
	var visit func(text string)
	visit = func(text string) {
		for _, w := range wordRe.FindAllString(text, -1) {
			if body, ok := byName[w]; ok && !used[w] {
				used[w] = true
				visit(body)
			}
		}
	}
	for _, b := range bodies {
		visit(b)
	}
	var out strings.Builder
	for _, f := range funcs {
		if used[f.name] {
			out.WriteString(f.text + "\n")
		}
	}
	return out.String()
}

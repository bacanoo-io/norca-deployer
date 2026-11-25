package step

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
)

// RenderOptions are the choices of Render.
type RenderOptions struct {
	// Boot refuses the steps with a credential: the script travels in the
	// user data of the machine at boot.
	Boot bool
	// BootHint is the advice of the error of such a step.
	BootHint string
}

// Functions returns the functions of the steps, each set once, in the order
// of the steps.
func Functions(steps []Step) string {
	var b strings.Builder
	seen := map[string]bool{}
	for _, s := range steps {
		if s.Functions == "" || seen[s.Functions] {
			continue
		}
		seen[s.Functions] = true
		b.WriteString(strings.TrimRight(s.Functions, "\n") + "\n")
	}
	return b.String()
}

// Render renders steps into POSIX sh for the install script of a scaled
// machine: a second run changes nothing. The script defines the functions
// of the steps (Functions) and these helpers before, with the variables
// changed, wrote and removed:
//
//	put_file PATH MODE [base64]  writes stdin (or its base64) unless PATH has it; wrote=1 when it changed
//	remove_file PATH             removes a file an earlier plan wrote; removed=1 when it did
//	ensure_dir PATH MODE         creates a directory or sets its mode
//	step_lines NAME RC OUTPUT    reads the result lines of a step (Lines)
//	log, warn, die               the console of the script
func Render(steps []Step, opts RenderOptions) (string, error) {
	var b strings.Builder
	for i := range steps {
		if err := renderStep(&b, &steps[i], opts); err != nil {
			return "", err
		}
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

func renderStep(b *strings.Builder, s *Step, opts RenderOptions) error {
	if err := s.validate(); err != nil {
		return err
	}
	if s.PowerShell {
		return fmt.Errorf("step %q is a step of a Windows node: the install script is for Linux machines", s.Name)
	}
	if s.Secret && opts.Boot {
		return fmt.Errorf("%s has credentials: at boot they would travel in the user data of the machine; %s", s.Name, opts.BootHint)
	}
	for _, f := range append(append([]File{}, s.Files...), s.Inputs...) {
		if f.Local != "" {
			return fmt.Errorf("step %q: %s is a file of the deployer machine, which the machine cannot reach", s.Name, f.Path)
		}
	}
	if len(s.Inputs) > 0 {
		return fmt.Errorf("step %q: inputs are copied over SSH; a step of the install script downloads its inputs in Apply", s.Name)
	}

	fmt.Fprintf(b, "# %s\n", oneLine(s.Name))
	indent := ""
	if s.When != "" {
		fmt.Fprintf(b, "if {\n%s\n}; then\n", strings.TrimRight(s.When, "\n"))
		indent = "  "
	}
	tracksFiles := s.Check != "" && (len(s.Files) > 0 || len(s.Remove) > 0)
	if tracksFiles {
		b.WriteString(indent + "step_changed=0\n")
	}
	for _, d := range s.Dirs {
		fmt.Fprintf(b, "%s%s %s %04o\n", indent, "ensure_dir", connector.ShellQuote(d.Path), d.Mode.Perm())
	}
	for _, f := range s.Files {
		// A here-document is not indented: its lines are the content.
		b.WriteString(ShellFile(f.Path, fmt.Sprintf("%04o", f.Mode.Perm()), f.Content) + "\n")
		if tracksFiles {
			b.WriteString(indent + "[ \"$wrote\" = 0 ] || step_changed=1\n")
		}
	}
	for _, p := range s.Remove {
		fmt.Fprintf(b, "%sremove_file %s\n", indent, connector.ShellQuote(p))
		if tracksFiles {
			b.WriteString(indent + "[ \"$removed\" = 0 ] || step_changed=1\n")
		}
	}
	apply := strings.TrimRight(s.Apply, "\n")
	switch {
	case apply == "":
	case s.Lines:
		fmt.Fprintf(b, "%sif step_out=$(\n%s\n); then\n%s  step_rc=0\n%selse\n%s  step_rc=1\n%sfi\n", indent, apply, indent, indent, indent, indent)
		fmt.Fprintf(b, "%sstep_lines %s \"$step_rc\" \"$step_out\"\n", indent, connector.ShellQuote(s.Name))
	case s.Check == "":
		b.WriteString(apply + "\n")
	default:
		cond := "! {\n" + strings.TrimRight(s.Check, "\n") + "\n}"
		if tracksFiles {
			cond = "[ \"$step_changed\" = 1 ] || " + cond
		}
		fmt.Fprintf(b, "%sif %s; then\n%s\n%s  changed=1\n%sfi\n", indent, cond, apply, indent, indent)
	}
	if s.When != "" {
		b.WriteString("fi\n")
	}
	b.WriteString("\n")
	return nil
}

// ShellFile renders a put_file call that writes content to path: a quoted
// here-document for text, base64 for anything else (a binary key, a file
// without a final newline), so that the file is written byte for byte.
func ShellFile(path, mode string, content []byte) string {
	const eof = "NORCA_EOF"
	text := utf8.Valid(content) && bytes.HasSuffix(content, []byte("\n")) &&
		!bytes.ContainsAny(content, "\x00\r") && !bytes.Contains(append([]byte("\n"), content...), []byte("\n"+eof+"\n"))
	if text {
		return "put_file " + connector.ShellQuote(path) + " " + mode + " <<'" + eof + "'\n" + string(content) + eof
	}
	enc := base64.StdEncoding.EncodeToString(content)
	var lines []string
	for len(enc) > 76 {
		lines = append(lines, enc[:76])
		enc = enc[76:]
	}
	lines = append(lines, enc)
	return "put_file " + connector.ShellQuote(path) + " " + mode + " base64 <<'" + eof + "'\n" +
		strings.Join(lines, "\n") + "\n" + eof
}

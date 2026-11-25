package plan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// planFile writes a plan of one server with the given body and loads it.
func loadBody(t *testing.T, name, body string) (*DeploymentPlan, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "repo.asc"), []byte(testKey), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadFromFile(path)
}

// The packages of the nodes whose package manager is apt are a block of
// their own; the lists of earlier plans and the blocks that follow later
// are refused with their reason, in YAML and in HCL alike.
func TestHostPackagesBlocks(t *testing.T) {
	const inventoryYAML = "inventory:\n  masters:\n    - host: 10.0.0.1\n"
	const inventoryHCL = "inventory {\n  masters {\n    host = \"10.0.0.1\"\n  }\n}\n"
	tests := []struct {
		name string
		file string
		body string
		err  string // "" is valid
	}{
		{"apt block", "p.yaml", "host_packages:\n  apt:\n    repositories:\n      - {name: internal, uri: https://apt.example.com/debian, suite: bookworm, components: [main], key_file: repo.asc}\n    install:\n      - {name: jq, hold: true}\n", ""},
		{"apt block in hcl", "p.hcl", "host_packages {\n  apt {\n    repository {\n    }\n  }\n}\n", "unknown key \"repository\""},
		{"apt block in hcl with lists", "p.hcl", "host_packages {\n  apt {\n    repositories {\n      name = \"internal\"\n      uri = \"https://apt.example.com/debian\"\n      suite = \"bookworm\"\n      components = [\"main\"]\n      key_file = \"repo.asc\"\n    }\n    install {\n      name = \"jq\"\n    }\n  }\n}\n", ""},
		{"old repositories", "p.yaml", "host_packages:\n  repositories:\n    - {name: internal}\n", "moved into host_packages.apt"},
		{"old install", "p.yaml", "host_packages:\n  install:\n    - name: jq\n", "moved into host_packages.apt"},
		{"old install in hcl", "p.hcl", "host_packages {\n  install {\n    name = \"jq\"\n  }\n}\n", "moved into host_packages.apt"},
		{"dnf", "p.yaml", "host_packages:\n  dnf:\n    install:\n      - name: jq\n", "host_packages.dnf is not supported yet"},
		{"apk in hcl", "p.hcl", "host_packages {\n  apk {\n    install {\n      name = \"jq\"\n    }\n  }\n}\n", "host_packages.apk is not supported yet"},
		{"rules of debian in the apt block", "p.yaml", "host_packages:\n  apt:\n    install:\n      - {name: Not_Debian}\n", "host_packages.apt.install[0]: invalid Debian package name"},
		{"baseurl is no key of apt", "p.yaml", "host_packages:\n  apt:\n    repositories:\n      - {name: internal, baseurl: https://x.example.com, key_file: repo.asc}\n", "host_packages.apt.repositories[0]: uri"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv := inventoryYAML
			if strings.HasSuffix(tt.file, ".hcl") {
				inv = inventoryHCL
			}
			plan, err := loadBody(t, tt.file, tt.body+inv)
			if tt.err == "" {
				if err != nil {
					t.Fatal(err)
				}
				if plan.AptPackages() == nil || len(plan.AptPackages().Install) != 1 {
					t.Fatalf("apt block %+v", plan.AptPackages())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Fatalf("err %v, want %q", err, tt.err)
			}
		})
	}
}

func TestScriptInterpreterAndFamilies(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(s *ScriptConfig)
		err    string
	}{
		{"default bash", func(*ScriptConfig) {}, ""},
		{"sh", func(s *ScriptConfig) { s.Interpreter = InterpreterSh }, ""},
		{"bash", func(s *ScriptConfig) { s.Interpreter = InterpreterBash }, ""},
		{"powershell follows", func(s *ScriptConfig) { s.Interpreter = "powershell" }, "not supported yet"},
		{"unknown interpreter", func(s *ScriptConfig) { s.Interpreter = "zsh" }, "must be bash or sh"},
		{"families", func(s *ScriptConfig) { s.Families = []string{FamilyDebian, FamilyRHEL, FamilyAlpine, FamilyWindows} }, ""},
		{"unknown family", func(s *ScriptConfig) { s.Families = []string{"suse"} }, "family \"suse\""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validPlan()
			tt.mutate(&p.Scripts[0])
			err := p.Validate()
			if tt.err == "" && err != nil {
				t.Fatal(err)
			}
			if tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)) {
				t.Fatalf("err %v, want %q", err, tt.err)
			}
		})
	}
	if got := ScriptInterpreter(&ScriptConfig{}); got != InterpreterBash {
		t.Fatalf("the default interpreter is %q", got)
	}
}

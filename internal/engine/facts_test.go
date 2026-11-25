package engine

import (
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
	"github.com/bacanoo-io/norca-deployer/internal/host"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// debianProbe is what the probe of the facts reads on a Debian 12 node
// with systemd, as root.
const debianProbe = `PRETTY_NAME="Debian GNU/Linux 12 (bookworm)"
NAME="Debian GNU/Linux"
VERSION_ID="12"
VERSION="12 (bookworm)"
VERSION_CODENAME=bookworm
ID=debian
--
x86_64
--
systemd
/run/systemd/system
--
apt-get
sudo
bash
iptables
systemctl
ss
--
--
cgroup2fs
--
0
`

// probed returns a fake connector of a node that answers the probe with
// out.
func probed(out string) *connector.FakeConnector {
	f := connector.NewFakeConnector()
	f.Responses[host.ProbeCommand] = out
	return f
}

// withProbe makes a probe output of debianProbe with other lines.
func withProbe(replace ...string) string {
	return strings.NewReplacer(replace...).Replace(debianProbe)
}

func TestOpenNode(t *testing.T) {
	tests := []struct {
		name   string
		probe  string
		conn   config.ConnectionConfig
		err    string
		escal  string
		family host.Family
	}{
		{"debian as root", debianProbe, config.ConnectionConfig{Mode: "ssh"}, "", host.EscalationNone, host.FamilyDebian},
		{"sudo", withProbe("\n0\n", "\n1000\n"), config.ConnectionConfig{Mode: "ssh", Become: true}, "", host.EscalationSudo, host.FamilyDebian},
		{"doas without sudo", withProbe("\n0\n", "\n1000\n", "sudo\n", "doas\n"), config.ConnectionConfig{Mode: "ssh", Become: true},
			"", host.EscalationDoas, host.FamilyDebian},
		{"neither", withProbe("\n0\n", "\n1000\n", "sudo\n", ""), config.ConnectionConfig{Mode: "ssh", Become: true},
			"neither sudo nor doas", "", ""},
		{"unknown system", withProbe("ID=debian", "ID=arch", "apt-get\n", ""), config.ConnectionConfig{Mode: "ssh"},
			"the node is not supported", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRunner(&config.DeploymentPlan{}, nil)
			conn := probed(tt.probe)
			r.connect = func(*config.NodeConfig) (connector.Connector, error) { return conn, nil }
			_, facts, err := r.openNode(&config.NodeConfig{Host: "10.0.0.1", Platform: "linux", Arch: "amd64", Connection: tt.conn})
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err %v, want %q", err, tt.err)
				}
				if len(conn.Commands) != 1 {
					t.Fatalf("commands after the probe: %q", conn.Commands)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if facts.Escalation != tt.escal || facts.Family != tt.family {
				t.Fatalf("facts %+v", facts)
			}
		})
	}
}

// A node whose package manager has no block of host_packages is an error of
// the plan before anything is changed on any node.
func TestCheckFacts(t *testing.T) {
	plan := &config.DeploymentPlan{
		HostPackages: &config.HostPackages{Apt: &config.AptPackages{Install: []config.HostPackage{{Name: "jq", Roles: []string{config.RoleClient}}}}},
	}
	node := func(probe string, isMaster bool) *target {
		f, err := host.Parse(probe)
		if err != nil {
			t.Fatal(err)
		}
		return &target{node: &config.NodeConfig{Host: "10.0.0.5", Platform: "linux"}, isMaster: isMaster, facts: f}
	}
	r := NewRunner(plan, nil)
	if err := r.checkFacts([]*target{node(debianProbe, false)}); err != nil {
		t.Fatalf("a debian client: %v", err)
	}
	// A node without a package manager and nothing to install.
	none := withProbe("apt-get\n", "")
	noneMaster := node(none, true)
	noneMaster.facts.Override(&config.HostFacts{PackageManager: config.PackageManagerNone})
	if err := r.checkFacts([]*target{noneMaster}); err != nil {
		t.Fatalf("a server without packages: %v", err)
	}
	noneClient := node(none, false)
	noneClient.facts.Override(&config.HostFacts{PackageManager: config.PackageManagerNone})
	err := r.checkFacts([]*target{node(debianProbe, false), noneClient})
	if err == nil || !strings.Contains(err.Error(), "node [10.0.0.5]: the node uses none, but host_packages has packages for its roles only in apt") ||
		!strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("a client without apt: %v", err)
	}
}

// release.source host-package needs the package in the block of every
// node's package manager, for that node.
func TestCheckFactsHostPackageRelease(t *testing.T) {
	plan := &config.DeploymentPlan{
		Release: config.ReleaseConfig{Source: config.ReleaseHostPackage, Package: "norca"},
		HostPackages: &config.HostPackages{Apt: &config.AptPackages{Install: []config.HostPackage{
			{Name: "norca", Roles: []string{config.RoleMaster}},
		}}},
	}
	r := NewRunner(plan, nil)
	facts, err := host.Parse(debianProbe)
	if err != nil {
		t.Fatal(err)
	}
	server := &target{node: &config.NodeConfig{Host: "10.0.0.1", Platform: "linux"}, isMaster: true, facts: facts}
	client := &target{node: &config.NodeConfig{Host: "10.0.0.2", Platform: "linux"}, facts: facts}
	if err := r.checkFacts([]*target{server}); err != nil {
		t.Fatalf("the server has the package: %v", err)
	}
	err = r.checkFacts([]*target{server, client})
	if err == nil || !strings.Contains(err.Error(), `node [10.0.0.2]: release.source host-package: the block of host_packages of the node's package manager (apt) does not install "norca"`) ||
		strings.Contains(err.Error(), "10.0.0.1") {
		t.Fatalf("the client without the package: %v", err)
	}
}

// A bash script on a node without bash is an error of the node, unless the
// packages that run before it install bash.
func TestCheckFactsBash(t *testing.T) {
	noBash, err := host.Parse(withProbe("bash\n", ""))
	if err != nil {
		t.Fatal(err)
	}
	node := &config.NodeConfig{Host: "10.0.0.3", Platform: "linux"}
	check := func(plan *config.DeploymentPlan) error {
		return NewRunner(plan, nil).checkFacts([]*target{{node: node, facts: noBash}})
	}
	pre := config.ScriptConfig{Name: "pre", Phase: config.PhasePreInstall, Inline: "true"}
	post := config.ScriptConfig{Name: "post", Phase: config.PhasePostPackages, Inline: "true"}
	withBash := &config.HostPackages{Apt: &config.AptPackages{Install: []config.HostPackage{{Name: "bash"}}}}

	if err := check(&config.DeploymentPlan{Scripts: []config.ScriptConfig{pre}}); err == nil ||
		!strings.Contains(err.Error(), `script "pre" runs with bash, which the node does not have`) {
		t.Fatalf("pre_install with bash: %v", err)
	}
	pre.Interpreter = config.InterpreterSh
	if err := check(&config.DeploymentPlan{Scripts: []config.ScriptConfig{pre, post}, HostPackages: withBash}); err != nil {
		t.Fatalf("sh, and bash from host_packages: %v", err)
	}
	if err := check(&config.DeploymentPlan{Scripts: []config.ScriptConfig{post}}); err == nil {
		t.Fatal("post_packages with bash and no bash package")
	}
	post.Families = []string{config.FamilyRHEL}
	if err := check(&config.DeploymentPlan{Scripts: []config.ScriptConfig{post}}); err != nil {
		t.Fatalf("a script of another family: %v", err)
	}
}

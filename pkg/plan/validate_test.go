package plan

import (
	"strings"
	"testing"
)

func TestValidateBecomeMethodAndHostFacts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(n *NodeConfig)
		err    string // "" is valid
	}{
		{"sudo", func(n *NodeConfig) { n.Connection.Become, n.Connection.BecomeMethod = true, BecomeSudo }, ""},
		{"doas", func(n *NodeConfig) { n.Connection.Become, n.Connection.BecomeMethod = true, BecomeDoas }, ""},
		{"sudo with a password", func(n *NodeConfig) {
			n.Connection.Become, n.Connection.BecomeMethod, n.Connection.BecomePassword = true, BecomeSudo, "pw"
		}, ""},
		{"doas with a password", func(n *NodeConfig) {
			n.Connection.Become, n.Connection.BecomeMethod, n.Connection.BecomePassword = true, BecomeDoas, "pw"
		}, "become_password cannot be used with become_method doas"},
		{"unknown method", func(n *NodeConfig) { n.Connection.Become, n.Connection.BecomeMethod = true, "su" }, "must be sudo or doas"},
		{"method without become", func(n *NodeConfig) { n.Connection.BecomeMethod = BecomeDoas }, "needs become: true"},
		{"all facts", func(n *NodeConfig) {
			n.HostFacts = &HostFacts{Family: FamilyDebian, PackageManager: PackageManagerApt, Init: InitSystemd}
		}, ""},
		{"no package manager", func(n *NodeConfig) { n.HostFacts = &HostFacts{PackageManager: PackageManagerNone} }, ""},
		{"rhel family", func(n *NodeConfig) { n.HostFacts = &HostFacts{Family: FamilyRHEL} }, ""},
		{"unknown family", func(n *NodeConfig) { n.HostFacts = &HostFacts{Family: "arch"} }, "must be debian, rhel or alpine"},
		{"dnf", func(n *NodeConfig) { n.HostFacts = &HostFacts{Family: FamilyRHEL, PackageManager: PackageManagerDnf} }, ""},
		{"apk and openrc", func(n *NodeConfig) {
			n.HostFacts = &HostFacts{Family: FamilyAlpine, PackageManager: PackageManagerApk, Init: InitOpenRC}
		}, ""},
		{"unknown package manager", func(n *NodeConfig) { n.HostFacts = &HostFacts{PackageManager: "pacman"} }, "must be apt, dnf, apk or none"},
		{"unknown init", func(n *NodeConfig) { n.HostFacts = &HostFacts{Init: "runit"} }, "must be systemd or openrc"},
		{"windows", func(n *NodeConfig) { n.Platform, n.HostFacts = "windows", &HostFacts{Family: FamilyDebian} }, "for Linux nodes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validPlan()
			tt.mutate(&p.Inventory.Masters[0])
			err := p.Validate()
			switch {
			case tt.err == "" && err != nil:
				t.Fatalf("valid plan rejected: %v", err)
			case tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)):
				t.Fatalf("got %v, want an error with %q", err, tt.err)
			}
		})
	}
}

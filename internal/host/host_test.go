package host

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// probeOutput is the output of ProbeCommand on a node: an /etc/os-release
// of testdata, process 1, the tools found and the user id.
func probeOutput(t *testing.T, osRelease, machine, pid1 string, tools []string, uid string) string {
	t.Helper()
	data := ""
	if osRelease != "" {
		b, err := os.ReadFile(filepath.Join("testdata", "os-release", osRelease))
		if err != nil {
			t.Fatal(err)
		}
		data = string(b)
	}
	return data + "--\n" + machine + "\n--\n" + pid1 + "\n--\n" + strings.Join(tools, "\n") + "\n--\n" +
		"Disabled\n--\ncgroup2fs\n--\n" + uid + "\n"
}

var (
	debianTools = []string{"apt-get", "sudo", "bash", "iptables", "systemctl", "ss"}
	rhelTools   = []string{"dnf", "yum", "sudo", "bash", "firewall-cmd", "nft", "iptables", "getenforce", "systemctl"}
	alpineTools = []string{"apk", "doas", "iptables", "rc-service"}
)

func TestParse(t *testing.T) {
	tests := []struct {
		name      string
		osRelease string
		machine   string
		pid1      string
		tools     []string
		want      Facts
	}{
		{"debian 12", "debian-12", "x86_64", "systemd", debianTools, Facts{
			OSID: "debian", OSVersion: "12", Family: FamilyDebian, Arch: "amd64", Init: InitSystemd, PID1: "systemd",
			PackageManager: "apt", HasBash: true, Firewalls: []string{"iptables"}}},
		{"ubuntu 24.04 is the debian family", "ubuntu-24.04", "aarch64", "systemd", append(debianTools, "ufw"), Facts{
			OSID: "ubuntu", OSIDLike: []string{"debian"}, OSVersion: "24.04", Family: FamilyDebian, Arch: "arm64",
			Init: InitSystemd, PID1: "systemd", PackageManager: "apt", HasBash: true, Firewalls: []string{"ufw", "iptables"}}},
		{"rhel 9", "rhel-9", "x86_64", "systemd", rhelTools, Facts{
			OSID: "rhel", OSIDLike: []string{"fedora"}, OSVersion: "9.9", Family: FamilyRHEL, Arch: "amd64", Init: InitSystemd,
			PID1: "systemd", PackageManager: "dnf", HasBash: true, Firewalls: []string{"firewalld", "nft", "iptables"}}},
		{"rocky 9", "rocky-9", "x86_64", "systemd", rhelTools, Facts{
			OSID: "rocky", OSIDLike: []string{"rhel", "centos", "fedora"}, OSVersion: "9.9", Family: FamilyRHEL, Arch: "amd64",
			Init: InitSystemd, PID1: "systemd", PackageManager: "dnf", HasBash: true, Firewalls: []string{"firewalld", "nft", "iptables"}}},
		{"alma 9", "alma-9", "x86_64", "systemd", rhelTools, Facts{
			OSID: "almalinux", OSIDLike: []string{"rhel", "centos", "fedora"}, OSVersion: "9.9", Family: FamilyRHEL, Arch: "amd64",
			Init: InitSystemd, PID1: "systemd", PackageManager: "dnf", HasBash: true, Firewalls: []string{"firewalld", "nft", "iptables"}}},
		{"alpine 3.22 with openrc", "alpine-3.22", "x86_64", "init\n/run/openrc", alpineTools, Facts{
			OSID: "alpine", OSVersion: "3.22.8", Family: FamilyAlpine, Arch: "amd64", Init: InitOpenRC, PID1: "init",
			PackageManager: "apk", Firewalls: []string{"iptables"}}},
		{"fedora is counted to rhel", "fedora-41", "x86_64", "systemd", rhelTools, Facts{
			OSID: "fedora", OSVersion: "41", Family: FamilyRHEL, Arch: "amd64", Init: InitSystemd, PID1: "systemd",
			PackageManager: "dnf", HasBash: true, Firewalls: []string{"firewalld", "nft", "iptables"}}},
		{"a debian derivative by ID_LIKE", "mint-22", "x86_64", "systemd", debianTools, Facts{
			OSID: "linuxmint", OSIDLike: []string{"ubuntu", "debian"}, OSVersion: "22", Family: FamilyDebian, Arch: "amd64",
			Init: InitSystemd, PID1: "systemd", PackageManager: "apt", HasBash: true, Firewalls: []string{"iptables"}}},
		{"an unknown ID", "arch", "x86_64", "systemd", []string{"bash", "systemctl"}, Facts{
			OSID: "arch", Arch: "amd64", Init: InitSystemd, PID1: "systemd", HasBash: true}},
		{"systemd in a container: the run directory", "debian-12", "x86_64", "sh\n/run/systemd/system", debianTools, Facts{
			OSID: "debian", OSVersion: "12", Family: FamilyDebian, Arch: "amd64", Init: InitSystemd, PID1: "sh",
			PackageManager: "apt", HasBash: true, Firewalls: []string{"iptables"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(probeOutput(t, tt.osRelease, tt.machine, tt.pid1, tt.tools, "1000"))
			if err != nil {
				t.Fatal(err)
			}
			want := tt.want
			want.Platform, want.Tools, want.SELinux, want.CgroupV2 = "linux", tt.tools, "disabled", true
			if got.String() != want.String() || strings.Join(got.OSIDLike, " ") != strings.Join(want.OSIDLike, " ") ||
				got.PID1 != want.PID1 || got.HasBash != want.HasBash || strings.Join(got.Firewalls, " ") != strings.Join(want.Firewalls, " ") ||
				got.SELinux != want.SELinux || !got.CgroupV2 || got.IsRoot {
				t.Fatalf("Parse = %+v\nwant    %+v", *got, want)
			}
		})
	}
}

func TestParseRootAndErrors(t *testing.T) {
	f, err := Parse(probeOutput(t, "debian-12", "x86_64", "systemd", debianTools, "0"))
	if err != nil || !f.IsRoot {
		t.Fatalf("uid 0: %+v, %v", f, err)
	}
	if _, err := Parse("ID=debian\n"); err == nil {
		t.Fatal("a truncated output is accepted")
	}
	// No /etc/os-release at all.
	f, err = Parse(probeOutput(t, "", "x86_64", "systemd", debianTools, "1000"))
	if err != nil || f.Family != "" || f.OSID != "" {
		t.Fatalf("no os-release: %+v, %v", f, err)
	}
}

func TestCheck(t *testing.T) {
	for _, sample := range []string{"rhel-9", "rocky-9", "alma-9", "alma-10", "fedora-41"} {
		f, _ := Parse(probeOutput(t, sample, "x86_64", "systemd", rhelTools, "1000"))
		if err := f.Check(); err != nil {
			t.Errorf("%s: %v", sample, err)
		}
	}
	for _, sample := range []string{"debian-11", "debian-12", "ubuntu-20.04", "ubuntu-24.04"} {
		f, _ := Parse(probeOutput(t, sample, "x86_64", "systemd", debianTools, "1000"))
		if err := f.Check(); err != nil {
			t.Errorf("%s: %v", sample, err)
		}
	}
	for _, sample := range []string{"alpine-3.22", "alpine-3.24", "alpine-edge"} {
		f, _ := Parse(probeOutput(t, sample, "x86_64", "init\n/run/openrc", alpineTools, "1000"))
		if err := f.Check(); err != nil {
			t.Errorf("%s: %v", sample, err)
		}
	}
	if err := Windows("amd64").Check(); err != nil {
		t.Fatalf("windows: %v", err)
	}
	tests := []struct {
		name string
		out  string
		want []string
	}{
		{"unknown family", probeOutput(t, "arch", "x86_64", "systemd", []string{"bash", "systemctl"}, "1000"),
			[]string{"not of a family the deployer knows", "no package manager", "ID=arch", "process 1 systemd", "tools: bash systemctl"}},
		{"no init", probeOutput(t, "debian-12", "x86_64", "runit", debianTools, "1000"),
			[]string{"neither systemd nor OpenRC", "process 1 runit", "ID=debian", "VERSION_ID=12"}},
		{"no os-release", probeOutput(t, "", "x86_64", "systemd", nil, "1000"),
			[]string{"ID=none (no /etc/os-release)", "tools: none"}},
		{"rocky 8", probeOutput(t, "rocky-8", "x86_64", "systemd", rhelTools, "1000"),
			[]string{"rocky 8.10", "from version 9", "glibc 2.34", "glibc 2.28", "VERSION_ID=8.10"}},
		{"rhel 8", probeOutput(t, "rhel-8", "x86_64", "systemd", rhelTools, "1000"),
			[]string{"rhel 8.10", "from version 9"}},
		{"debian 10", probeOutput(t, "debian-10", "x86_64", "systemd", debianTools, "1000"),
			[]string{"debian 10", "Debian is supported from version 11", "glibc 2.31", "deb822", "VERSION_ID=10"}},
		{"ubuntu 18.04", probeOutput(t, "ubuntu-18.04", "x86_64", "systemd", debianTools, "1000"),
			[]string{"ubuntu 18.04", "Ubuntu is supported from version 20.04", "glibc 2.31"}},
		{"alpine 3.21", probeOutput(t, "alpine-3.21", "x86_64", "init\n/run/openrc", alpineTools, "1000"),
			[]string{"alpine 3.21.10", "Alpine is supported from version 3.22", "no longer supported by Alpine"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := Parse(tt.out)
			if err != nil {
				t.Fatal(err)
			}
			err = f.Check()
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("the node is accepted or another error: %v", err)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("the error has no %q: %v", w, err)
				}
			}
		})
	}
}

// A version is older than the minimum of its ID, else of its family; a
// version that is not a number before a dot is not older. The install
// script of a scaled machine is held to the same results (engine
// TestInstallScriptOSVersion).
func TestVersionProblem(t *testing.T) {
	tests := []struct {
		id      string
		family  Family
		version string
		older   bool
	}{
		{"debian", FamilyDebian, "10", true},
		{"debian", FamilyDebian, "11", false},
		{"debian", FamilyDebian, "", false},
		{"debian", FamilyDebian, "011", false},
		{"debian", FamilyDebian, "+9", false},
		{"debian", FamilyDebian, "1234567890", false},
		{"ubuntu", FamilyDebian, "19.10", true},
		{"ubuntu", FamilyDebian, "20", true},
		{"ubuntu", FamilyDebian, "20.04", false},
		{"ubuntu", FamilyDebian, "20.x", false},
		{"linuxmint", FamilyDebian, "19", false},
		{"alpine", FamilyAlpine, "3.21.10", true},
		{"alpine", FamilyAlpine, "3.22.0", false},
		{"alpine", FamilyAlpine, "3.25.0_alpha20260805", false},
		{"rocky", FamilyRHEL, "8.10", true},
		{"centos", FamilyRHEL, "7", true},
		{"rhel", FamilyRHEL, "9.0", false},
		{"fedora", FamilyRHEL, "41", false},
	}
	for _, tt := range tests {
		t.Run(tt.id+" "+tt.version, func(t *testing.T) {
			got := VersionProblem(tt.id, tt.family, tt.version)
			if (got != "") != tt.older {
				t.Fatalf("VersionProblem = %q, want older %t", got, tt.older)
			}
			if tt.older && !strings.HasPrefix(got, "it runs "+tt.id+" "+tt.version+", but ") {
				t.Fatalf("VersionProblem = %q", got)
			}
		})
	}
	if got := VersionProblem("rocky", FamilyRHEL, "8.10"); got != "it runs rocky 8.10, but RHEL (with its rebuilds) is supported from version 9: "+
		"the Norca binary needs glibc 2.34, which RHEL 9 has; RHEL 8 has glibc 2.28" {
		t.Fatalf("rocky 8.10: %q", got)
	}
}

// SELinux: enforcing is a warning of the node, and enforcing and
// permissive both label files.
func TestSELinux(t *testing.T) {
	tests := []struct {
		mode    string
		active  bool
		warning bool
	}{
		{mode: "Enforcing", active: true, warning: true},
		{mode: "Permissive", active: true},
		{mode: "Disabled"},
		{mode: ""},
	}
	for _, tt := range tests {
		t.Run("mode "+tt.mode, func(t *testing.T) {
			out := strings.Replace(probeOutput(t, "rocky-9", "x86_64", "systemd", rhelTools, "1000"), "--\nDisabled\n--", "--\n"+tt.mode+"\n--", 1)
			f, err := Parse(out)
			if err != nil {
				t.Fatal(err)
			}
			if f.SELinuxActive() != tt.active {
				t.Errorf("SELinuxActive() = %t, want %t", f.SELinuxActive(), tt.active)
			}
			w := f.Warnings()
			if (len(w) == 1) != tt.warning || (tt.warning && !strings.Contains(w[0], "Norca under SELinux enforcing is not verified yet")) {
				t.Errorf("Warnings() = %q", w)
			}
		})
	}
}

func TestOverride(t *testing.T) {
	f, _ := Parse(probeOutput(t, "arch", "x86_64", "systemd", []string{"bash", "systemctl"}, "1000"))
	lines := f.Override(&config.HostFacts{Family: "debian", PackageManager: "none"})
	if f.Family != FamilyDebian || f.PackageManager != "none" || f.Init != InitSystemd {
		t.Fatalf("facts after the override: %+v", f)
	}
	want := []string{"family debian (from the plan; detected nothing)", "package manager none (from the plan; detected nothing)"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("override lines %q", lines)
	}
	if err := f.Check(); err != nil {
		t.Fatalf("an overridden node is refused: %v", err)
	}
	if strings.Join(f.Overrides, ",") != "family,package_manager" {
		t.Fatalf("overrides %v", f.Overrides)
	}
}

func TestChooseEscalation(t *testing.T) {
	facts := func(tools []string, uid string) *Facts {
		f, err := Parse(probeOutput(t, "debian-12", "x86_64", "systemd", tools, uid))
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	tests := []struct {
		name   string
		tools  []string
		uid    string
		become bool
		method string
		want   string
		err    string
	}{
		{"no become", []string{"apt-get"}, "1000", false, "", EscalationNone, ""},
		{"root", []string{"apt-get"}, "0", true, "", EscalationNone, ""},
		{"sudo found", []string{"sudo", "doas"}, "1000", true, "", EscalationSudo, ""},
		{"doas when no sudo", []string{"doas"}, "1000", true, "", EscalationDoas, ""},
		{"doas of the plan", []string{"sudo", "doas"}, "1000", true, "doas", EscalationDoas, ""},
		{"method of the plan missing", []string{"sudo"}, "1000", true, "doas", "", "doas is not installed"},
		{"neither", []string{"apt-get"}, "1000", true, "", "", "neither sudo nor doas"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := facts(tt.tools, tt.uid)
			err := f.ChooseEscalation(tt.become, tt.method)
			if tt.err != "" {
				if !errors.Is(err, ErrNoEscalation) || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil || f.Escalation != tt.want {
				t.Fatalf("escalation %q, %v; want %q", f.Escalation, err, tt.want)
			}
		})
	}
}

// The probe runs as the SSH user through the connector; a Windows node is
// not probed.
func TestProbe(t *testing.T) {
	conn := connector.NewFakeConnector()
	conn.Responses[ProbeCommand] = probeOutput(t, "ubuntu-24.04", "x86_64", "systemd", debianTools, "1000")
	f, lines, err := Probe(conn, &config.NodeConfig{Platform: "linux", HostFacts: &config.HostFacts{Init: "systemd"}})
	if err != nil {
		t.Fatal(err)
	}
	if f.OSID != "ubuntu" || f.Family != FamilyDebian || len(lines) != 1 || lines[0] != "init systemd (from the plan; detected systemd)" {
		t.Fatalf("facts %+v, lines %q", f, lines)
	}
	if got := f.String(); got != "ubuntu 24.04 (debian family), amd64, systemd, apt" {
		t.Fatalf("String = %q", got)
	}

	win := connector.NewFakeConnector()
	f, _, err = Probe(win, &config.NodeConfig{Platform: "windows", Arch: "amd64"})
	if err != nil || f.Family != FamilyWindows || len(win.Commands) != 0 {
		t.Fatalf("windows: %+v, %v, commands %v", f, err, win.Commands)
	}

	failing := connector.NewFakeConnector()
	failing.Failures["cat /etc/os-release"] = os.ErrPermission
	if _, _, err := Probe(failing, &config.NodeConfig{Platform: "linux"}); err == nil || !strings.Contains(err.Error(), "probe") {
		t.Fatalf("a failed probe: %v", err)
	}
}

// Whatever a node prints, the parser returns facts or an error; it does not
// panic.
func FuzzParse(f *testing.F) {
	f.Add(debianSample)
	f.Add("--\n--\n--\n--\n--\n--\n")
	f.Add("ID=\"unterminated\n--\nx\n--\n\n--\n--\n--\n--\nabc")
	f.Fuzz(func(t *testing.T, out string) {
		facts, err := Parse(out)
		if err == nil && facts == nil {
			t.Fatal("no facts and no error")
		}
		if err == nil {
			_ = facts.Check()
			_ = facts.String()
		}
	})
}

const debianSample = "ID=debian\nVERSION_ID=\"12\"\n--\nx86_64\n--\nsystemd\n--\napt-get\nsudo\n--\n--\ncgroup2fs\n--\n0\n"

// Package host finds out what a node is: the family of its operating system
// and the tools the deployer drives there (package manager, init system,
// escalation, shell, firewall).
//
// One probe command collects the facts after the deployer connected and
// before it changes anything. It runs without escalation, in POSIX sh that
// dash, bash and busybox ash all read. The plan may override what the probe
// found (host_facts of a node); an override is taken as it is.
package host

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// Family is the family of an operating system: the deployer drives the
// members of a family with the same tools.
type Family string

// The families.
const (
	FamilyDebian  Family = config.FamilyDebian // Debian, Ubuntu
	FamilyRHEL    Family = config.FamilyRHEL   // RHEL, Rocky Linux, AlmaLinux (Fedora: detected, not tested)
	FamilyAlpine  Family = config.FamilyAlpine
	FamilyWindows Family = "windows"
)

// Init is the init system of a node, which runs the agent as a service.
type Init string

// The init systems.
const (
	InitSystemd Init = config.InitSystemd
	InitOpenRC  Init = config.InitOpenRC
	InitWindows Init = "windows"
)

// Escalation methods of a node: how the deployer gets root.
const (
	EscalationSudo = "sudo"
	EscalationDoas = "doas"
	EscalationNone = "none" // root, no become, or Windows
)

// ErrUnsupported is the error of a node the deployer cannot drive; the
// error that wraps it lists what the probe found.
var ErrUnsupported = errors.New("the node is not supported")

// ErrNoEscalation is the error of a node where become is on but no
// escalation method can be used.
var ErrNoEscalation = errors.New("no escalation method")

// Facts are what the deployer knows about a node.
type Facts struct {
	Platform       string   `json:"platform"`             // linux, windows (from the plan)
	OSID           string   `json:"os_id,omitempty"`      // ID of /etc/os-release: debian, ubuntu, rhel, rocky, almalinux, alpine
	OSIDLike       []string `json:"os_id_like,omitempty"` // ID_LIKE
	OSVersion      string   `json:"os_version,omitempty"` // VERSION_ID
	Family         Family   `json:"family"`               // debian, rhel, alpine, windows; empty: unknown
	Arch           string   `json:"arch,omitempty"`       // amd64, arm64, arm, 386 (GOARCH names); empty: unknown
	Init           Init     `json:"init"`                 // systemd, openrc, windows; empty: unknown
	PID1           string   `json:"pid1,omitempty"`       // the name of process 1, for the messages
	PackageManager string   `json:"package_manager"`      // apt, dnf, apk, none; empty: none found
	Escalation     string   `json:"escalation,omitempty"` // sudo, doas, none
	HasBash        bool     `json:"has_bash,omitempty"`   // bash is installed
	Firewalls      []string `json:"firewalls,omitempty"`  // ufw, firewalld, nft, iptables: the tools found
	SELinux        string   `json:"selinux,omitempty"`    // enforcing, permissive, disabled, ""
	CgroupV2       bool     `json:"cgroup_v2,omitempty"`  // /sys/fs/cgroup is cgroup2
	IsRoot         bool     `json:"-"`                    // the SSH user is root (uid 0)
	Tools          []string `json:"tools,omitempty"`      // the tools of the probe that were found
	Overrides      []string `json:"overrides,omitempty"`  // the keys of host_facts the plan set
}

// Windows returns the facts of a Windows node: no probe beyond the
// platform of the plan.
func Windows(arch string) *Facts {
	return &Facts{Platform: "windows", Family: FamilyWindows, Arch: arch, Init: InitWindows,
		PackageManager: config.PackageManagerNone, Escalation: EscalationNone}
}

// Has reports whether the probe found a tool.
func (f *Facts) Has(tool string) bool { return slices.Contains(f.Tools, tool) }

// FamilyOf returns the family of an /etc/os-release: its ID first, then the
// IDs of ID_LIKE in their order. Ubuntu is the Debian family (with its own
// OSID); Fedora is counted to the RHEL family.
func FamilyOf(id string, like []string) Family {
	for _, v := range append([]string{id}, like...) {
		switch v {
		case "debian", "ubuntu":
			return FamilyDebian
		case "rhel", "rocky", "almalinux", "centos", "fedora":
			return FamilyRHEL
		case "alpine":
			return FamilyAlpine
		}
	}
	return ""
}

// packageManagerOf returns the package manager of a family when the probe
// found its tool, else the first one found.
func packageManagerOf(fam Family, tools []string) string {
	want := map[Family]string{FamilyDebian: "apt-get", FamilyRHEL: "dnf", FamilyAlpine: "apk"}[fam]
	names := map[string]string{"apt-get": config.PackageManagerApt, "dnf": config.PackageManagerDnf, "apk": config.PackageManagerApk}
	if want != "" && slices.Contains(tools, want) {
		return names[want]
	}
	for _, t := range []string{"apt-get", "dnf", "apk"} {
		if slices.Contains(tools, t) {
			return names[t]
		}
	}
	return ""
}

// Override sets the facts the plan names (host_facts of the node) and
// returns a line for the log per key.
func (f *Facts) Override(hf *config.HostFacts) []string {
	if hf == nil {
		return nil
	}
	var lines []string
	set := func(key, name, value string, detected string, apply func()) {
		if value == "" {
			return
		}
		if detected == "" {
			detected = "nothing"
		}
		lines = append(lines, fmt.Sprintf("%s %s (from the plan; detected %s)", name, value, detected))
		f.Overrides = append(f.Overrides, key)
		apply()
	}
	set("family", "family", hf.Family, string(f.Family), func() { f.Family = Family(hf.Family) })
	set("package_manager", "package manager", hf.PackageManager, f.PackageManager, func() { f.PackageManager = hf.PackageManager })
	set("init", "init", hf.Init, string(f.Init), func() { f.Init = Init(hf.Init) })
	return lines
}

// Check returns the error of a node the deployer cannot drive: a family
// outside Debian, RHEL and Alpine, no package manager, an init system that
// is neither systemd nor OpenRC, a RHEL older than MinRHELMajor, or a
// Debian, Ubuntu or Alpine older than its minimum (minimums). It lists
// what the probe found.
func (f *Facts) Check() error {
	if f.Platform == "windows" {
		return nil
	}
	var problems []string
	if f.Family == "" {
		problems = append(problems, "its operating system is not of a family the deployer knows (Debian, Ubuntu, RHEL, Rocky Linux, AlmaLinux, Alpine)")
	}
	if f.PackageManager == "" {
		problems = append(problems, "it has no package manager the deployer knows (apt-get, dnf, apk); set host_facts.package_manager: none for a system without one")
	}
	if f.Init == "" {
		problems = append(problems, "its init system is neither systemd nor OpenRC")
	}
	if major, ok := majorVersion(f.OSVersion); ok && f.Family == FamilyRHEL && major < MinRHELMajor {
		problems = append(problems, fmt.Sprintf("it runs %s %s, but RHEL and its rebuilds are supported from version %d: "+
			"the Norca binary needs glibc 2.34, and version %d has glibc 2.28 or older", f.OSID, f.OSVersion, MinRHELMajor, major))
	}
	if p := f.tooOld(); p != "" {
		problems = append(problems, p)
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s (found: %s)", ErrUnsupported, strings.Join(problems, "; "), f.found())
}

// SELinux modes of getenforce, lower case.
const (
	SELinuxEnforcing  = "enforcing"
	SELinuxPermissive = "permissive"
	SELinuxDisabled   = "disabled"
)

// SELinuxActive reports whether SELinux labels the files of the node
// (enforcing or permissive): the deployer then restores the labels of the
// files it writes.
func (f *Facts) SELinuxActive() bool {
	return f.SELinux == SELinuxEnforcing || f.SELinux == SELinuxPermissive
}

// Warnings returns what the apply should tell about a node it goes on
// with: SELinux in enforcing mode, under which Norca is not verified yet.
func (f *Facts) Warnings() []string {
	if f.SELinux != SELinuxEnforcing {
		return nil
	}
	return []string{"SELinux is enforcing: Norca under SELinux enforcing is not verified yet; the deployer restores " +
		"the SELinux labels of the files it writes, but the agent and its drivers may be denied (set SELinux to " +
		"permissive if the agent fails)"}
}

// MinRHELMajor is the first major version of RHEL (and of its rebuilds)
// the deployer installs on: the Linux release of Norca is built against
// glibc 2.34, which RHEL 9 has and RHEL 8 (glibc 2.28) has not.
const MinRHELMajor = 9

// minimum is the first version of a distribution the deployer installs on,
// and why the versions before it are refused.
type minimum struct {
	name         string // the distribution in the message
	major, minor int
	version      string // the first version, as the distribution writes it
	reason       string
}

// The first versions of Debian, Ubuntu and Alpine (RHEL: MinRHELMajor).
// Debian and Ubuntu: the Linux release of Norca needs glibc 2.31, and the
// deployer writes deb822 sources with Signed-By. Alpine: the musl build of
// Norca runs on older branches too, but the deployer installs on the
// branches Alpine supports (3.21 reached its end of support on 2026-11-01).
var minimums = map[string]minimum{
	"debian": {name: "Debian", major: 11, version: "11",
		reason: "the Norca binary needs glibc 2.31 and the deployer writes deb822 sources with Signed-By; Debian 10 has glibc 2.28"},
	"ubuntu": {name: "Ubuntu", major: 20, minor: 4, version: "20.04",
		reason: "the Norca binary needs glibc 2.31 and the deployer writes deb822 sources with Signed-By; Ubuntu 18.04 has glibc 2.27"},
	"alpine": {name: "Alpine", major: 3, minor: 22, version: "3.22",
		reason: "older branches are no longer supported by Alpine (end of support), and the deployer installs on the supported ones"},
}

// tooOld returns the problem of a Debian, Ubuntu or Alpine older than its
// minimum; "" when it is not older or its version is not known (Debian
// testing has no VERSION_ID).
func (f *Facts) tooOld() string {
	m, ok := minimums[f.OSID]
	if !ok {
		return ""
	}
	major, minor, ok := majorMinor(f.OSVersion)
	if !ok || major > m.major || (major == m.major && minor >= m.minor) {
		return ""
	}
	return fmt.Sprintf("it runs %s %s, but %s is supported from version %s: %s", f.OSID, f.OSVersion, m.name, m.version, m.reason)
}

// majorVersion reads the major version of a VERSION_ID ("8.10" is 8).
func majorVersion(v string) (int, bool) {
	major, _, _ := strings.Cut(v, ".")
	n, err := strconv.Atoi(major)
	return n, err == nil
}

// majorMinor reads the major and minor version of a VERSION_ID: "20.04" is
// 20 and 4, "3.25.0_alpha20260805" (Alpine edge) is 3 and 25, "11" is 11
// and 0.
func majorMinor(v string) (int, int, bool) {
	major, ok := majorVersion(v)
	if !ok {
		return 0, 0, false
	}
	_, rest, found := strings.Cut(v, ".")
	if !found {
		return major, 0, true
	}
	end := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
	if end >= 0 {
		rest = rest[:end]
	}
	minor, err := strconv.Atoi(rest)
	return major, minor, err == nil
}

// found describes what the probe saw, for an error.
func (f *Facts) found() string {
	id := f.OSID
	if id == "" {
		id = "none (no /etc/os-release)"
	}
	parts := []string{"ID=" + id}
	if len(f.OSIDLike) > 0 {
		parts = append(parts, "ID_LIKE="+strings.Join(f.OSIDLike, " "))
	}
	if f.OSVersion != "" {
		parts = append(parts, "VERSION_ID="+f.OSVersion)
	}
	pid1 := f.PID1
	if pid1 == "" {
		pid1 = "unknown"
	}
	parts = append(parts, "process 1 "+pid1)
	tools := "none"
	if len(f.Tools) > 0 {
		tools = strings.Join(f.Tools, " ")
	}
	return strings.Join(parts, ", ") + ", tools: " + tools
}

// String describes the facts in one line for the log.
func (f *Facts) String() string {
	if f.Platform == "windows" {
		return "windows " + f.Arch
	}
	os := f.OSID
	if f.OSVersion != "" {
		os += " " + f.OSVersion
	}
	if os == "" {
		os = "unknown"
	}
	parts := []string{fmt.Sprintf("%s (%s family)", os, f.Family)}
	for _, p := range []string{f.Arch, string(f.Init), f.PackageManager} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if f.Escalation != "" && f.Escalation != EscalationNone {
		parts = append(parts, f.Escalation)
	}
	return strings.Join(parts, ", ")
}

// ChooseEscalation decides how commands get root on a node: none when the
// plan has no become or the SSH user is root; else the method of the plan
// (become_method), which must be installed; else sudo when the node has it,
// else doas.
func (f *Facts) ChooseEscalation(become bool, method string) error {
	switch {
	case f.Platform == "windows" || !become || f.IsRoot:
		f.Escalation = EscalationNone
	case method != "":
		if !f.Has(method) {
			return fmt.Errorf("%w: connection.become_method is %s, but %s is not installed on the node", ErrNoEscalation, method, method)
		}
		f.Escalation = method
	case f.Has(EscalationSudo):
		f.Escalation = EscalationSudo
	case f.Has(EscalationDoas):
		f.Escalation = EscalationDoas
	default:
		return fmt.Errorf("%w: become is on and the SSH user is not root, but neither sudo nor doas is installed on the node", ErrNoEscalation)
	}
	return nil
}

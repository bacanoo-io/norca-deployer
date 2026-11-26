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
// is neither systemd nor OpenRC, or a version older than its minimum
// (Minimums). It lists what the probe found.
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
	if p := VersionProblem(f.OSID, f.Family, f.OSVersion); p != "" {
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

// Minimum is the first version of a distribution the deployer installs on,
// and why the versions before it are refused. The check of a node over SSH
// (Check) and the install script of a scaled machine take the same
// minimums.
type Minimum struct {
	// ID is the ID of /etc/os-release the minimum is of. A minimum
	// without an ID holds for the members of Family that have no minimum
	// of their own.
	ID     string
	Family Family
	// Name is the distribution in the message; Version is the first
	// version, as the distribution writes it.
	Name, Version string
	Major, Minor  int
	Reason        string
}

// minimums are the first versions of Debian, Ubuntu, Alpine and the RHEL
// family. Debian and Ubuntu: the Linux release of Norca needs glibc 2.31,
// and the deployer writes deb822 sources with Signed-By. RHEL and its
// rebuilds: the release needs glibc 2.34, which RHEL 9 has and RHEL 8
// (glibc 2.28) has not. Alpine: the musl build of Norca runs on older
// branches too, but the deployer installs on the branches Alpine supports
// (3.21 reached its end of support on 2026-11-01).
var minimums = []Minimum{
	{ID: "debian", Family: FamilyDebian, Name: "Debian", Version: "11", Major: 11,
		Reason: "the Norca binary needs glibc 2.31 and the deployer writes deb822 sources with Signed-By; Debian 10 has glibc 2.28"},
	{ID: "ubuntu", Family: FamilyDebian, Name: "Ubuntu", Version: "20.04", Major: 20, Minor: 4,
		Reason: "the Norca binary needs glibc 2.31 and the deployer writes deb822 sources with Signed-By; Ubuntu 18.04 has glibc 2.27"},
	{ID: "alpine", Family: FamilyAlpine, Name: "Alpine", Version: "3.22", Major: 3, Minor: 22,
		Reason: "older branches are no longer supported by Alpine (end of support), and the deployer installs on the supported ones"},
	{Family: FamilyRHEL, Name: "RHEL (with its rebuilds)", Version: "9", Major: 9,
		Reason: "the Norca binary needs glibc 2.34, which RHEL 9 has; RHEL 8 has glibc 2.28"},
}

// Minimums returns the minimums of the distributions: those of an ID
// first, then those of a family.
func Minimums() []Minimum { return slices.Clone(minimums) }

// VersionProblem returns why a version of an operating system is older than
// the deployer installs on: the minimum of its ID, else the one of its
// family. It is "" when the version is not older or not known (Debian
// testing has no VERSION_ID).
func VersionProblem(id string, family Family, version string) string {
	m, ok := minimumOf(id, family)
	if !ok || !m.olderThan(version) {
		return ""
	}
	return fmt.Sprintf("it runs %s %s, but %s is supported from version %s: %s", id, version, m.Name, m.Version, m.Reason)
}

func minimumOf(id string, family Family) (Minimum, bool) {
	for _, m := range minimums {
		if m.ID != "" && m.ID == id {
			return m, true
		}
	}
	for _, m := range minimums {
		if m.ID == "" && m.Family == family {
			return m, true
		}
	}
	return Minimum{}, false
}

// olderThan reports whether a VERSION_ID is older than the minimum. The
// major version is the number before the first dot, the minor one the
// digits after it ("20.04" is 20 and 4, "3.25.0_alpha20260805" of Alpine
// edge is 3 and 25, "11" is 11 and 0). A version that is not of this form
// is not older; the minor version counts only when the major one is the
// minimum's. The install script of a scaled machine compares the same way
// in sh (os_too_old).
func (m Minimum) olderThan(version string) bool {
	majorPart, rest, hasMinor := strings.Cut(version, ".")
	major, ok := versionNumber(majorPart)
	if !ok {
		return false
	}
	if major != m.Major {
		return major < m.Major
	}
	if !hasMinor {
		return m.Minor > 0
	}
	if end := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' }); end >= 0 {
		rest = rest[:end]
	}
	minor, ok := versionNumber(rest)
	return ok && minor < m.Minor
}

// versionNumber reads 1 to 9 decimal digits: a longer number is not a
// version (and would overflow the test of a shell).
func versionNumber(s string) (int, bool) {
	if s == "" || len(s) > 9 || strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
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

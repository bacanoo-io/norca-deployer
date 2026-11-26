package host

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// ProbeCommand collects the facts of a Linux node in one round trip. It
// needs no root and is POSIX sh. Its sections are separated by lines of
// "--": /etc/os-release, the machine, process 1 and the run directories of
// systemd and OpenRC, the tools found, SELinux, the file system of the
// cgroups and the user id. The file system of the cgroups is the type of the
// last mount on /sys/fs/cgroup in /proc/mounts (cgroup2: v2; tmpfs: the
// hierarchies of v1): the stat of busybox names no cgroup2 (UNKNOWN).
const ProbeCommand = `cat /etc/os-release 2>/dev/null; echo "--"
uname -m; echo "--"
cat /proc/1/comm 2>/dev/null; [ -d /run/systemd/system ] && echo /run/systemd/system; [ -d /run/openrc ] && echo /run/openrc; echo "--"
for t in apt-get dnf yum apk sudo doas bash ufw firewall-cmd nft iptables getenforce rc-service systemctl ss; do
  command -v "$t" >/dev/null 2>&1 && echo "$t"
done; echo "--"
getenforce 2>/dev/null; echo "--"
awk '$2 == "/sys/fs/cgroup" { t = $3 } END { print t }' /proc/mounts 2>/dev/null; echo "--"
id -u`

// Probe collects the facts of a node: the probe command as the SSH user
// for Linux, the plan for Windows. The host_facts of the node override what
// was found; lines has the override lines for the log.
func Probe(conn connector.Connector, node *config.NodeConfig) (f *Facts, lines []string, err error) {
	if strings.EqualFold(node.Platform, "windows") {
		return Windows(node.Arch), nil, nil
	}
	out, err := connector.ExecuteAsUser(conn, ProbeCommand)
	if err != nil {
		return nil, nil, fmt.Errorf("the probe of the node failed: %w", err)
	}
	f, err = Parse(out)
	if err != nil {
		return nil, nil, err
	}
	lines = f.Override(node.HostFacts)
	return f, lines, nil
}

// Parse reads the output of ProbeCommand.
func Parse(out string) (*Facts, error) {
	sections := splitSections(out)
	if len(sections) != 7 {
		return nil, fmt.Errorf("unexpected output of the probe of the node (%d sections, want 7): %q", len(sections), out)
	}
	f := &Facts{Platform: "linux"}
	osr := parseOSRelease(sections[0])
	f.OSID = strings.ToLower(osr["ID"])
	f.OSIDLike = strings.Fields(strings.ToLower(osr["ID_LIKE"]))
	f.OSVersion = osr["VERSION_ID"]
	f.Family = FamilyOf(f.OSID, f.OSIDLike)
	f.Arch = goArch(strings.TrimSpace(sections[1]))

	f.Tools = lines(sections[3])
	pid1 := lines(sections[2])
	if len(pid1) > 0 && !strings.HasPrefix(pid1[0], "/") {
		f.PID1 = pid1[0]
	}
	switch {
	case f.PID1 == "systemd" || slices.Contains(pid1, "/run/systemd/system"):
		f.Init = InitSystemd
	case f.PID1 == "openrc-init" || slices.Contains(pid1, "/run/openrc"):
		f.Init = InitOpenRC
	}
	f.PackageManager = packageManagerOf(f.Family, f.Tools)
	f.HasBash = f.Has("bash")
	for _, fw := range []struct{ tool, name string }{{"ufw", "ufw"}, {"firewall-cmd", "firewalld"}, {"nft", "nft"}, {"iptables", "iptables"}} {
		if f.Has(fw.tool) {
			f.Firewalls = append(f.Firewalls, fw.name)
		}
	}
	f.SELinux = strings.ToLower(strings.TrimSpace(sections[4]))
	f.CgroupV2 = strings.TrimSpace(sections[5]) == "cgroup2"
	uid, err := strconv.Atoi(strings.TrimSpace(sections[6]))
	if err != nil {
		return nil, fmt.Errorf("unexpected user id in the probe of the node: %q", sections[6])
	}
	f.IsRoot = uid == 0
	return f, nil
}

// splitSections splits the output at the lines "--".
func splitSections(out string) []string {
	var sections []string
	var cur []string
	for line := range strings.SplitSeq(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		if line == "--" {
			sections = append(sections, strings.Join(cur, "\n"))
			cur = nil
			continue
		}
		cur = append(cur, line)
	}
	return append(sections, strings.Join(cur, "\n"))
}

// lines returns the non-empty lines of a section.
func lines(s string) []string {
	var res []string
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			res = append(res, l)
		}
	}
	return res
}

// parseOSRelease reads the KEY=value lines of /etc/os-release; values may
// be quoted with " or ' (os-release(5)).
func parseOSRelease(s string) map[string]string {
	res := map[string]string{}
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		res[k] = unquoteOSRelease(v)
	}
	return res
}

func unquoteOSRelease(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		inner := v[1 : len(v)-1]
		if v[0] == '\'' {
			return inner
		}
		var b strings.Builder
		for i := 0; i < len(inner); i++ {
			if inner[i] == '\\' && i+1 < len(inner) && strings.IndexByte("\"\\$`", inner[i+1]) >= 0 {
				i++
			}
			b.WriteByte(inner[i])
		}
		return b.String()
	}
	return v
}

// goArch maps the machine name of uname -m to the GOARCH name.
func goArch(machine string) string {
	switch machine {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	case "armv7l", "armv6l", "armhf", "arm":
		return "arm"
	case "i386", "i486", "i586", "i686":
		return "386"
	}
	return ""
}

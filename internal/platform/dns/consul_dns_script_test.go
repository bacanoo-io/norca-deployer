package dns

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// These tests run consul_dns.sh for real, through Apply and the fake
// connector, against a fixture root (NCD_ROOT) with stub systemctl and ip
// commands: under dash and busybox sh as the SSH path runs it, and under
// bash with set -euo pipefail as the install script of a scaled machine
// calls it. A shell that is not installed is left out (in the test
// container: NORCA_TEST_BUSYBOX names a static busybox).

// shell is one way of running the script.
type shell struct {
	name string
	argv []string
	// path is the PATH after the stubs.
	path string
	// machine runs the functions as the machine script does.
	machine bool
	// applets are the commands the shell runs as its own applet before a
	// command of PATH (a busybox built with FEATURE_PREFER_APPLETS, as
	// those of Debian and Ubuntu are): a stub of such a name is called
	// through a shell function, which comes before an applet.
	applets []string
}

func (s shell) hasPython() bool {
	for dir := range strings.SplitSeq(s.path, ":") {
		if _, err := os.Stat(filepath.Join(dir, "python3")); err == nil {
			return true
		}
	}
	return false
}

const systemPath = "/usr/local/bin:/usr/bin:/bin:/usr/local/sbin:/usr/sbin:/sbin"

// busyboxApplets are the commands the script and the stubs use.
var busyboxApplets = []string{"sh", "awk", "grep", "sha256sum", "cp", "mv", "rm", "mkdir", "chmod", "mktemp", "dirname", "tr", "cat", "printf", "echo", "["}

func shells(t *testing.T) []shell {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the script runs on Linux")
	}
	var res []shell
	if p, err := exec.LookPath("dash"); err == nil {
		res = append(res, shell{name: "dash", argv: []string{p}, path: systemPath})
		if dir := minimalPath(t); dir != "" {
			res = append(res, shell{name: "dash-minimal", argv: []string{p}, path: dir})
		}
	}
	bb := os.Getenv("NORCA_TEST_BUSYBOX")
	if bb == "" {
		bb, _ = exec.LookPath("busybox")
	}
	if bb != "" {
		// Only busybox applets on the path, as on a busybox system.
		dir := t.TempDir()
		for _, applet := range busyboxApplets {
			if err := os.Symlink(bb, filepath.Join(dir, applet)); err != nil {
				t.Fatal(err)
			}
		}
		list, err := exec.Command(bb, "--list").Output()
		if err != nil {
			t.Fatalf("%s --list: %v", bb, err)
		}
		res = append(res, shell{name: "busybox", argv: []string{bb, "sh"}, path: dir, applets: strings.Fields(string(list))})
	}
	if p, err := exec.LookPath("bash"); err == nil {
		res = append(res, shell{name: "bash-machine", argv: []string{p}, path: systemPath, machine: true})
	}
	if len(res) == 0 {
		t.Skip("neither dash, busybox nor bash is installed")
	}
	return res
}

// minimalTools are the commands of a minimal system that the script and
// the stubs may use: those of coreutils, grep and gawk (or busybox). A
// minimal RHEL image has no diffutils, so no cmp.
var minimalTools = []string{"sh", "awk", "grep", "sha256sum", "cp", "mv", "rm", "mkdir", "chmod", "mktemp", "dirname", "tr", "cat", "printf", "echo", "["}

// minimalPath returns a directory with links to the minimal tools of this
// system, for a PATH of nothing else; "" when one is missing.
func minimalPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range minimalTools {
		p, err := exec.LookPath(tool)
		if err != nil {
			return ""
		}
		if err := os.Symlink(p, filepath.Join(dir, tool)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const systemctlStub = `#!/bin/sh
s=$NCD_TEST_STATE
case $1 in
--version)
  [ -f "$s/version" ] || exit 1
  cat "$s/version"
  ;;
is-active) [ -f "$s/active-$3" ] ;;
is-enabled) [ -f "$s/enabled-$3" ] ;;
restart)
  echo "$2" >>"$s/restarts"
  if [ -f "$s/fail-restart-$2" ]; then
    echo "Job for $2.service failed." >&2
    exit 1
  fi
  ;;
show) cat "$s/mainpid-$5" 2>/dev/null || echo 0 ;;
*)
  echo "systemctl stub: unexpected $*" >&2
  exit 2
  ;;
esac
`

// resolvectlStub prints the servers of resolvectl dns when the test gave
// them; without them it fails, as without a running resolved.
const resolvectlStub = `#!/bin/sh
[ "$1" = dns ] || exit 2
[ -f "$NCD_TEST_STATE/resolvectl-dns" ] || exit 1
cat "$NCD_TEST_STATE/resolvectl-dns"
`

const ipStub = `#!/bin/sh
if [ "$*" != "-4 -o addr show dev docker0" ]; then
  echo "ip stub: unexpected $*" >&2
  exit 2
fi
if [ ! -f "$NCD_TEST_STATE/docker0" ]; then
  echo 'Device "docker0" does not exist.' >&2
  exit 1
fi
cat "$NCD_TEST_STATE/docker0"
`

// rcServiceStub is rc-service of OpenRC: status exits 0 for a started
// service and 3 for a stopped one, as OpenRC does.
const rcServiceStub = `#!/bin/sh
s=$NCD_TEST_STATE
echo "$*" >>"$s/rc-service"
case $2 in
status)
  [ -f "$s/started-$1" ] && exit 0
  exit 3
  ;;
restart)
  echo "$1" >>"$s/restarts"
  if [ -f "$s/fail-restart-$1" ]; then
    echo " * ERROR: $1 failed to start" >&2
    exit 1
  fi
  ;;
*)
  echo "rc-service stub: unexpected $*" >&2
  exit 2
  ;;
esac
`

// pidofStub prints the pids the test gave for a program.
const pidofStub = `#!/bin/sh
cat "$NCD_TEST_STATE/pidof-$1" 2>/dev/null || exit 1
`

// stubs are the commands newHost writes into the bin directory of a host,
// by name.
var stubs = map[string]string{
	"systemctl": systemctlStub, "ip": ipStub, "resolvectl": resolvectlStub,
	"rc-service": rcServiceStub, "pidof": pidofStub,
}

// host is a fixture host: the files below root, the state of the stubs,
// and a fake connector that runs the uploaded script there.
type host struct {
	t     *testing.T
	sh    shell
	root  string
	state string
	bin   string
	conn  *connector.FakeConnector
	// init is the init system the deployer found on the host (NCD_INIT).
	init string
}

func newHost(t *testing.T, sh shell) *host {
	t.Helper()
	h := &host{t: t, sh: sh, root: t.TempDir(), state: t.TempDir(), bin: t.TempDir(), init: config.InitSystemd}
	for name, stub := range stubs {
		h.write(filepath.Join(h.bin, name), stub, 0o755)
	}
	// A running systemd, a Debian 12 without a local resolver.
	if err := os.MkdirAll(filepath.Join(h.root, "run/systemd/system"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.version("252")
	h.file("/etc/resolv.conf", "nameserver 10.0.0.2\n")
	h.file("/etc/nsswitch.conf", "passwd: files\nhosts:          files dns\n")
	h.conn = connector.NewFakeConnector()
	h.conn.Handler = h.handle
	return h
}

func (h *host) write(p, content string, mode os.FileMode) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		h.t.Fatal(err)
	}
}

// file writes a file of the host.
func (h *host) file(p, content string) { h.write(filepath.Join(h.root, p), content, 0o644) }

// read returns a file of the host, "" when it is missing.
func (h *host) read(p string) string {
	data, err := os.ReadFile(filepath.Join(h.root, p))
	if err != nil {
		return ""
	}
	return string(data)
}

func (h *host) exists(p string) bool {
	_, err := os.Stat(filepath.Join(h.root, p))
	return err == nil
}

func (h *host) flag(name string) { h.write(filepath.Join(h.state, name), "", 0o644) }

func (h *host) version(v string) {
	h.write(filepath.Join(h.state, "version"), "systemd "+v+" ("+v+".30-1~deb12u2)\n+PAM +AUDIT\n", 0o644)
}

// resolved makes systemd-resolved the active resolver of the host.
func (h *host) resolved() {
	h.write(filepath.Join(h.root, "usr/lib/systemd/systemd-resolved"), "", 0o755)
	h.flag("active-systemd-resolved")
	h.file("/etc/resolv.conf", "# This is /run/systemd/resolve/stub-resolv.conf\nnameserver 127.0.0.53\noptions edns0 trust-ad\nsearch .\n")
}

// dnsmasq makes dnsmasq the active resolver, started as Debian's unit
// starts it (conf-dir on the command line).
func (h *host) dnsmasq() {
	h.write(filepath.Join(h.root, "usr/sbin/dnsmasq"), "", 0o755)
	h.flag("active-dnsmasq")
	h.file("/etc/resolv.conf", "nameserver 127.0.0.1\n")
	h.write(filepath.Join(h.state, "mainpid-dnsmasq"), "4242\n", 0o644)
	h.file("/proc/4242/cmdline", strings.Join([]string{"/usr/sbin/dnsmasq", "-x", "/run/dnsmasq/dnsmasq.pid", "-u", "dnsmasq",
		"-7", "/etc/dnsmasq.d,.dpkg-dist,.dpkg-old,.dpkg-new", "--local-service", ""}, "\x00"))
}

// openrc makes the host an Alpine node with OpenRC booted and without a
// local resolver: systemctl is not called there (the stub of systemctl
// fails).
func (h *host) openrc() {
	h.t.Helper()
	h.init = config.InitOpenRC
	if err := os.RemoveAll(filepath.Join(h.root, "run/systemd")); err != nil {
		h.t.Fatal(err)
	}
	h.file("/run/openrc/softlevel", "default\n")
	h.file("/etc/nsswitch.conf", "")
	h.write(filepath.Join(h.bin, "systemctl"), "#!/bin/sh\necho \"systemctl $*\" >>\"$NCD_TEST_STATE/systemctl\"\nexit 2\n", 0o755)
}

// alpineDnsmasq makes dnsmasq the started resolver of an OpenRC host, set up
// as the documentation says: the dnsmasq.conf of the Alpine package
// (conf-dir), its pid file and command line, 127.0.0.1 in resolv.conf and
// the servers of the network in /etc/resolv.dnsmasq.
func (h *host) alpineDnsmasq() {
	h.write(filepath.Join(h.root, "usr/sbin/dnsmasq"), "", 0o755)
	h.flag("started-dnsmasq")
	h.file("/etc/resolv.conf", "nameserver 127.0.0.1\n")
	h.file("/etc/dnsmasq.conf", "#conf-dir=/etc/dnsmasq.d\nlocal-service\nconf-dir=/etc/dnsmasq.d/,*.conf\n")
	h.file("/etc/dnsmasq.d/local.conf", "listen-address=127.0.0.1\nbind-interfaces\nresolv-file=/etc/resolv.dnsmasq\n")
	h.file("/etc/resolv.dnsmasq", "nameserver 10.0.0.2\n")
	h.file("/run/dnsmasq.pid", "412\n")
	h.file("/proc/412/cmdline", strings.Join([]string{"/usr/sbin/dnsmasq", "--keep-in-foreground", "--pid-file=",
		"--conf-file=/etc/dnsmasq.conf", ""}, "\x00"))
}

// assertNoSystemctl fails when the script called systemctl on an OpenRC
// host.
func (h *host) assertNoSystemctl() {
	h.t.Helper()
	if data, err := os.ReadFile(filepath.Join(h.state, "systemctl")); err == nil {
		h.t.Errorf("systemctl called on an OpenRC host:\n%s", data)
	}
}

func (h *host) docker0(addr string) {
	h.write(filepath.Join(h.state, "docker0"),
		"4: docker0    inet "+addr+" brd 172.17.255.255 scope global docker0\\       valid_lft forever preferred_lft forever\n", 0o644)
	h.flag("active-docker")
}

func (h *host) restarts() []string {
	data, err := os.ReadFile(filepath.Join(h.state, "restarts"))
	if err != nil {
		return nil
	}
	return strings.Fields(string(data))
}

func (h *host) handle(cmd string) (string, error, bool) {
	switch {
	case strings.HasPrefix(cmd, "# when: "+StepName+"\n"):
		return h.run(cmd)
	case strings.Contains(cmd, " sh "+ScriptPath):
		prefix, rest, _ := strings.Cut(cmd, " sh "+ScriptPath)
		script := string(h.conn.Files[ScriptPath])
		if h.sh.machine {
			script = "set -euo pipefail\n" + Functions() + "\nif norca_consul_dns; then exit 0; else exit 1; fi\n"
		}
		for stub := range stubs {
			if slices.Contains(h.sh.applets, stub) {
				script = stub + "() { " + connector.ShellQuote(filepath.Join(h.bin, stub)) + " \"$@\"; }\n" + script
			}
		}
		p := filepath.Join(h.bin, "consul-dns.sh")
		h.write(p, script, 0o600)
		var argv []string
		for _, a := range h.sh.argv {
			argv = append(argv, connector.ShellQuote(a))
		}
		out, err, handled := h.run(prefix + " " + strings.Join(argv, " ") + " " + connector.ShellQuote(p))
		if err == nil && strings.Contains(rest, "rm -f "+ScriptPath) {
			delete(h.conn.Files, ScriptPath)
		}
		return out, err, handled

	}
	h.t.Errorf("unexpected command %q", cmd)
	return "", errors.New("unexpected command"), true
}

func (h *host) run(cmd string) (string, error, bool) {
	c := exec.Command("/bin/sh", "-c", cmd)
	c.Env = []string{"PATH=" + h.bin + ":" + h.sh.path, "NCD_TEST_STATE=" + h.state, "TMPDIR=" + h.state, "LC_ALL=C"}
	out, err := c.CombinedOutput()
	return string(out), err, true
}

func (h *host) apply(f *config.ConsulDNSForwarding) (*Result, error) {
	h.t.Helper()
	res, err := apply(h.t.Context(), h.conn, "10.0.0.1", h.init, f, h.root)
	if res != nil {
		for _, e := range res.Errors {
			h.t.Logf("error line: %s", e)
		}
	}
	return res, err
}

// mustApply applies and fails the test on an error.
func (h *host) mustApply(f *config.ConsulDNSForwarding) *Result {
	h.t.Helper()
	res, err := h.apply(f)
	if err != nil {
		h.t.Fatalf("apply: %v", err)
	}
	return res
}

func forward(method string, docker bool) *config.ConsulDNSForwarding {
	return &config.ConsulDNSForwarding{Enabled: true, Address: "127.0.0.1:8600", Domain: "consul", Method: method, Docker: docker}
}

// eachShell runs a test on a fresh host for every shell.
func eachShell(t *testing.T, test func(t *testing.T, h *host)) {
	for _, sh := range shells(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			test(t, newHost(t, sh))
		})
	}
}

// managedFiles are the files the script may write.
var managedFiles = []string{ResolvedDropIn, TrustAnchor, DnsmasqConf, DaemonJSON}

// assertUntouched fails when the script wrote a file or restarted a service.
func (h *host) assertUntouched() {
	h.t.Helper()
	for _, p := range managedFiles {
		if h.exists(p) {
			h.t.Errorf("%s was written:\n%s", p, h.read(p))
		}
	}
	if r := h.restarts(); len(r) > 0 {
		h.t.Errorf("restarted %v", r)
	}
}

const wantDropIn = `# Managed by norca-deployer. Do not edit.
# consul.dns_forwarding: the Consul agent answers the consul domain; other
# names are answered by the servers the host has.
[Resolve]
DNS=127.0.0.1:8600
Domains=~consul
`

// wantAnchor keeps the built-in negative trust anchors of systemd-resolved,
// which any .negative file replaces (seen on Debian 12, systemd 252: the
// journal listed only "consul" after the file of the domain alone).
const wantAnchor = `; Managed by norca-deployer. Do not edit.
; consul.dns_forwarding: no DNSSEC validation below the consul domain, whose
; answers the Consul agent does not sign.
consul
; A .negative file replaces the built-in negative trust anchors of
; systemd-resolved, so they follow (the list of systemd 252).
home.arpa
10.in-addr.arpa
16.172.in-addr.arpa
17.172.in-addr.arpa
18.172.in-addr.arpa
19.172.in-addr.arpa
20.172.in-addr.arpa
21.172.in-addr.arpa
22.172.in-addr.arpa
23.172.in-addr.arpa
24.172.in-addr.arpa
25.172.in-addr.arpa
26.172.in-addr.arpa
27.172.in-addr.arpa
28.172.in-addr.arpa
29.172.in-addr.arpa
30.172.in-addr.arpa
31.172.in-addr.arpa
170.0.0.192.in-addr.arpa
171.0.0.192.in-addr.arpa
168.192.in-addr.arpa
d.f.ip6.arpa
ipv4only.arpa
corp
home
internal
intranet
lan
local
private
test
`

const wantDnsmasq = `# Managed by norca-deployer. Do not edit.
# consul.dns_forwarding: the consul domain goes to the Consul agent; other
# names keep going to the servers dnsmasq has.
server=/consul/127.0.0.1#8600
`

func TestScriptNoResolver(t *testing.T) {
	eachShell(t, func(t *testing.T, h *host) {
		_, err := h.apply(forward(config.DNSMethodAuto, false))
		want := "consul.dns_forwarding: 10.0.0.1 has no local resolver the deployer can forward the consul domain from: " +
			"systemd-resolved not installed, dnsmasq not installed, /etc/resolv.conf names 10.0.0.2. " +
			"Install systemd-resolved or dnsmasq with host_packages, or set consul.dns_forwarding.enabled = false"
		if err == nil || err.Error() != want {
			t.Fatalf("apply() = %v\nwant %s", err, want)
		}
		h.assertUntouched()
	})
}

func TestScriptResolverNotUsable(t *testing.T) {
	tests := []struct {
		name  string
		setup func(h *host)
		want  string
	}{
		{
			name: "systemd-resolved installed, not running",
			setup: func(h *host) {
				h.write(filepath.Join(h.root, "lib/systemd/systemd-resolved"), "", 0o755)
			},
			want: "systemd-resolved inactive, dnsmasq not installed, /etc/resolv.conf names 10.0.0.2.",
		},
		{
			name: "systemd-resolved running, not used",
			setup: func(h *host) {
				h.resolved()
				h.file("/etc/resolv.conf", "nameserver 10.0.0.2\nnameserver 10.0.0.3\n")
			},
			want: "systemd-resolved active but not used for lookups (no 127.0.0.53 in /etc/resolv.conf, no resolve in the hosts line of /etc/nsswitch.conf), " +
				"dnsmasq not installed, /etc/resolv.conf names 10.0.0.2 10.0.0.3.",
		},
		{
			name: "dnsmasq running, not named",
			setup: func(h *host) {
				h.dnsmasq()
				h.file("/etc/resolv.conf", "")
			},
			want: "systemd-resolved not installed, dnsmasq active but not named in /etc/resolv.conf (no 127.0.0.1), /etc/resolv.conf names no server.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eachShell(t, func(t *testing.T, h *host) {
				tt.setup(h)
				_, err := h.apply(forward(config.DNSMethodAuto, false))
				if err == nil || !strings.Contains(err.Error(), "has no local resolver") || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("apply() = %v\nwant %s", err, tt.want)
				}
				h.assertUntouched()
			})
		})
	}
}

func TestScriptResolved(t *testing.T) {
	eachShell(t, func(t *testing.T, h *host) {
		h.resolved()
		res := h.mustApply(forward(config.DNSMethodAuto, false))
		if got := h.read(ResolvedDropIn); got != wantDropIn {
			t.Errorf("%s:\n%s\nwant\n%s", ResolvedDropIn, got, wantDropIn)
		}
		if got := h.read(TrustAnchor); got != wantAnchor {
			t.Errorf("%s:\n%s\nwant\n%s", TrustAnchor, got, wantAnchor)
		}
		if res.Method != config.DNSMethodResolved || !slices.Equal(res.Changed, []string{TrustAnchor, ResolvedDropIn}) ||
			!slices.Equal(res.Restarted, []string{"systemd-resolved"}) || res.Bridge.IsValid() {
			t.Fatalf("result = %+v", res)
		}
		if h.exists(DnsmasqConf) || h.exists(DaemonJSON) {
			t.Fatal("files of dnsmasq or docker written")
		}

		// The second run changes nothing and restarts nothing.
		again := h.mustApply(forward(config.DNSMethodResolved, false))
		if len(again.Changed) != 0 || len(again.Restarted) != 0 || len(again.Unchanged) != 2 {
			t.Fatalf("second run = %+v", again)
		}
		if r := h.restarts(); !slices.Equal(r, []string{"systemd-resolved"}) {
			t.Fatalf("restarts = %v", r)
		}
	})
}

// The NSS module of resolved is enough: getent goes through it, even when
// /etc/resolv.conf names the upstream servers.
func TestScriptResolvedThroughNSS(t *testing.T) {
	eachShell(t, func(t *testing.T, h *host) {
		h.resolved()
		h.file("/etc/resolv.conf", "nameserver 10.0.0.2\n")
		h.file("/etc/nsswitch.conf", "hosts: files resolve [!UNAVAIL=return] dns\n")
		if res := h.mustApply(forward(config.DNSMethodAuto, false)); res.Method != config.DNSMethodResolved {
			t.Fatalf("method = %q", res.Method)
		}
	})
}

// systemd-resolved without a server for other names (Debian 12 with
// ifupdown right after systemd-resolved is installed: the DHCP server comes
// with the next lease): a warning, on every run until it is fixed.
func TestScriptResolvedWithoutUpstream(t *testing.T) {
	tests := []struct {
		name    string
		servers string
		warn    bool
	}{
		{name: "only the Consul agent", servers: "Global: 127.0.0.1:8600\nLink 2 (mgmt0):\nLink 3 (lab0):\nLink 4 (docker0):\n", warn: true},
		{name: "a server on a link", servers: "Global: 127.0.0.1:8600\nLink 2 (mgmt0): 172.25.64.1\nLink 3 (lab0):\n"},
		{name: "a global server too", servers: "Global: 127.0.0.1:8600 10.0.0.2\nLink 2 (eth0):\n"},
		{name: "an IPv6 server on a link", servers: "Global: 127.0.0.1:8600\nLink 2 (eth0): fe80::1%2\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eachShell(t, func(t *testing.T, h *host) {
				h.resolved()
				h.write(filepath.Join(h.state, "resolvectl-dns"), tt.servers, 0o644)
				for run := range 2 {
					res := h.mustApply(forward(config.DNSMethodAuto, false))
					got := len(res.Warnings) == 1 && strings.Contains(res.Warnings[0], "systemd-resolved has no DNS server but the Consul agent")
					if got != tt.warn || len(res.Warnings) > 1 {
						t.Fatalf("run %d: warnings = %q, want the warning: %v", run+1, res.Warnings, tt.warn)
					}
				}
			})
		})
	}
}

func TestScriptOtherDomainAndPort(t *testing.T) {
	eachShell(t, func(t *testing.T, h *host) {
		h.resolved()
		h.mustApply(&config.ConsulDNSForwarding{Enabled: true, Address: "[::1]:18600", Domain: "dc1.example", Method: config.DNSMethodAuto})
		if got := h.read(ResolvedDropIn); !strings.Contains(got, "\nDNS=[::1]:18600\nDomains=~dc1.example\n") {
			t.Fatalf("%s:\n%s", ResolvedDropIn, got)
		}
		if got := h.read(TrustAnchor); !strings.Contains(got, "\ndc1.example\n;") || !strings.HasSuffix(got, "\ntest\n") {
			t.Fatalf("%s:\n%s", TrustAnchor, got)
		}
	})
}

func TestScriptDnsmasq(t *testing.T) {
	tests := []struct {
		name  string
		setup func(h *host)
	}{
		{name: "conf-dir on the command line (Debian)", setup: func(*host) {}},
		{
			name: "conf-dir in dnsmasq.conf",
			setup: func(h *host) {
				h.write(filepath.Join(h.state, "mainpid-dnsmasq"), "0\n", 0o644)
				h.file("/etc/dnsmasq.conf", "# local\nconf-dir=/etc/dnsmasq.d/,*.conf\n")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eachShell(t, func(t *testing.T, h *host) {
				h.dnsmasq()
				tt.setup(h)
				res := h.mustApply(forward(config.DNSMethodAuto, false))
				if got := h.read(DnsmasqConf); got != wantDnsmasq {
					t.Errorf("%s:\n%s\nwant\n%s", DnsmasqConf, got, wantDnsmasq)
				}
				if res.Method != config.DNSMethodDnsmasq || !slices.Equal(res.Restarted, []string{"dnsmasq"}) {
					t.Fatalf("result = %+v", res)
				}
				if h.exists(ResolvedDropIn) || h.exists(TrustAnchor) {
					t.Fatal("files of systemd-resolved written")
				}
				again := h.mustApply(forward(config.DNSMethodDnsmasq, false))
				if len(again.Changed) != 0 || len(again.Restarted) != 0 {
					t.Fatalf("second run = %+v", again)
				}
			})
		})
	}
}

func TestScriptDnsmasqWithoutConfDir(t *testing.T) {
	eachShell(t, func(t *testing.T, h *host) {
		h.dnsmasq()
		h.write(filepath.Join(h.state, "mainpid-dnsmasq"), "0\n", 0o644)
		h.file("/etc/dnsmasq.conf", "#conf-dir=/etc/dnsmasq.d/,*.conf\n")
		_, err := h.apply(forward(config.DNSMethodAuto, false))
		if err == nil || !strings.Contains(err.Error(), "10.0.0.1 runs a dnsmasq that does not read /etc/dnsmasq.d") {
			t.Fatalf("apply() = %v", err)
		}
		h.assertUntouched()
	})
}

func TestScriptExplicitMethod(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(h *host)
		method string
		want   string
	}{
		{
			name:   "dnsmasq asked, resolved there",
			setup:  (*host).resolved,
			method: config.DNSMethodDnsmasq,
			want:   "10.0.0.1 cannot forward the consul domain with dnsmasq (consul.dns_forwarding.method): dnsmasq not installed, /etc/resolv.conf names 127.0.0.53.",
		},
		{
			name:   "resolved asked, dnsmasq there",
			setup:  (*host).dnsmasq,
			method: config.DNSMethodResolved,
			want:   "10.0.0.1 cannot forward the consul domain with systemd-resolved (consul.dns_forwarding.method): systemd-resolved not installed, /etc/resolv.conf names 127.0.0.1.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eachShell(t, func(t *testing.T, h *host) {
				tt.setup(h)
				_, err := h.apply(forward(tt.method, false))
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("apply() = %v\nwant %s", err, tt.want)
				}
				h.assertUntouched()
			})
		})
	}
}

func TestScriptOldSystemd(t *testing.T) {
	eachShell(t, func(t *testing.T, h *host) {
		h.resolved()
		h.version("245")
		_, err := h.apply(forward(config.DNSMethodAuto, false))
		if err == nil || !strings.Contains(err.Error(), "10.0.0.1 runs systemd 245: systemd-resolved sends to a DNS server on a port other than 53 (127.0.0.1:8600) from systemd 246 on") {
			t.Fatalf("apply() = %v", err)
		}
		h.assertUntouched()

		// Port 53 works with any version.
		f := forward(config.DNSMethodAuto, false)
		f.Address = "127.0.0.1:53"
		h.mustApply(f)
		if !strings.Contains(h.read(ResolvedDropIn), "\nDNS=127.0.0.1:53\n") {
			t.Fatalf("drop-in:\n%s", h.read(ResolvedDropIn))
		}
	})
}

func TestScriptDocker(t *testing.T) {
	eachShell(t, func(t *testing.T, h *host) {
		h.resolved()
		h.docker0("10.230.0.1/24")
		res := h.mustApply(forward(config.DNSMethodAuto, true))
		if got := h.read(ResolvedDropIn); got != wantDropIn+"DNSStubListenerExtra=10.230.0.1\n" {
			t.Errorf("%s:\n%s", ResolvedDropIn, got)
		}
		if got := h.read(DaemonJSON); got != "{\n  \"dns\": [\"10.230.0.1\"]\n}\n" {
			t.Errorf("%s:\n%s", DaemonJSON, got)
		}
		if res.Bridge.String() != "10.230.0.1/24" || !slices.Equal(res.Restarted, []string{"systemd-resolved", "docker"}) {
			t.Fatalf("result = %+v", res)
		}
		if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "docker was restarted") || !strings.Contains(res.Warnings[0], "live-restore") {
			t.Fatalf("warnings = %q, want the restart of docker", res.Warnings)
		}

		// Unchanged: nothing restarted, the bridge still reported for the
		// firewall rules.
		again := h.mustApply(forward(config.DNSMethodAuto, true))
		if len(again.Changed) != 0 || len(again.Restarted) != 0 || len(again.Warnings) != 0 || again.Bridge.String() != "10.230.0.1/24" {
			t.Fatalf("second run = %+v", again)
		}
	})
}

// The dns key is merged into the daemon.json of the host, every other key
// kept (the lab moves docker0 with bip).
func TestScriptDockerMergesDaemonJSON(t *testing.T) {
	const existing = "{\n  \"bip\": \"10.230.0.1/24\",\n  \"log-driver\": \"journald\",\n  \"dns\": [\"8.8.8.8\"]\n}\n"
	eachShell(t, func(t *testing.T, h *host) {
		h.resolved()
		h.docker0("10.230.0.1/24")
		h.file(DaemonJSON, existing)
		res, err := h.apply(forward(config.DNSMethodAuto, true))
		if !h.sh.hasPython() {
			// busybox: no jq, no python3.
			if err == nil || !strings.Contains(err.Error(), "neither jq nor python3 is installed. Install jq with host_packages") {
				t.Fatalf("apply() = %v", err)
			}
			if h.exists(ResolvedDropIn) || h.read(DaemonJSON) != existing {
				t.Fatal("files changed before the error")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		want := "{\n  \"bip\": \"10.230.0.1/24\",\n  \"log-driver\": \"journald\",\n  \"dns\": [\n    \"10.230.0.1\"\n  ]\n}\n"
		if got := h.read(DaemonJSON); got != want {
			t.Fatalf("%s:\n%s\nwant\n%s", DaemonJSON, got, want)
		}
		if !slices.Contains(res.Restarted, "docker") {
			t.Fatalf("result = %+v", res)
		}
		again := h.mustApply(forward(config.DNSMethodAuto, true))
		if len(again.Changed) != 0 || len(again.Restarted) != 0 {
			t.Fatalf("second run = %+v", again)
		}
	})
}

func TestScriptDockerRefusals(t *testing.T) {
	tests := []struct {
		name  string
		setup func(h *host)
		want  string
	}{
		{
			name: "systemd 246",
			setup: func(h *host) {
				h.resolved()
				h.version("246")
				h.docker0("172.17.0.1/16")
			},
			want: "10.0.0.1 runs systemd 246: consul.dns_forwarding.docker needs DNSStubListenerExtra= of systemd 247 or newer",
		},
		{
			name:  "no docker0",
			setup: (*host).resolved,
			want:  "10.0.0.1 has no docker0 bridge with an IPv4 address",
		},
		{
			name: "daemon.json does not parse",
			setup: func(h *host) {
				h.resolved()
				h.docker0("172.17.0.1/16")
				h.file(DaemonJSON, "{\"bip\": ")
			},
			want: "/etc/docker/daemon.json", // not a JSON object, or no tool to read it (busybox)
		},
		{
			name: "dnsmasq",
			setup: func(h *host) {
				h.dnsmasq()
				h.docker0("172.17.0.1/16")
			},
			want: "10.0.0.1 cannot let containers resolve the consul domain with dnsmasq",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eachShell(t, func(t *testing.T, h *host) {
				tt.setup(h)
				before := h.read(DaemonJSON)
				_, err := h.apply(forward(config.DNSMethodAuto, true))
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("apply() = %v\nwant %s", err, tt.want)
				}
				if h.exists(ResolvedDropIn) || h.exists(TrustAnchor) || h.exists(DnsmasqConf) || h.read(DaemonJSON) != before || len(h.restarts()) > 0 {
					t.Fatal("the host was changed before the error")
				}
			})
		})
	}
}

func TestScriptDisable(t *testing.T) {
	eachShell(t, func(t *testing.T, h *host) {
		h.resolved()
		h.docker0("172.17.0.1/16")
		h.mustApply(forward(config.DNSMethodAuto, true))

		res := h.mustApply(&config.ConsulDNSForwarding{Enabled: false})
		if h.exists(ResolvedDropIn) || h.exists(TrustAnchor) {
			t.Fatal("the files of systemd-resolved are still there")
		}
		if !strings.Contains(h.read(DaemonJSON), "\"dns\"") {
			t.Fatal("the dns key of daemon.json was removed")
		}
		if !slices.Equal(res.Removed, []string{ResolvedDropIn, TrustAnchor}) || !slices.Equal(res.Restarted, []string{"systemd-resolved"}) {
			t.Fatalf("result = %+v", res)
		}
		if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], DaemonJSON+" keeps its \"dns\" key") {
			t.Fatalf("warnings = %q, want the dns key of daemon.json named", res.Warnings)
		}
		if _, ok := h.conn.Files[ScriptPath]; ok {
			t.Fatal("the script stays on the node")
		}

		// Nothing left: no upload, no run.
		n := len(h.conn.Commands)
		if res := h.mustApply(nil); res.Method != "" || len(h.conn.Commands) != n+1 {
			t.Fatalf("third run: %+v, commands %q", res, h.conn.Commands[n:])
		}
	})
}

// docker: true turned off keeps the forwarding and warns about daemon.json.
func TestScriptDockerTurnedOff(t *testing.T) {
	eachShell(t, func(t *testing.T, h *host) {
		h.resolved()
		h.docker0("172.17.0.1/16")
		h.mustApply(forward(config.DNSMethodAuto, true))
		res := h.mustApply(forward(config.DNSMethodAuto, false))
		if got := h.read(ResolvedDropIn); got != wantDropIn {
			t.Fatalf("%s:\n%s", ResolvedDropIn, got)
		}
		if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "keeps its \"dns\" key") {
			t.Fatalf("warnings = %q", res.Warnings)
		}
	})
}

// A host that moves from dnsmasq to systemd-resolved loses the dnsmasq file.
func TestScriptSwitchMethod(t *testing.T) {
	eachShell(t, func(t *testing.T, h *host) {
		h.dnsmasq()
		h.mustApply(forward(config.DNSMethodAuto, false))
		h.resolved()
		res := h.mustApply(forward(config.DNSMethodResolved, false))
		if h.exists(DnsmasqConf) || !h.exists(ResolvedDropIn) {
			t.Fatal("the dnsmasq file stays or the drop-in is missing")
		}
		if !slices.Equal(res.Removed, []string{DnsmasqConf}) || !slices.Equal(h.restarts(), []string{"dnsmasq", "systemd-resolved", "dnsmasq"}) {
			t.Fatalf("result = %+v, restarts %v", res, h.restarts())
		}
	})
}

// Without a running systemd (an image built in a container) the enabled
// unit decides, and nothing is restarted.
func TestScriptWithoutRunningSystemd(t *testing.T) {
	eachShell(t, func(t *testing.T, h *host) {
		if err := os.RemoveAll(filepath.Join(h.root, "run")); err != nil {
			t.Fatal(err)
		}
		h.write(filepath.Join(h.root, "usr/lib/systemd/systemd-resolved"), "", 0o755)
		h.flag("enabled-systemd-resolved")
		res := h.mustApply(forward(config.DNSMethodAuto, true))
		if res.Method != config.DNSMethodResolved || len(res.Restarted) != 0 || h.read(ResolvedDropIn) != wantDropIn {
			t.Fatalf("result = %+v, drop-in:\n%s", res, h.read(ResolvedDropIn))
		}
		if len(res.Warnings) != 2 || !strings.Contains(res.Warnings[0], "systemd does not run") || !strings.Contains(res.Warnings[1], "docker0 is not there") {
			t.Fatalf("warnings = %q", res.Warnings)
		}
	})
}

func TestScriptRestartFails(t *testing.T) {
	eachShell(t, func(t *testing.T, h *host) {
		h.resolved()
		h.flag("fail-restart-systemd-resolved")
		_, err := h.apply(forward(config.DNSMethodAuto, false))
		if err == nil || !strings.Contains(err.Error(), "10.0.0.1 could not restart systemd-resolved: Job for systemd-resolved.service failed.") {
			t.Fatalf("apply() = %v", err)
		}
	})
}

// An Alpine node with OpenRC and dnsmasq set up as the documentation says:
// the method is dnsmasq, its file is written and dnsmasq restarted with
// rc-service; systemctl is never called. The conf-dir of dnsmasq is found
// in dnsmasq.conf (the Alpine package), on the command line of the dnsmasq
// of the pid file, or on that of a dnsmasq pidof finds.
func TestScriptOpenRCDnsmasq(t *testing.T) {
	tests := []struct {
		name  string
		setup func(h *host)
	}{
		{name: "conf-dir in dnsmasq.conf (Alpine)", setup: func(*host) {}},
		{
			name: "conf-dir on the command line of the pid file",
			setup: func(h *host) {
				h.file("/etc/dnsmasq.conf", "local-service\n")
				h.file("/proc/412/cmdline", "/usr/sbin/dnsmasq\x00--conf-file=/etc/dnsmasq.conf\x00--conf-dir=/etc/dnsmasq.d,*.conf\x00")
			},
		},
		{
			name: "conf-dir on the command line of a dnsmasq under supervise-daemon",
			setup: func(h *host) {
				h.file("/etc/dnsmasq.conf", "local-service\n")
				if err := os.Remove(filepath.Join(h.root, "run/dnsmasq.pid")); err != nil {
					h.t.Fatal(err)
				}
				h.write(filepath.Join(h.state, "pidof-dnsmasq"), "77 513\n", 0o644)
				h.file("/proc/513/cmdline", "/usr/sbin/dnsmasq\x00-7\x00/etc/dnsmasq.d/\x00")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eachShell(t, func(t *testing.T, h *host) {
				h.openrc()
				h.alpineDnsmasq()
				tt.setup(h)
				res := h.mustApply(forward(config.DNSMethodAuto, false))
				if got := h.read(DnsmasqConf); got != wantDnsmasq {
					t.Errorf("%s:\n%s\nwant\n%s", DnsmasqConf, got, wantDnsmasq)
				}
				if res.Method != config.DNSMethodDnsmasq || !slices.Equal(res.Changed, []string{DnsmasqConf}) ||
					!slices.Equal(res.Restarted, []string{"dnsmasq"}) || len(res.Warnings) != 0 {
					t.Fatalf("result = %+v", res)
				}
				if h.exists(ResolvedDropIn) || h.exists(TrustAnchor) {
					t.Fatal("files of systemd-resolved written")
				}
				again := h.mustApply(forward(config.DNSMethodDnsmasq, false))
				if len(again.Changed) != 0 || len(again.Restarted) != 0 || !slices.Equal(again.Unchanged, []string{DnsmasqConf}) {
					t.Fatalf("second run = %+v", again)
				}
				if r := h.restarts(); !slices.Equal(r, []string{"dnsmasq"}) {
					t.Fatalf("restarts = %v", r)
				}
				h.assertNoSystemctl()
			})
		})
	}
}

// The refusals of an OpenRC node, each before anything is changed.
func TestScriptOpenRCRefusals(t *testing.T) {
	const remedy = "start it (rc-update add dnsmasq default; rc-service dnsmasq start) and name 127.0.0.1 in /etc/resolv.conf"
	tests := []struct {
		name   string
		setup  func(h *host)
		method string
		docker bool
		want   string
	}{
		{
			name:  "no dnsmasq",
			setup: func(*host) {},
			want: "consul.dns_forwarding: 10.0.0.1 has no local resolver the deployer can forward the consul domain from: " +
				"dnsmasq not installed, /etc/resolv.conf names 10.0.0.2. OpenRC has no systemd-resolved: install dnsmasq with host_packages, " +
				remedy + ", or set consul.dns_forwarding.enabled = false",
		},
		{
			name: "dnsmasq installed, not started",
			setup: func(h *host) {
				h.alpineDnsmasq()
				if err := os.Remove(filepath.Join(h.state, "started-dnsmasq")); err != nil {
					h.t.Fatal(err)
				}
			},
			want: "has no local resolver the deployer can forward the consul domain from: dnsmasq inactive, /etc/resolv.conf names 127.0.0.1.",
		},
		{
			name: "dnsmasq not named in resolv.conf",
			setup: func(h *host) {
				h.alpineDnsmasq()
				h.file("/etc/resolv.conf", "search example.com\nnameserver 10.0.0.2\n")
			},
			want: "dnsmasq active but not named in /etc/resolv.conf (no 127.0.0.1), /etc/resolv.conf names 10.0.0.2.",
		},
		{
			name:   "dnsmasq asked, not installed",
			setup:  func(*host) {},
			method: config.DNSMethodDnsmasq,
			want: "10.0.0.1 cannot forward the consul domain with dnsmasq (consul.dns_forwarding.method): dnsmasq not installed, " +
				"/etc/resolv.conf names 10.0.0.2. Install dnsmasq with host_packages, " + remedy,
		},
		{
			name: "conf-dir missing",
			setup: func(h *host) {
				h.alpineDnsmasq()
				h.file("/etc/dnsmasq.conf", "local-service\n#conf-dir=/etc/dnsmasq.d/,*.conf\n")
			},
			want: "10.0.0.1 runs a dnsmasq that does not read /etc/dnsmasq.d (no conf-dir=/etc/dnsmasq.d in /etc/dnsmasq.conf or on its command line); " +
				"add conf-dir=/etc/dnsmasq.d/,*.conf to /etc/dnsmasq.conf (the dnsmasq.conf of the Alpine package has that line)",
		},
		{
			name:   "systemd-resolved asked",
			setup:  (*host).alpineDnsmasq,
			method: config.DNSMethodResolved,
			want: "10.0.0.1 cannot forward the consul domain with systemd-resolved (consul.dns_forwarding.method): " +
				"the node runs OpenRC, which has no systemd-resolved; use method auto or dnsmasq",
		},
		{
			name:   "docker",
			setup:  (*host).alpineDnsmasq,
			docker: true,
			want: "10.0.0.1 cannot let containers resolve the consul domain (consul.dns_forwarding.docker): " +
				"the node runs OpenRC, and the containers need systemd-resolved; set docker = false",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eachShell(t, func(t *testing.T, h *host) {
				h.openrc()
				tt.setup(h)
				method := tt.method
				if method == "" {
					method = config.DNSMethodAuto
				}
				_, err := h.apply(forward(method, tt.docker))
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("apply() = %v\nwant %s", err, tt.want)
				}
				h.assertUntouched()
				h.assertNoSystemctl()
			})
		})
	}
}

// dnsmasq without a server for other names: it reads /etc/resolv.conf,
// which names only itself (dnsmasq skips its own address), and nothing
// gives it another server. A warning, on every run until it is fixed.
func TestScriptOpenRCDnsmasqWithoutUpstream(t *testing.T) {
	tests := []struct {
		name  string
		setup func(h *host)
		warn  bool
	}{
		{name: "resolv-file in /etc/dnsmasq.d", setup: func(*host) {}},
		{
			name:  "only resolv.conf",
			setup: func(h *host) { h.file("/etc/dnsmasq.d/local.conf", "listen-address=127.0.0.1\n") },
			warn:  true,
		},
		{
			name: "only resolv.conf, with ::1 too",
			setup: func(h *host) {
				h.file("/etc/dnsmasq.d/local.conf", "")
				h.file("/etc/resolv.conf", "nameserver 127.0.0.1\nnameserver ::1\n")
			},
			warn: true,
		},
		{
			name:  "a server line of a domain is no upstream",
			setup: func(h *host) { h.file("/etc/dnsmasq.d/local.conf", "server=/lan/10.0.0.1\n") },
			warn:  true,
		},
		{
			name:  "a server line",
			setup: func(h *host) { h.file("/etc/dnsmasq.d/local.conf", "  server=10.0.0.2\n") },
		},
		{
			name: "another server in resolv.conf",
			setup: func(h *host) {
				h.file("/etc/dnsmasq.d/local.conf", "")
				h.file("/etc/resolv.conf", "nameserver 127.0.0.1\nnameserver 10.0.0.2\n")
			},
		},
		{
			// Another loopback address is not dnsmasq (the DNS of a Docker
			// network is 127.0.0.11).
			name: "another loopback server in resolv.conf",
			setup: func(h *host) {
				h.file("/etc/dnsmasq.d/local.conf", "")
				h.file("/etc/resolv.conf", "nameserver 127.0.0.1\nnameserver 127.0.0.11\n")
			},
		},
		{
			name: "resolv-file on the command line of the service",
			setup: func(h *host) {
				h.file("/etc/dnsmasq.d/local.conf", "")
				h.file("/proc/412/cmdline", "/usr/sbin/dnsmasq\x00--conf-file=/etc/dnsmasq.conf\x00--resolv-file=/etc/resolv.dnsmasq\x00")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eachShell(t, func(t *testing.T, h *host) {
				h.openrc()
				h.alpineDnsmasq()
				tt.setup(h)
				for run := range 2 {
					res := h.mustApply(forward(config.DNSMethodAuto, false))
					got := len(res.Warnings) == 1 && strings.Contains(res.Warnings[0], "dnsmasq has no DNS server for names outside the consul domain") &&
						strings.Contains(res.Warnings[0], "RESOLV_CONF=/etc/resolv.dnsmasq in /etc/udhcpc/udhcpc.conf")
					if got != tt.warn || len(res.Warnings) > 1 {
						t.Fatalf("run %d: warnings = %q, want the warning: %v", run+1, res.Warnings, tt.warn)
					}
				}
			})
		})
	}
}

// OpenRC not started (an image built in a container): dnsmasq in a
// runlevel counts as started, /etc/resolv.conf is not checked, and nothing
// is restarted. Without dnsmasq in a runlevel the node is refused.
func TestScriptOpenRCNotBooted(t *testing.T) {
	eachShell(t, func(t *testing.T, h *host) {
		h.openrc()
		h.alpineDnsmasq()
		for _, p := range []string{"run/openrc", "run/dnsmasq.pid"} {
			if err := os.RemoveAll(filepath.Join(h.root, p)); err != nil {
				t.Fatal(err)
			}
		}
		h.file("/etc/resolv.conf", "nameserver 10.0.0.2\n")
		if _, err := h.apply(forward(config.DNSMethodAuto, false)); err == nil || !strings.Contains(err.Error(), "dnsmasq inactive") {
			t.Fatalf("apply() without dnsmasq in a runlevel = %v", err)
		}
		h.assertUntouched()

		// rc-update add dnsmasq default: a link to the absolute path of the
		// service, which is outside the root of the test.
		if err := os.MkdirAll(filepath.Join(h.root, "etc/runlevels/default"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/etc/init.d/dnsmasq", filepath.Join(h.root, "etc/runlevels/default/dnsmasq")); err != nil {
			t.Fatal(err)
		}
		res := h.mustApply(forward(config.DNSMethodAuto, false))
		if res.Method != config.DNSMethodDnsmasq || len(res.Restarted) != 0 || h.read(DnsmasqConf) != wantDnsmasq {
			t.Fatalf("result = %+v", res)
		}
		if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "OpenRC was not started (an image built in a container?): dnsmasq is chosen by its runlevel") {
			t.Fatalf("warnings = %q", res.Warnings)
		}
		if len(h.restarts()) != 0 {
			t.Fatalf("restarts = %v", h.restarts())
		}
		h.assertNoSystemctl()
	})
}

// Turning the forwarding off on an OpenRC node removes the file and
// restarts dnsmasq with rc-service.
func TestScriptOpenRCDisable(t *testing.T) {
	eachShell(t, func(t *testing.T, h *host) {
		h.openrc()
		h.alpineDnsmasq()
		h.mustApply(forward(config.DNSMethodAuto, false))
		res := h.mustApply(&config.ConsulDNSForwarding{Enabled: false})
		if h.exists(DnsmasqConf) || !slices.Equal(res.Removed, []string{DnsmasqConf}) || !slices.Equal(res.Restarted, []string{"dnsmasq"}) {
			t.Fatalf("result = %+v", res)
		}
		if r := h.restarts(); !slices.Equal(r, []string{"dnsmasq", "dnsmasq"}) {
			t.Fatalf("restarts = %v", r)
		}
		h.assertNoSystemctl()
	})
}

func TestScriptOpenRCRestartFails(t *testing.T) {
	eachShell(t, func(t *testing.T, h *host) {
		h.openrc()
		h.alpineDnsmasq()
		h.flag("fail-restart-dnsmasq")
		_, err := h.apply(forward(config.DNSMethodAuto, false))
		if err == nil || !strings.Contains(err.Error(), "10.0.0.1 could not restart dnsmasq:  * ERROR: dnsmasq failed to start") {
			t.Fatalf("apply() = %v", err)
		}
	})
}

// The init system is a parameter the caller must give.
func TestScriptWithoutInit(t *testing.T) {
	eachShell(t, func(t *testing.T, h *host) {
		h.resolved()
		h.init = ""
		_, err := h.apply(forward(config.DNSMethodAuto, false))
		if err == nil || !strings.Contains(err.Error(), `10.0.0.1 was given the init system "" (NCD_INIT), not systemd or openrc`) {
			t.Fatalf("apply() = %v", err)
		}
		h.assertUntouched()
	})
}

package engine

import (
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/artifacts"
	"github.com/bacanoo-io/norca-deployer/internal/host"
	"github.com/bacanoo-io/norca-deployer/internal/platform"
	"github.com/bacanoo-io/norca-deployer/internal/platform/packages"
	"github.com/bacanoo-io/norca-deployer/internal/platform/service"
	"github.com/bacanoo-io/norca-deployer/internal/platform/tuning"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// alpineProbe is what the probe of the facts reads on an Alpine 3.22 node
// with OpenRC (busybox init), apk, doas and nftables, as root.
const alpineProbe = `NAME="Alpine Linux"
ID=alpine
VERSION_ID=3.22.8
PRETTY_NAME="Alpine Linux v3.22"
--
x86_64
--
init
/run/openrc
--
apk
doas
nft
iptables
rc-service
--
--
cgroup2fs
--
0
`

// alpinePlan is a plan of one server with an apk block (a tagged repository
// with its key), the tuning and the firewall.
const alpinePlan = `
metadata: {name: prod, region: eu}
release: {version: v0.3.0, download_base_url: https://mirror.example/norca, checksum_sha256: ` + sumRelease + `}
tuning: {apply_sysctl: true, apply_firewall_rules: true}
host_packages:
  apk:
    repositories:
      - name: mine
        url: https://apk.example.com/v3.22/main
        key_name: packager-6b0763be.rsa.pub
        key: |
          -----BEGIN PUBLIC KEY-----
          MIICIjANBgkqhkiG9w0BAQEFAAOCAg8AMIICCgKCAgEAutQkua2CAig4VFSJ7v54
          ALyu/J1WB3oni7qwCZD3veURw7HxpNAj9hR+S5N/pNeZgubQvJWyaPuQDm7PTs1+
          tFGiYNfAsiibX6Rv0wci3M+z2XEVAeR9Vzg6v4qoofDyoTbovn2LztaNEjTkB+oK
          tlvpNhg1zhou0jDVYFniEXvzjckxswHVb8cT0OMTKHALyLPrPOJzVtM9C1ew2Nnc
          3848xLiApMu3NBk0JqfcS3Bo5Y2b1FRVBvdt+2gFoKZix1MnZdAEZ8xQzL/a0YS5
          Hd0wj5+EEKHfOd3A75uPa/WQmA+o0cBFfrzm69QDcSJSwGpzWrD1ScH3AK8nWvoj
          v7e9gukK/9yl1b4fQQ00vttwJPSgm9EnfPHLAtgXkRloI27H6/PuLoNvSAMQwuCD
          hQRlyGLPBETKkHeodfLoULjhDi1K2gKJTMhtbnUcAA7nEphkMhPWkBpgFdrH+5z4
          Lxy+3ek0cqcI7K68EtrffU8jtUj9LFTUC8dERaIBs7NgQ/LfDbDfGh9g6qVj1hZl
          k9aaIPTm/xsi8v3u+0qaq7KzIBc9s59JOoA8TlpOaYdVgSQhHHLBaahOuAigH+VI
          isbC9vmqsThF2QdDtQt37keuqoda2E6sL7PUvIyVXDRfwX7uMDjlzTxHTymvq2Ck
          htBqojBnThmjJQFgZXocHG8CAwEAAQ==
          -----END PUBLIC KEY-----
        tagged: true
    install:
      - {name: jq, hold: true}
      - {name: norca-tools, version: 1.0.0-r0, repository: mine}
inventory:
  masters:
    - {host: 10.0.0.1, hostname: s1}
`

// The drivers of an Alpine node: apk, OpenRC and its tuning; the musl build
// of the release.
func TestDriversAlpine(t *testing.T) {
	f, err := host.Parse(alpineProbe)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Check(); err != nil {
		t.Fatal(err)
	}
	d, err := platform.For(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.Packages.(packages.Apk); !ok {
		t.Errorf("packages %T", d.Packages)
	}
	if _, ok := d.Service.(service.OpenRC); !ok {
		t.Errorf("service %T", d.Service)
	}
	if _, ok := d.Tuning.(tuning.OpenRC); !ok {
		t.Errorf("tuning %T", d.Tuning)
	}
	if libc := (&target{facts: f}).libc(); libc != artifacts.LibcMusl {
		t.Errorf("libc %q", libc)
	}
	debian, _ := host.Parse(debianProbe)
	for _, tg := range []*target{{facts: debian}, {}} {
		if libc := tg.libc(); libc != artifacts.LibcGlibc {
			t.Errorf("libc of a node without musl: %q", libc)
		}
	}
}

// prepareNode on an Alpine 3.22 node: the steps of apk, the OpenRC service
// and tuning, the nftables table loaded at boot by OpenRC; nothing of apt,
// systemd or sysctl --system. A second run changes nothing.
func TestPrepareNodeAlpine(t *testing.T) {
	transcript, _ := recordPlanWith(t, loadPlan(t, alpinePlan), alpineProbe)
	first, second, found := strings.Cut(transcript, "run 2:")
	if !found {
		t.Fatalf("no second run:\n%s", transcript)
	}
	for _, want := range []string{
		"run 1: changed true, error <nil>",
		"> write /etc/apk/repositories.d/norca.list 0644",
		"> write /etc/apk/keys/packager-6b0763be.rsa.pub 0644",
		"apk --wait 300 update >/dev/null",
		"apk --wait 300 add -q -U 'jq' 'norca-tools@norca-mine=1.0.0-r0'",
		"apk --wait 300 add -q \"jq=$(apk_version 'jq')\" 'norca-tools@norca-mine'",
		"> write /etc/init.d/norca 0755",
		"rc-update add norca default",
		"sysctl -p /etc/sysctl.d/99-norca-production.conf",
		"# apply: openrc services",
		"> write /etc/init.d/norca-nftables 0755",
		"rc-update add norca-nftables boot",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("the first run has no %q", want)
		}
	}
	// The probe names the tools it looks for: it is left out.
	var rest []string
	for line := range strings.SplitSeq(transcript, "\n") {
		if !strings.Contains(line, "for t in apt-get dnf") {
			rest = append(rest, line)
		}
	}
	// The firewall asks whether systemd runs: that is no systemctl action.
	for _, unwanted := range []string{"apt-get", "dpkg-query", "dnf ", "systemctl enable", "systemctl restart", "daemon-reload",
		"/etc/systemd/system", "sysctl --system", "restorecon"} {
		if strings.Contains(strings.Join(rest, "\n"), unwanted) {
			t.Errorf("an Alpine node runs or writes %q:\n%s", unwanted, strings.Join(rest, "\n"))
		}
	}
	if !strings.HasPrefix(second, " changed false, error <nil>") {
		t.Fatalf("the second run changes something:\n%s", second)
	}
}

// The facts checks of an Alpine node: the apk block, and consul DNS
// forwarding, which needs systemd.
func TestCheckFactsAlpine(t *testing.T) {
	apt := &config.AptPackages{Install: []config.HostPackage{{Name: "jq"}}}
	apk := &config.ApkPackages{Install: []config.HostPackage{{Name: "jq"}}}
	dns := &config.ConsulConfig{DNSForwarding: &config.ConsulDNSForwarding{Enabled: true}}
	tests := []struct {
		name   string
		plan   *config.DeploymentPlan
		probe  string
		errors []string
	}{
		{name: "alpine with its block", plan: &config.DeploymentPlan{HostPackages: &config.HostPackages{Apt: apt, Apk: apk}}, probe: alpineProbe},
		{name: "alpine without its block", plan: &config.DeploymentPlan{HostPackages: &config.HostPackages{Apt: apt}}, probe: alpineProbe,
			errors: []string{"node [10.0.0.5]: the node uses apk, but host_packages has packages for its roles only in apt"}},
		{name: "debian without its block", plan: &config.DeploymentPlan{HostPackages: &config.HostPackages{Apk: apk}}, probe: debianProbe,
			errors: []string{"the node uses apt, but host_packages has packages for its roles only in apk"}},
		{name: "dns forwarding on alpine", plan: &config.DeploymentPlan{Consul: dns}, probe: alpineProbe,
			errors: []string{"consul.dns_forwarding needs systemd", "the node runs OpenRC"}},
		{name: "dns forwarding on debian", plan: &config.DeploymentPlan{Consul: dns}, probe: debianProbe},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := host.Parse(tt.probe)
			if err != nil {
				t.Fatal(err)
			}
			client := &target{node: &config.NodeConfig{Host: "10.0.0.5", Platform: "linux"}, facts: f}
			err = NewRunner(tt.plan, nil).checkFacts([]*target{client})
			if len(tt.errors) == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			for _, e := range tt.errors {
				if err == nil || !strings.Contains(err.Error(), e) {
					t.Fatalf("got %v, want %q", err, e)
				}
			}
		})
	}
}

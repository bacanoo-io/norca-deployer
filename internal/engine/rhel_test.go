package engine

import (
	"strings"
	"testing"
)

// rhelPlan is a plan of one server with a dnf block, the tuning and the
// firewall.
const rhelPlan = `
metadata: {name: prod, region: eu}
release: {version: v0.3.0, download_base_url: https://mirror.example/norca, checksum_sha256: ` + sumRelease + `}
tuning: {apply_sysctl: true, apply_firewall_rules: true}
host_packages:
  dnf:
    repositories:
      - name: docker
        baseurl: https://download.docker.com/linux/rhel/$releasever/$basearch/stable
        key: |
          -----BEGIN PGP PUBLIC KEY BLOCK-----
          mQINBFit2ioBEADhWpZ8/wvZ6hUTiXOwQHXMAlaFHcPH9hAtr4F1y2+OYdbtMuth
          -----END PGP PUBLIC KEY BLOCK-----
        priority: 10
    install:
      - {name: jq, hold: true}
inventory:
  masters:
    - {host: 10.0.0.1, hostname: s1}
`

// prepareNode on a Rocky Linux 9 node under SELinux enforcing: the steps of
// dnf, and the SELinux labels restored after a change only.
func TestPrepareNodeRHEL(t *testing.T) {
	transcript, _ := recordPlanWith(t, loadPlan(t, rhelPlan), rhelProbe)
	first, second, found := strings.Cut(transcript, "run 2:")
	if !found {
		t.Fatalf("no second run:\n%s", transcript)
	}
	for _, want := range []string{
		"run 1: changed true, error <nil>",
		"> write /etc/yum.repos.d/norca-docker.repo 0644",
		"> write /etc/pki/rpm-gpg/norca-docker.asc 0644",
		"dnf -q makecache --repo='norca-docker'",
		"dnf -y -q --setopt=install_weak_deps=False --refresh install 'jq'",
		"dnf_lock 'jq'",
		"# apply: SELinux labels",
		"restorecon -R \"$p\"",
		"'/usr/local/bin/norca' '/etc/systemd/system/norca.service'",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("the first run has no %q", want)
		}
	}
	for _, unwanted := range []string{"apt-get install", "apt-get update", "dpkg-query", "apt-mark"} {
		if strings.Contains(transcript, unwanted) {
			t.Errorf("a RHEL node runs %q", unwanted)
		}
	}
	if !strings.HasPrefix(second, " changed false, error <nil>") || strings.Contains(second, "SELinux labels") {
		t.Fatalf("the second run changes something or restores labels:\n%s", second)
	}
}

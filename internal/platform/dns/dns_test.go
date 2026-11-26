package dns

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

func TestScript(t *testing.T) {
	s := Script()
	if !strings.HasPrefix(s, "#!/bin/sh\n") || !strings.HasSuffix(s, "\nnorca_consul_dns\n") {
		t.Fatalf("Script() is not the functions and their call:\n%s", s)
	}
	if !strings.Contains(Functions(), "norca_consul_dns() {") {
		t.Fatal("Functions() has no norca_consul_dns")
	}
	// No bash features in the POSIX script (the dash and busybox runs of
	// consul_dns_script_test.go are the real check).
	for i, line := range strings.Split(Functions(), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		for _, bashism := range []string{"[[ ", "pipefail", "$'", "<(", "source ", "declare ", "${!"} {
			if strings.Contains(line, bashism) {
				t.Errorf("line %d has %q: %s", i+1, bashism, line)
			}
		}
		for _, keyword := range []string{"local ", "function "} {
			if strings.HasPrefix(line, keyword) {
				t.Errorf("line %d starts with %q: %s", i+1, keyword, line)
			}
		}
	}
}

func TestVars(t *testing.T) {
	tests := []struct {
		name string
		f    *config.ConsulDNSForwarding
		init string
		want string
	}{
		{
			name: "disabled",
			f:    nil,
			init: config.InitSystemd,
			want: "NCD_ENABLED='0' NCD_DOMAIN='consul' NCD_RESOLVED_DNS='127.0.0.1:8600' NCD_DNSMASQ_SERVER='127.0.0.1#8600' NCD_PORT='8600' NCD_METHOD='auto' NCD_DOCKER='0' NCD_INIT='systemd' NCD_ROOT=''",
		},
		{
			name: "enabled with docker",
			f:    &config.ConsulDNSForwarding{Enabled: true, Address: "127.0.0.1:18600", Domain: "dc1.example", Method: config.DNSMethodResolved, Docker: true},
			init: config.InitSystemd,
			want: "NCD_ENABLED='1' NCD_DOMAIN='dc1.example' NCD_RESOLVED_DNS='127.0.0.1:18600' NCD_DNSMASQ_SERVER='127.0.0.1#18600' NCD_PORT='18600' NCD_METHOD='systemd-resolved' NCD_DOCKER='1' NCD_INIT='systemd' NCD_ROOT=''",
		},
		{
			name: "ipv6 on openrc",
			f:    &config.ConsulDNSForwarding{Enabled: true, Address: "[::1]:8600", Domain: "consul", Method: config.DNSMethodDnsmasq},
			init: config.InitOpenRC,
			want: "NCD_ENABLED='1' NCD_DOMAIN='consul' NCD_RESOLVED_DNS='[::1]:8600' NCD_DNSMASQ_SERVER='::1#8600' NCD_PORT='8600' NCD_METHOD='dnsmasq' NCD_DOCKER='0' NCD_INIT='openrc' NCD_ROOT=''",
		},
		{
			name: "init from the environment (the install script)",
			f:    &config.ConsulDNSForwarding{Enabled: true},
			want: "NCD_ENABLED='1' NCD_DOMAIN='consul' NCD_RESOLVED_DNS='127.0.0.1:8600' NCD_DNSMASQ_SERVER='127.0.0.1#8600' NCD_PORT='8600' NCD_METHOD='auto' NCD_DOCKER='0' NCD_ROOT=''",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vs, err := Vars(tt.f, tt.init)
			if err != nil {
				t.Fatal(err)
			}
			if got := Assignments(vs, " "); got != tt.want {
				t.Fatalf("Assignments() =\n %s\nwant\n %s", got, tt.want)
			}
		})
	}
	if _, err := Vars(&config.ConsulDNSForwarding{Enabled: true, Address: "localhost:8600"}, config.InitSystemd); err == nil {
		t.Fatal("Vars() accepted a host name")
	}
	if _, err := Vars(&config.ConsulDNSForwarding{Enabled: true}, "windows"); err == nil ||
		err.Error() != `consul.dns_forwarding: the init system "windows" is not systemd or openrc` {
		t.Fatalf("Vars() of windows = %v", err)
	}
}

func TestParse(t *testing.T) {
	out := "method systemd-resolved\nchanged /etc/dnssec-trust-anchors.d/norca-consul.negative\n" +
		"unchanged /etc/systemd/resolved.conf.d/norca-consul.conf\nrestarted systemd-resolved\n" +
		"bridge 172.17.0.1/16\nwarn something\nsudo: noise of another program\nremoved /etc/dnsmasq.d/norca-consul.conf\n"
	got := Parse(out)
	if got.Method != "systemd-resolved" ||
		!reflect.DeepEqual(got.Changed, []string{TrustAnchor}) ||
		!reflect.DeepEqual(got.Unchanged, []string{ResolvedDropIn}) ||
		!reflect.DeepEqual(got.Removed, []string{DnsmasqConf}) ||
		!reflect.DeepEqual(got.Restarted, []string{"systemd-resolved"}) ||
		!reflect.DeepEqual(got.Warnings, []string{"something"}) ||
		got.Bridge.String() != "172.17.0.1/16" || got.Bridge.Masked().String() != "172.17.0.0/16" ||
		len(got.Errors) != 0 {
		t.Fatalf("Parse() = %+v", got)
	}
	if s := got.Summary(); s != "systemd-resolved; written: "+TrustAnchor+"; removed: "+DnsmasqConf+"; restarted: systemd-resolved" {
		t.Fatalf("Summary() = %q", s)
	}
	if s := Parse("method dnsmasq\nunchanged " + DnsmasqConf + "\n").Summary(); s != "dnsmasq; unchanged" {
		t.Fatalf("Summary() = %q", s)
	}
}

// cannedNode answers the commands of Apply with fixed outputs.
func cannedNode(exists bool, out string, err error) *connector.FakeConnector {
	f := connector.NewFakeConnector()
	f.Handler = func(cmd string) (string, error, bool) {
		switch {
		case strings.HasPrefix(cmd, "# when: "+StepName+"\n"):
			if exists {
				return "norca-step yes\n", nil, true
			}
			return "norca-step no\n", nil, true
		case strings.Contains(cmd, " sh "+ScriptPath):
			return out, err, true
		}
		return "", nil, true
	}
	return f
}

func TestApplyUploadsAndRuns(t *testing.T) {
	conn := cannedNode(false, "method dnsmasq\nchanged "+DnsmasqConf+"\nrestarted dnsmasq\n", nil)
	res, err := Apply(t.Context(), conn, "10.0.0.1", config.InitSystemd, &config.ConsulDNSForwarding{Enabled: true, Address: "127.0.0.1:8600", Domain: "consul", Method: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	if string(conn.Files[ScriptPath]) != Script() || conn.Modes[ScriptPath] != 0o700 {
		t.Fatalf("the script was not uploaded to %s", ScriptPath)
	}
	runs := conn.CommandsWithPrefix("# apply: " + StepName + "\n")
	if len(runs) != 1 || !strings.Contains(runs[0], "\nNCD_ENABLED='1' ") ||
		!strings.Contains(runs[0], " NCD_INIT='systemd' ") || !strings.HasSuffix(runs[0], " sh "+ScriptPath) {
		t.Fatalf("commands = %q, want one run of the script", conn.Commands)
	}
	if res.Method != "dnsmasq" || len(res.Restarted) != 1 {
		t.Fatalf("result = %+v", res)
	}
}

func TestApplyErrors(t *testing.T) {
	fail := errors.New("exit status 1")
	tests := []struct {
		name string
		out  string
		err  error
		want string
	}{
		{
			name: "error line",
			out:  "error has no local resolver the deployer can forward the consul domain from: systemd-resolved inactive\n",
			err:  fail,
			want: "consul.dns_forwarding: 10.0.0.1 has no local resolver the deployer can forward the consul domain from: systemd-resolved inactive",
		},
		{
			name: "failure without an error line",
			out:  "sh: 1: cannot open /var/lib/norca-deployer/consul-dns.sh\n",
			err:  fail,
			want: "consul.dns_forwarding failed: exit status 1 (output: sh: 1: cannot open",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Apply(t.Context(), cannedNode(false, tt.out, tt.err), "10.0.0.1", config.InitSystemd, &config.ConsulDNSForwarding{Enabled: true})
			if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
				t.Fatalf("Apply() = %v, want %q", err, tt.want)
			}
		})
	}
}

// A node that never had the forwarding gets no upload when it is off.
func TestApplyDisabledWithoutFiles(t *testing.T) {
	for _, f := range []*config.ConsulDNSForwarding{nil, {Enabled: false}} {
		conn := cannedNode(false, "", nil)
		res, err := Apply(t.Context(), conn, "10.0.0.1", config.InitOpenRC, f)
		if err != nil {
			t.Fatal(err)
		}
		if len(conn.Files) != 0 || len(conn.Commands) != 1 || res.Method != "" {
			t.Fatalf("files %v, commands %q, want only the look for files", conn.Files, conn.Commands)
		}
	}
}

func TestApplyDisabledRemoves(t *testing.T) {
	conn := cannedNode(true, "removed "+ResolvedDropIn+"\nrestarted systemd-resolved\n", nil)
	res, err := Apply(t.Context(), conn, "10.0.0.1", config.InitSystemd, nil)
	if err != nil {
		t.Fatal(err)
	}
	runs := conn.CommandsWithPrefix("# apply: " + StepName + "\n")
	if len(runs) != 1 || !strings.Contains(runs[0], "\nNCD_ENABLED='0' ") || !strings.HasSuffix(runs[0], " sh "+ScriptPath+"\nrm -f "+ScriptPath) {
		t.Fatalf("commands = %q, want the script run with NCD_ENABLED=0 and then removed", conn.Commands)
	}
	if len(res.Removed) != 1 || res.Restarted[0] != "systemd-resolved" {
		t.Fatalf("result = %+v", res)
	}
}

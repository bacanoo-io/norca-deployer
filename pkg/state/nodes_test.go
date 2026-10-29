package state

import (
	"os"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/pkg/config"
	"github.com/bacanoo-io/norca-deployer/pkg/crypto"
)

func TestInstalledRoundTripEncrypted(t *testing.T) {
	_, key, err := crypto.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	vault, err := crypto.NewVault(key)
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{Dir: t.TempDir(), vault: vault}

	inst, err := s.LoadInstalled("eu")
	if err != nil || len(inst.Nodes) != 0 {
		t.Fatalf("new list: %v %v", inst, err)
	}
	node := config.NodeConfig{Host: "10.0.0.5", Hostname: "w5", Connection: config.ConnectionConfig{SSHUser: "root", SSHPassword: "secret"}}
	inst.Record(&node, config.RoleClient, "v0.2.0")
	first := inst.Nodes["w5"].InstalledAt
	if err := s.SaveInstalled(inst); err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(s.Path("nodes-eu.json"))
	if strings.Contains(string(raw), "secret") || !strings.HasPrefix(string(raw), crypto.EncryptedPrefix) {
		t.Fatalf("the list must be encrypted with a master key: %s", raw)
	}

	again, err := s.LoadInstalled("eu")
	if err != nil {
		t.Fatal(err)
	}
	again.Record(&node, config.RoleClient, "v0.3.0")
	e := again.Nodes["w5"]
	if e.Role != "client" || e.Release != "v0.3.0" || !e.InstalledAt.Equal(first) || e.Node.Connection.SSHPassword != "secret" {
		t.Fatalf("entry after reload: %+v", e)
	}
	if key, found := again.Lookup("10.0.0.5"); key != "w5" || found == nil {
		t.Fatal("lookup by host failed")
	}
	if other, _ := s.LoadInstalled("us"); len(other.Nodes) != 0 {
		t.Fatal("regions must not share the list")
	}
}

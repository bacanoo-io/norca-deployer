//go:build equivalence

package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The equivalence check of the host platform work: what prepareNode does on
// a node (the commands it runs and the files it writes, in order) is
// recorded for a set of plans, so that two versions of the deployer can be
// compared. NORCA_EQUIV_PLANS names the plans (separated by the path list
// separator of the OS), NORCA_EQUIV_OUT the directory of the transcripts:
// <plan>.txt with every node of the plan, provisioned twice on the same fake
// host (a fresh node, then the same node again; see equivHost).
//
// The test is behind the build tag equivalence: its plans come from the
// environment, which the taint analysis of gosec follows into the file
// paths of the plans that prepareNode opens.
//
//	NORCA_EQUIV_OUT=<dir> NORCA_EQUIV_PLANS=<plans> go test -tags equivalence -run TestRecordPrepareNode ./internal/engine/
func TestRecordPrepareNode(t *testing.T) {
	out := os.Getenv("NORCA_EQUIV_OUT")
	if out == "" {
		t.Skip("NORCA_EQUIV_OUT is not set")
	}
	for _, file := range filepath.SplitList(os.Getenv("NORCA_EQUIV_PLANS")) {
		name := strings.TrimSuffix(filepath.Base(filepath.Dir(file)), string(filepath.Separator)) + "-" +
			strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
		t.Run(name, func(t *testing.T) {
			transcript, _ := recordPlan(t, file)
			if err := os.WriteFile(filepath.Join(out, name+".txt"), []byte(transcript), 0o600); err != nil {
				t.Fatal(err)
			}
		})
	}
}

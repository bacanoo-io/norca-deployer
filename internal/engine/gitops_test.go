// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

const gitopsStackHCL = "rigger {\n  name      = \"platform\"\n  namespace = \"platform\"\n  region    = \"eu\"\n}\n"

type pgpSigner struct{ entity *openpgp.Entity }

func (s pgpSigner) Sign(_ context.Context, message io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&buf, s.entity, message, nil); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// signedStackRepo is a repository with a stack at stacks/platform, in a
// commit signed with a key. It returns its URL, the armored public key and
// the commit.
func signedStackRepo(t *testing.T) (url, publicKey, commit string) {
	t.Helper()
	key, err := openpgp.NewEntity("release", "", "release@example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	var armored bytes.Buffer
	w, err := armor.Encode(&armored, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := key.Serialize(w); err != nil {
		t.Fatal(err)
	}
	w.Close()

	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"stacks/platform/stack.hcl":            gitopsStackHCL,
		"stacks/platform/values/prod.vars.hcl": "replicas = 1\n",
		"stacks/platform/.env":                 "TOKEN=secret",
		"README.md":                            "not of the stack",
	} {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := tree.Add(name); err != nil {
			t.Fatal(err)
		}
	}
	h, err := tree.Commit("the stack", &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()},
		Signer: pgpSigner{key},
	})
	if err != nil {
		t.Fatal(err)
	}
	return "file://" + filepath.ToSlash(dir), armored.String(), h.String()
}

// gitopsPlan is a plan with one binding of the stack of a repository.
func gitopsPlan(t *testing.T, url, key, policy string) string {
	t.Helper()
	binding := "      - name: platform\n        namespace: platform\n        region: eu\n" +
		"        forgejo:\n          url: " + url + "\n          subpath: stacks/platform\n" +
		"        values:\n          replicas: 3\n          password: \"!generate:gitops-test\"\n" + policy
	if key != "" {
		binding += "        verify:\n          public_keys:\n            - |\n"
		for line := range strings.SplitSeq(strings.TrimSpace(key), "\n") {
			binding += "              " + line + "\n"
		}
	}
	return bootstrapPlan + "\nrigger:\n  gitops:\n    enabled: true\n    bindings:\n" + binding
}

const (
	gitopsPlanAnswer = `{"Plan":{"stack":"platform","region":"eu","releases":[` +
		`{"release":{"namespace":"platform","name":"cache"},"action":"install","module":{"name":"redis","version":"0.4.1"}}],"outputs":[]},"Warnings":null}`
	gitopsOperation = "platform-20261125-120000-a847"
)

// sentStack is the body of a plan or an apply of the stacks API.
type sentStack struct {
	Files      []byte
	ValuesJSON map[string]any
	Prune      bool
	Origin     map[string]string
}

func sentTo(t *testing.T, api *apiRecorder, call string) sentStack {
	t.Helper()
	var sent sentStack
	if err := json.Unmarshal([]byte(api.bodies[call]), &sent); err != nil {
		t.Fatalf("the body of %s: %v", call, err)
	}
	return sent
}

func archiveNames(t *testing.T, archive []byte) []string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
}

// apply runs a binding through the CLI of the first server: the servers
// plan the stack, apply it as an operation, and the deployer follows it.
func TestDeployGitOpsAppliesAStack(t *testing.T) {
	url, key, commit := signedStackRepo(t)
	r, first, api := bootstrapRunner(t, gitopsPlan(t, url, key, ""))
	api.answers["PUT /v1/rigger/stack/plan"] = "< HTTP/1.1 200 OK\n" + gitopsPlanAnswer
	api.answers["PUT /v1/rigger/stack/apply"] = "< HTTP/1.1 200 OK\n" +
		`{"namespace":"platform","name":"` + gitopsOperation + `","spec":{"stack":"platform","action":"apply"},"status":{"phase":"Running"}}`
	api.answers["GET /v1/rigger/operation/"+gitopsOperation+"/log"] = "< HTTP/1.1 200 OK\n" +
		`{"Lines":[{"Seq":1,"Line":{"level":"INFO","msg":"done","release":"cache"}}],"Phase":"Succeeded","Finished":true}`
	api.answers["GET /v1/object/rigger.norca.io/Operation/"+gitopsOperation] = "< HTTP/1.1 200 OK\n" +
		`{"Name":"` + gitopsOperation + `","Status":{"phase":"Succeeded","outcomes":[{"namespace":"platform","release":"cache","action":"install","revision":1}]}}`

	if err := r.deployGitOps(first, "token"); err != nil {
		t.Fatal(err)
	}

	// Every call goes to the namespace and the region of the binding.
	want := []string{
		"PUT /v1/rigger/stack/plan?namespace=platform&region=eu",
		"PUT /v1/rigger/stack/apply?namespace=platform&region=eu",
		"GET /v1/rigger/operation/" + gitopsOperation + "/log?namespace=platform&region=eu&wait=10s",
		"GET /v1/object/rigger.norca.io/Operation/" + gitopsOperation + "?namespace=platform&region=eu",
	}
	if !slices.Equal(api.calls, want) {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(api.calls, "\n"), strings.Join(want, "\n"))
	}

	for _, call := range want[:2] {
		sent := sentTo(t, api, call)
		// The files of the stack directory, without those whose name
		// starts with a dot and without the rest of the repository.
		if names := archiveNames(t, sent.Files); !slices.Equal(names, []string{".rigger-stack.json", "stack.hcl", "values/prod.vars.hcl"}) {
			t.Errorf("%s: the archive has %v", call, names)
		}
		// The values of the binding, the generated one resolved.
		password, _ := sent.ValuesJSON["password"].(string)
		if sent.ValuesJSON["replicas"] != float64(3) || len(password) < 20 || strings.HasPrefix(password, "!") {
			t.Errorf("%s: values %v", call, sent.ValuesJSON)
		}
		if sent.Prune {
			t.Errorf("%s: prune is off by default", call)
		}
		origin := map[string]string{"repository": url, "commit": commit, "signer": "release <release@example.com>", "ref": sent.Origin["ref"]}
		if !maps.Equal(sent.Origin, origin) || sent.Origin["ref"] == "" {
			t.Errorf("%s: origin %v, want %v", call, sent.Origin, origin)
		}
	}

	// What is in sync the servers say: the deployer keeps no marker.
	if _, err := os.Stat(r.store.Path("gitops")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the state has a gitops directory: %v", err)
	}
}

func TestDeployGitOpsLeavesAStackInSyncAlone(t *testing.T) {
	url, key, _ := signedStackRepo(t)
	r, first, api := bootstrapRunner(t, gitopsPlan(t, url, key, ""))
	api.answers["PUT /v1/rigger/stack/plan"] = "< HTTP/1.1 200 OK\n" + strings.Replace(gitopsPlanAnswer, `"install"`, `"unchanged"`, 1)

	if err := r.deployGitOps(first, "token"); err != nil {
		t.Fatal(err)
	}
	if len(api.calls) != 1 || !strings.HasPrefix(api.calls[0], "PUT /v1/rigger/stack/plan?") {
		t.Fatalf("calls %v", api.calls)
	}
}

func TestDeployGitOpsOnlyPlans(t *testing.T) {
	url, _, _ := signedStackRepo(t)
	r, first, api := bootstrapRunner(t, gitopsPlan(t, url, "", "        policy:\n          mode: plan-only\n          prune: true\n"))
	api.answers["PUT /v1/rigger/stack/plan"] = "< HTTP/1.1 200 OK\n" + gitopsPlanAnswer

	if err := r.deployGitOps(first, "token"); err != nil {
		t.Fatal(err)
	}
	if len(api.calls) != 1 || !strings.HasPrefix(api.calls[0], "PUT /v1/rigger/stack/plan?") {
		t.Fatalf("calls %v", api.calls)
	}
	if sent := sentTo(t, api, api.calls[0]); !sent.Prune || sent.Origin["signer"] != "" {
		t.Errorf("sent %+v", sent)
	}
}

// The error of a run names the binding, the stack, the commit and the
// operation, and what the operation failed with.
func TestDeployGitOpsFailedOperation(t *testing.T) {
	url, key, commit := signedStackRepo(t)
	r, first, api := bootstrapRunner(t, gitopsPlan(t, url, key, ""))
	api.answers["PUT /v1/rigger/stack/plan"] = "< HTTP/1.1 200 OK\n" + gitopsPlanAnswer
	api.answers["PUT /v1/rigger/stack/apply"] = "< HTTP/1.1 200 OK\n" + `{"namespace":"platform","name":"` + gitopsOperation + `","status":{"phase":"Running"}}`
	api.answers["GET /v1/rigger/operation/"+gitopsOperation+"/log"] = "< HTTP/1.1 200 OK\n" + `{"Lines":null,"Phase":"Failed","Finished":true}`
	api.answers["GET /v1/object/rigger.norca.io/Operation/"+gitopsOperation] = "< HTTP/1.1 200 OK\n" +
		`{"Name":"` + gitopsOperation + `","Status":{"phase":"Failed","error":"release cache: no node fits the job"}}`

	err := r.deployGitOps(first, "token")
	if err == nil {
		t.Fatal("must fail")
	}
	for _, part := range []string{"gitops binding platform", "stack platform", "commit " + commit[:12], "operation " + gitopsOperation, "failed (release cache: no node fits the job)"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("%q is not in: %v", part, err)
		}
	}
}

func TestDeployGitOpsNeedsATokenAndALinuxServer(t *testing.T) {
	url, key, _ := signedStackRepo(t)

	// With ACLs and no management token nothing is run.
	r, first, api := bootstrapRunner(t, gitopsPlan(t, url, key, ""))
	if err := r.deployGitOps(first, ""); err != nil {
		t.Fatal(err)
	}
	if len(api.calls) != 0 {
		t.Fatalf("calls %v", api.calls)
	}

	first.node.Platform = "windows"
	err := r.deployGitOps(first, "token")
	if err == nil || !strings.Contains(err.Error(), "Windows") || len(api.calls) != 0 {
		t.Fatalf("got %v, calls %v", err, api.calls)
	}
}

// A rigger section with bindings only has no stack of the plan to apply.
func TestDeployRiggerWithBindingsOnly(t *testing.T) {
	url, key, _ := signedStackRepo(t)
	r, first, api := bootstrapRunner(t, gitopsPlan(t, url, key, ""))
	server := newRiggerServer(t, first)

	if err := r.deployRigger(first, "token"); err != nil {
		t.Fatal(err)
	}
	if len(server.runs) != 0 || len(api.calls) != 0 {
		t.Fatalf("runs %v calls %v", server.runs, api.calls)
	}
}

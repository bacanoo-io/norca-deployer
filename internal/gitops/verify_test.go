package gitops

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"

	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

type pgpSigner struct{ entity *openpgp.Entity }

func (s pgpSigner) Sign(_ context.Context, message io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&buf, s.entity, message, nil); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func newKey(t *testing.T, name string) (*openpgp.Entity, string) {
	t.Helper()
	e, err := openpgp.NewEntity(name, "", name+"@example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Serialize(w); err != nil {
		t.Fatal(err)
	}
	w.Close()
	return e, buf.String()
}

type signedRepo struct {
	dir                     string
	signed, unsigned, other string
}

// newSignedRepo has a commit signed by release (tags signed-tag, plain-tag and
// tag-of-signed), an unsigned one (tags unsigned-tag and annotated-signed), and
// a commit signed by another key at the head of the branch.
func newSignedRepo(t *testing.T, release, stranger *openpgp.Entity) *signedRepo {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	w, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	who := &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()}
	commit := func(n string, signer *openpgp.Entity) plumbing.Hash {
		if err := os.WriteFile(filepath.Join(dir, "stack.hcl"), []byte(stackAt(n)), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Add("stack.hcl"); err != nil {
			t.Fatal(err)
		}
		opts := &git.CommitOptions{Author: who}
		if signer != nil {
			opts.Signer = pgpSigner{signer}
		}
		h, err := w.Commit(n, opts)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	tag := func(name string, h plumbing.Hash, signer *openpgp.Entity) {
		var opts *git.CreateTagOptions
		if signer != nil {
			opts = &git.CreateTagOptions{Tagger: who, Message: name, Signer: pgpSigner{signer}}
		}
		if _, err := repo.CreateTag(name, h, opts); err != nil {
			t.Fatal(err)
		}
	}

	signed := commit("1.0.0", release)
	tag("plain-tag", signed, nil)
	unsigned := commit("2.0.0", nil)
	tag("unsigned-tag", unsigned, nil)
	tag("annotated-signed", unsigned, release)
	tag("annotated-stranger", unsigned, stranger)
	other := commit("3.0.0", stranger)
	return &signedRepo{dir: dir, signed: signed.String(), unsigned: unsigned.String(), other: other.String()}
}

func TestVerify(t *testing.T) {
	release, releaseKey := newKey(t, "release")
	stranger, _ := newKey(t, "stranger")
	_, otherKey := newKey(t, "unrelated")
	r := newSignedRepo(t, release, stranger)
	url := "file://" + filepath.ToSlash(r.dir)
	branch := branchOf(t, r.dir)
	keys := []string{otherKey, releaseKey}

	for name, tc := range map[string]struct {
		opts   FetchOptions
		signer string // empty: refused
	}{
		"signed commit":            {FetchOptions{Branch: branch, CommitSHA: r.signed}, "commit signed by release"},
		"unsigned commit":          {FetchOptions{Branch: branch, CommitSHA: r.unsigned}, ""},
		"commit of another key":    {FetchOptions{Branch: branch}, ""},
		"signed annotated tag":     {FetchOptions{Tag: "annotated-signed"}, "tag annotated-signed signed by release"},
		"annotated tag by another": {FetchOptions{Tag: "annotated-stranger"}, ""},
		"light tag, signed commit": {FetchOptions{Tag: "plain-tag"}, "commit signed by release"},
		"light tag, unsigned":      {FetchOptions{Tag: "unsigned-tag"}, ""},
		"ref to a signed tag":      {FetchOptions{Ref: "annotated-signed"}, "tag annotated-signed signed by release"},
	} {
		t.Run(name, func(t *testing.T) {
			opts := tc.opts
			opts.URL, opts.VerifyKeys = url, keys
			res, err := FetchStackInMemory(context.Background(), &opts)
			if tc.signer == "" {
				if err == nil {
					t.Fatalf("must be refused, got %s", res.Signature())
				}
				if !strings.Contains(err.Error(), "signature") {
					t.Fatalf("got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(res.Signature(), tc.signer) {
				t.Fatalf("signature %q", res.Signature())
			}
			// Who signed is the name of the key alone: it is what the
			// servers record as the signer of the origin.
			if res.Signer != "release <release@example.com>" {
				t.Fatalf("signer %q", res.Signer)
			}
		})
	}

	// The fetched commit is the one of the tag.
	res, err := FetchStackInMemory(context.Background(), &FetchOptions{URL: url, Tag: "annotated-signed", VerifyKeys: keys})
	if err != nil {
		t.Fatal(err)
	}
	if res.CommitSHA != r.unsigned || string(res.Files["stack.hcl"]) != stackAt("2.0.0") || res.Ref != "annotated-signed" {
		t.Fatalf("commit %s ref %s stack %s", res.CommitSHA, res.Ref, res.Files["stack.hcl"])
	}

	// Without keys nothing is verified.
	res, err = FetchStackInMemory(context.Background(), &FetchOptions{URL: url, Branch: branch})
	if err != nil || res.Signer != "" || res.Signature() != "" {
		t.Fatalf("signer %q err %v", res.Signer, err)
	}
}

func TestVerify_NeverUsesTheArchive(t *testing.T) {
	var archives int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/archive/") {
			atomic.AddInt32(&archives, 1)
			_, _ = w.Write(tarball(t, "", map[string]string{"r/stack.hcl": stackHCL}))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer s.Close()
	_, key := newKey(t, "release")
	_, err := FetchStackInMemory(context.Background(), &FetchOptions{Provider: "forgejo", URL: s.URL + "/o/r.git", VerifyKeys: []string{key}})
	if err == nil || atomic.LoadInt32(&archives) != 0 {
		t.Fatalf("archives %d err %v", archives, err)
	}
}

func TestVerify_BadKey(t *testing.T) {
	_, err := FetchStackInMemory(context.Background(), &FetchOptions{URL: "file:///nowhere", VerifyKeys: []string{"not a key"}})
	if err == nil || !strings.Contains(err.Error(), "public key 1") {
		t.Fatalf("got %v", err)
	}
}

func TestVerify_FetchOptionsOfABinding(t *testing.T) {
	off := false
	b := &config.GitOpsBinding{Verify: &config.VerifyConfig{PublicKeys: []string{"k"}}}
	if got := FetchOptionsFor(b).VerifyKeys; len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	b.Verify.Enabled = &off
	if got := FetchOptionsFor(b).VerifyKeys; got != nil {
		t.Fatalf("enabled = false must turn it off: %v", got)
	}
	b.Verify = nil
	if got := FetchOptionsFor(b).VerifyKeys; got != nil {
		t.Fatalf("got %v", got)
	}
}

// A push of an unsigned commit doesn't reach the cluster, and the log says
// why. A signed one is applied: the servers are told the commit and who
// signed it, and the log has the operation.
func TestVerify_DaemonRefusesAnUnsignedPush(t *testing.T) {
	release, key := newKey(t, "release")
	stranger, _ := newKey(t, "stranger")
	r := newSignedRepo(t, release, stranger)
	url := "file://" + filepath.ToSlash(r.dir)

	logger := NewRingLogger(50)
	f := &fakeNorca{}
	rec := NewReconciler(logger, f, time.Millisecond)
	defer rec.Close()
	rec.RegisterBinding(config.GitOpsBinding{Name: "b", Namespace: "production",
		Forgejo: &config.RepoConfig{URL: url, Branch: branchOf(t, r.dir)},
		Verify:  &config.VerifyConfig{PublicKeys: []string{key}}})

	err := rec.ReconcileBinding(context.Background(), "b", "webhook", &WebhookEvent{Branch: branchOf(t, r.dir), CommitSHA: r.unsigned})
	if err == nil || f.count("") != 0 {
		t.Fatalf("err %v calls %v", err, f.called())
	}
	if !strings.Contains(err.Error(), "gitops binding b") || !strings.Contains(err.Error(), "has no signature") {
		t.Fatalf("got %v", err)
	}
	failed := logger.All()[len(logger.All())-1]
	if failed.Phase != "FAILED" || !strings.Contains(failed.Error, "has no signature") {
		t.Fatalf("entry %+v", failed)
	}

	if err := rec.ReconcileBinding(context.Background(), "b", "webhook", &WebhookEvent{Branch: branchOf(t, r.dir), CommitSHA: r.signed}); err != nil {
		t.Fatal(err)
	}
	if f.count(planCall) != 1 || f.count(applyCall) != 1 {
		t.Fatalf("calls %v", f.called())
	}
	for _, req := range f.asked() {
		want := map[string]string{"repository": url, "ref": branchOf(t, r.dir), "commit": r.signed, "signer": "release <release@example.com>"}
		if !maps.Equal(req.Origin, want) {
			t.Fatalf("%s: origin %v, want %v", req.Path, req.Origin, want)
		}
	}

	var phases []string
	for _, e := range logger.All() {
		phases = append(phases, e.Phase)
	}
	if got := strings.Join(phases, " "); !strings.Contains(got, "VERIFY PLAN") || !strings.Contains(got, "APPLY OPERATION") || !strings.HasSuffix(got, "SUCCESS") {
		t.Fatalf("phases %s", got)
	}
	last := logger.All()[len(logger.All())-1]
	if last.Stack != "demo" || last.Operation != operationName(0) || last.CommitSHA != r.signed ||
		!strings.Contains(last.Details, "applied by the operation "+operationName(0)) {
		t.Fatalf("entry %+v", last)
	}
}

// A binding that applies and has no keys never reaches the repository or
// the cluster, whatever configured it.
func TestVerify_DaemonRefusesToApplyWithoutKeys(t *testing.T) {
	dir, _, _ := localRepo(t)
	logger := NewRingLogger(50)
	f := &fakeNorca{}
	rec := NewReconciler(logger, f, time.Millisecond)
	defer rec.Close()
	rec.RegisterBinding(config.GitOpsBinding{Name: "b",
		Forgejo: &config.RepoConfig{URL: "file://" + filepath.ToSlash(dir), Branch: branchOf(t, dir), Subpath: "stacks/a"}})

	err := rec.ReconcileBinding(context.Background(), "b", "webhook", nil)
	if !errors.Is(err, ErrApplyNeedsVerify) || f.count("") != 0 {
		t.Fatalf("err %v calls %v", err, f.called())
	}
	for _, e := range logger.All() {
		if e.Phase == "FETCH" {
			t.Fatalf("the repository was fetched: %+v", e)
		}
	}
}

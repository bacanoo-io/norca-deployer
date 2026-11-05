package gitops

import (
	"bytes"
	"context"
	"io"
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

	"github.com/bacanoo-io/norca-deployer/internal/config"
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
		if err := os.WriteFile(filepath.Join(dir, "metadata.hcl"), []byte(strings.ReplaceAll(packMeta, "0.1.0", n)), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Add("metadata.hcl"); err != nil {
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
			res, err := FetchPackInMemory(context.Background(), &opts)
			if tc.signer == "" {
				if err == nil {
					t.Fatalf("must be refused, got %s", res.Signer)
				}
				if !strings.Contains(err.Error(), "signature") {
					t.Fatalf("got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(res.Signer, tc.signer) {
				t.Fatalf("signer %q", res.Signer)
			}
		})
	}

	// The deployed commit is the one of the tag.
	res, err := FetchPackInMemory(context.Background(), &FetchOptions{URL: url, Tag: "annotated-signed", VerifyKeys: keys})
	if err != nil {
		t.Fatal(err)
	}
	if res.CommitSHA != r.unsigned || res.PackVersion != "2.0.0" {
		t.Fatalf("commit %s version %s", res.CommitSHA, res.PackVersion)
	}

	// Without keys nothing is verified.
	res, err = FetchPackInMemory(context.Background(), &FetchOptions{URL: url, Branch: branch})
	if err != nil || res.Signer != "" {
		t.Fatalf("signer %q err %v", res.Signer, err)
	}
}

func TestVerify_NeverUsesTheArchive(t *testing.T) {
	var archives int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/archive/") {
			atomic.AddInt32(&archives, 1)
			_, _ = w.Write(tarball(t, "", map[string]string{"r/metadata.hcl": packMeta}))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer s.Close()
	_, key := newKey(t, "release")
	_, err := FetchPackInMemory(context.Background(), &FetchOptions{Provider: "forgejo", URL: s.URL + "/o/r.git", VerifyKeys: []string{key}})
	if err == nil || atomic.LoadInt32(&archives) != 0 {
		t.Fatalf("archives %d err %v", archives, err)
	}
}

func TestVerify_BadKey(t *testing.T) {
	_, err := FetchPackInMemory(context.Background(), &FetchOptions{URL: "file:///nowhere", VerifyKeys: []string{"not a key"}})
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

// A push of an unsigned commit doesn't reach the cluster, and the log says why.
func TestVerify_DaemonRefusesAnUnsignedPush(t *testing.T) {
	release, key := newKey(t, "release")
	stranger, _ := newKey(t, "stranger")
	r := newSignedRepo(t, release, stranger)

	logger := NewRingLogger(50)
	f := &fakeNorca{}
	rec := NewReconciler(logger, f, time.Millisecond)
	defer rec.Close()
	rec.RegisterBinding(config.GitOpsBinding{Name: "b", ReleaseName: "rel",
		Forgejo: &config.RepoConfig{URL: "file://" + filepath.ToSlash(r.dir), Branch: branchOf(t, r.dir)},
		Verify:  &config.VerifyConfig{PublicKeys: []string{key}}})

	err := rec.ReconcileBinding(context.Background(), "b", "webhook", &WebhookEvent{Branch: branchOf(t, r.dir), CommitSHA: r.unsigned})
	if err == nil || f.count("") != 0 {
		t.Fatalf("err %v calls %v", err, f.calls)
	}
	if err := rec.ReconcileBinding(context.Background(), "b", "webhook", &WebhookEvent{Branch: branchOf(t, r.dir), CommitSHA: r.signed}); err != nil {
		t.Fatal(err)
	}
	if f.count("POST") != 1 {
		t.Fatalf("calls %v", f.calls)
	}
	phases := ""
	for _, e := range logger.All() {
		phases += e.Phase + " "
	}
	if !strings.Contains(phases, "VERIFY") {
		t.Fatalf("phases %s", phases)
	}
}

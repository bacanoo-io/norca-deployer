package gitops

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/bacanoo-io/norca-deployer/internal/config"
)

const packMeta = "app {\n  url = \"https://example.com\"\n  name = \"wrong\"\n}\n\npack {\n  name        = \"demo\"\n  description = \"d\"\n  version     = \"0.1.0\"\n\n  requires {\n    name = \"nested\"\n  }\n}\n"

const sha = "7f8b91a24d8c910283e5891d1209384bcdaef190"

func tarball(t *testing.T, commit string, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	if commit != "" {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header",
			PAXRecords: map[string]string{"comment": commit}, Format: tar.FormatPAX}); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return b.Bytes()
}

// fakeNorca records the calls of a deployment like the servers answer them.
type fakeNorca struct {
	mu      sync.Mutex
	calls   []string
	tokens  []string
	deploy  map[string]any
	archive []byte
	uploads []string
	status  int
}

func (f *fakeNorca) API(_ context.Context, method, path string, body []byte) (int, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, method+" "+path)
	if f.status != 0 {
		return f.status, "refused", nil
	}
	switch {
	case method == http.MethodPut && strings.HasPrefix(path, "/v1/pack-upload"):
		f.archive = body
		sum := sha256.Sum256(body)
		digest := "sha256:" + hex.EncodeToString(sum[:])
		f.uploads = append(f.uploads, digest)
		list := []map[string]string{}
		for _, u := range f.uploads {
			list = append(list, map[string]string{"digest": u})
		}
		out, _ := json.Marshal(map[string]any{"Digest": digest, "Registry": map[string]any{"Status": map[string]any{"uploads": list}}})
		return 200, string(out), nil
	case method == http.MethodDelete:
		f.uploads = f.uploads[len(f.uploads)-1:]
		return 200, "{}", nil
	case strings.Contains(path, "/deploy-pack"):
		_ = json.Unmarshal(body, &f.deploy)
		return 200, "{}", nil
	}
	return 404, "unknown", nil
}

func (f *fakeNorca) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func fetched(version string) *FetchResult {
	return &FetchResult{PackName: "demo", PackVersion: version, Branch: "main", CommitSHA: sha,
		PackFiles: map[string][]byte{"metadata.hcl": []byte(packMeta), "templates/app.nomad.tpl": []byte("job")}}
}

func TestDeploy_UploadsThePackThenDeploysItFromTheRegistry(t *testing.T) {
	f := &fakeNorca{}
	rel := &Release{Name: "payment", Namespace: "production", Region: "eu", Values: map[string]any{"replicas": 5},
		Atomic: true, MaxHistory: 7, Description: "d"}
	if err := Deploy(context.Background(), f, fetched("0.1.0"), rel); err != nil {
		t.Fatal(err)
	}

	registry := UploadRegistry("production", "payment")
	if !strings.Contains(f.calls[0], "PUT /v1/pack-upload?") || !strings.Contains(f.calls[0], "registry="+registry) || !strings.Contains(f.calls[0], "region=eu") {
		t.Fatalf("upload: %v", f.calls)
	}
	if !strings.Contains(f.calls[1], "POST /v1/release/payment/deploy-pack?") || !strings.Contains(f.calls[1], "namespace=production") || !strings.Contains(f.calls[1], "region=eu") {
		t.Fatalf("deploy: %v", f.calls)
	}
	if f.deploy["Registry"] != registry || f.deploy["Pack"] != "demo" || f.deploy["Version"] != "0.1.0" || f.deploy["Atomic"] != true || f.deploy["MaxHistory"] != float64(7) {
		t.Fatalf("request: %v", f.deploy)
	}

	// The pack is at the root of the archive, where the servers look for it.
	files, _, err := extractPlain(f.archive)
	if err != nil {
		t.Fatal(err)
	}
	if files["metadata.hcl"] != packMeta || files["templates/app.nomad.tpl"] != "job" {
		t.Fatalf("archive: %v", files)
	}
}

func extractPlain(data []byte) (map[string]string, string, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, "", err
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out, "", nil
		}
		if err != nil {
			return nil, "", err
		}
		b, _ := io.ReadAll(tr)
		out[h.Name] = string(b)
	}
}

func TestDeploy_AtomicIsLeftToTheNamespace(t *testing.T) {
	f := &fakeNorca{}
	if err := Deploy(context.Background(), f, fetched("0.1.0"), &Release{Name: "r"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.deploy["Atomic"]; ok {
		t.Fatal("false must not override the namespace")
	}
}

func TestDeploy_OldUploadIsRemovedBeforeTheDeployment(t *testing.T) {
	f := &fakeNorca{}
	rel := &Release{Name: "r"}
	if err := Deploy(context.Background(), f, fetched("0.1.0"), rel); err != nil {
		t.Fatal(err)
	}
	res := fetched("0.1.0")
	res.PackFiles["templates/app.nomad.tpl"] = []byte("job changed")
	if err := Deploy(context.Background(), f, res, rel); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.calls, "\n")
	del, dep := strings.LastIndex(got, "DELETE /v1/pack-upload/"), strings.LastIndex(got, "deploy-pack")
	if del < 0 || del > dep {
		t.Fatalf("calls: %s", got)
	}
}

func TestDeploy_Refusals(t *testing.T) {
	f := &fakeNorca{}
	noVersion := fetched("")
	if err := Deploy(context.Background(), f, noVersion, &Release{Name: "r"}); err == nil || len(f.calls) != 0 {
		t.Fatalf("a pack without a version must not be sent: %v %v", err, f.calls)
	}
	f.status = 403
	if err := Deploy(context.Background(), f, fetched("0.1.0"), &Release{Name: "r"}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("got %v", err)
	}
}

func TestProvenance_HidesCredentials(t *testing.T) {
	d := Provenance("github", "https://oauth2:SECRET@github.com/o/r.git", fetched("1"))
	if strings.Contains(d, "SECRET") || !strings.Contains(d, sha) {
		t.Fatalf("got %s", d)
	}
}

func TestExtractPackAttr_RealMetadata(t *testing.T) {
	if got := extractPackAttr(packMeta, "name"); got != "demo" {
		t.Fatalf("name %q", got)
	}
	if got := extractPackAttr(packMeta, "version"); got != "0.1.0" {
		t.Fatalf("version %q", got)
	}
	// A pack block of another name doesn't count.
	if got := extractPackAttr("packages {\n  name = \"x\"\n}\n", "name"); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestFetch_CommitOfAnEventIsRequestedAndReported(t *testing.T) {
	var path string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write(tarball(t, "", map[string]string{"r/metadata.hcl": packMeta}))
	}))
	defer s.Close()
	res, err := FetchPackInMemory(context.Background(), &FetchOptions{Provider: "forgejo", URL: s.URL + "/o/r.git", Branch: "main", CommitSHA: sha})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(path, sha) || res.CommitSHA != sha {
		t.Fatalf("path %s commit %s", path, res.CommitSHA)
	}
}

func TestFetch_CommitOfTheArchive(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball(t, sha, map[string]string{"r/metadata.hcl": packMeta}))
	}))
	defer s.Close()
	res, err := FetchPackInMemory(context.Background(), &FetchOptions{Provider: "forgejo", URL: s.URL + "/o/r.git", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if res.CommitSHA != sha {
		t.Fatalf("commit %q", res.CommitSHA)
	}
}

func TestFetch_RedirectDropsCredentials(t *testing.T) {
	var got string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("PRIVATE-TOKEN") + r.Header.Get("Authorization")
		_, _ = w.Write(tarball(t, "", map[string]string{"r/metadata.hcl": packMeta}))
	}))
	defer other.Close()
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "SECRET" {
			t.Error("the first request needs the token")
		}
		http.Redirect(w, r, strings.Replace(other.URL, "127.0.0.1", "localhost", 1)+"/s3", http.StatusFound)
	}))
	defer gl.Close()
	if _, err := FetchPackInMemory(context.Background(), &FetchOptions{Provider: "gitlab", URL: gl.URL + "/g/p.git", Auth: &AuthCredentials{Token: "SECRET"}}); err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("credentials reached the other host: %q", got)
	}
}

func TestArchiveURL_TokenStaysOnTheHostOfTheRepository(t *testing.T) {
	u, _, _ := buildArchiveURLAndHeaders("github", &FetchOptions{URL: "https://ghe.corp.example/o/r.git"}, "main")
	if !strings.HasPrefix(u, "https://ghe.corp.example/api/v3/repos/o/r/") {
		t.Fatalf("got %s", u)
	}
	u, _, _ = buildArchiveURLAndHeaders("github", &FetchOptions{URL: "https://github.com/o/r.git"}, "main")
	if !strings.HasPrefix(u, "https://api.github.com/repos/o/r/") {
		t.Fatalf("got %s", u)
	}
	u, _, _ = buildArchiveURLAndHeaders("github", &FetchOptions{URL: "https://ghe.corp.example/o/r.git", APIURL: "https://api.corp.example/"}, "main")
	if !strings.HasPrefix(u, "https://api.corp.example/repos/o/r/") {
		t.Fatalf("got %s", u)
	}
	u, _, _ = buildArchiveURLAndHeaders("gitlab", &FetchOptions{URL: "https://git.example/x.git", ProjectPath: "finance/services/billing"}, "main")
	if !strings.Contains(u, "/projects/finance%2Fservices%2Fbilling/") {
		t.Fatalf("got %s", u)
	}
}

func TestDetectProvider_LooksAtTheHostOnly(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/o/r.git":                        "github",
		"git@github.com:o/r.git":                            "github",
		"https://git.corp.example/mirrors/github.com-t.git": "forgejo",
		"https://gitlab.corp.example/g/p.git":               "gitlab",
		"https://gitea.corp.example/o/r.git":                "gitea",
		"https://github.com.evil.example/o/r.git":           "forgejo",
	} {
		if got := detectProvider(in); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
}

func TestSubpath(t *testing.T) {
	for in, want := range map[string]string{"": "", ".": "", "/": "", "/packs/a/": "packs/a", "packs/./a": "packs/a"} {
		got, err := cleanSubpath(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q %v", in, got, err)
		}
	}
	for _, in := range []string{"..", "../x", "a/../../x"} {
		if _, err := cleanSubpath(in); err == nil {
			t.Errorf("%q must be refused", in)
		}
	}
}

func TestExtract_Limits(t *testing.T) {
	var space packSpace
	if err := space.add(maxPackBytes + 1); err == nil {
		t.Fatal("too many bytes")
	}
	space = packSpace{files: maxPackFiles}
	if err := space.add(1); err == nil {
		t.Fatal("too many files")
	}
	big := tarball(t, "", map[string]string{"r/big": strings.Repeat("x", maxPackBytes+1)})
	if _, _, err := extractTarGzStream(bytes.NewReader(big), ""); err == nil {
		t.Fatal("an oversized pack must be refused")
	}
	// Files outside the subpath take no memory and count for nothing.
	files, _, err := extractTarGzStream(bytes.NewReader(big), "packs")
	if err != nil || len(files) != 0 {
		t.Fatalf("got %v %v", files, err)
	}
}

func TestRedact_URLCredentials(t *testing.T) {
	got := redact("Fetching https://oauth2:ghp_SECRET@github.com/o/r.git (main)")
	if strings.Contains(got, "ghp_SECRET") || !strings.Contains(got, "github.com/o/r.git") {
		t.Fatalf("got %s", got)
	}
}

// localRepo is a repository with a branch, a tag and an older commit.
func localRepo(t *testing.T) (dir, first, second string) {
	t.Helper()
	dir = t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	w, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	commit := func(version string) string {
		if err := os.MkdirAll(filepath.Join(dir, "packs", "a"), 0755); err != nil {
			t.Fatal(err)
		}
		meta := strings.ReplaceAll(packMeta, "0.1.0", version)
		if err := os.WriteFile(filepath.Join(dir, "packs", "a", "metadata.hcl"), []byte(meta), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Add("packs/a/metadata.hcl"); err != nil {
			t.Fatal(err)
		}
		h, err := w.Commit("v"+version, &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()}})
		if err != nil {
			t.Fatal(err)
		}
		return h.String()
	}
	first = commit("1.0.0")
	if _, err := repo.CreateTag("v1", plumbing.NewHash(first), nil); err != nil {
		t.Fatal(err)
	}
	second = commit("2.0.0")
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if head.Hash().String() != second {
		t.Fatal("head")
	}
	return dir, first, second
}

func TestFetch_GoGitFallback(t *testing.T) {
	dir, first, second := localRepo(t)
	url := "file://" + filepath.ToSlash(dir)
	branch := branchOf(t, dir)
	for name, tc := range map[string]struct {
		opts    FetchOptions
		version string
		commit  string
	}{
		"branch": {FetchOptions{Branch: branch}, "2.0.0", second},
		"tag":    {FetchOptions{Tag: "v1"}, "1.0.0", first},
		"commit": {FetchOptions{Branch: branch, CommitSHA: first}, "1.0.0", first},
		"ref":    {FetchOptions{Ref: "v1"}, "1.0.0", first},
	} {
		t.Run(name, func(t *testing.T) {
			opts := tc.opts
			opts.URL, opts.Subpath = url, "packs/a"
			res, err := FetchPackInMemory(context.Background(), &opts)
			if err != nil {
				t.Fatal(err)
			}
			if res.PackVersion != tc.version || res.CommitSHA != tc.commit {
				t.Fatalf("version %s commit %s", res.PackVersion, res.CommitSHA)
			}
		})
	}
}

func branchOf(t *testing.T, dir string) string {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	return head.Name().Short()
}

func TestFetch_ArchiveErrorIsKept(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer s.Close()
	_, err := FetchPackInMemory(context.Background(), &FetchOptions{Provider: "forgejo", URL: s.URL + "/o/r.git"})
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("got %v", err)
	}
}

func hmacHex(payload []byte, secret string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(payload)
	return hex.EncodeToString(m.Sum(nil))
}

// daemonFixture is a daemon with a Git server and a fake cluster.
type daemonFixture struct {
	url     string
	norca   *fakeNorca
	fetches *int32
	auth    *atomic.Value
	path    *atomic.Value
}

func newDaemonFixture(t *testing.T, cfg DaemonConfig, bindings ...config.GitOpsBinding) *daemonFixture {
	t.Helper()
	var fetches int32
	auth, path := &atomic.Value{}, &atomic.Value{}
	gitSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fetches, 1)
		auth.Store(r.Header.Get("Authorization"))
		path.Store(r.URL.Path)
		_, _ = w.Write(tarball(t, "", map[string]string{"r/metadata.hcl": packMeta}))
	}))
	t.Cleanup(gitSrv.Close)

	if len(bindings) == 0 {
		bindings = []config.GitOpsBinding{{Name: "b", ReleaseName: "rel", Namespace: "production",
			Forgejo: &config.RepoConfig{URL: gitSrv.URL + "/o/r.git", Branch: "main", AuthSecret: "git-token"},
			Webhook: &config.WebhookConfig{SecretKey: "secret", Events: []string{"push"}}}}
	} else {
		for i := range bindings {
			if bindings[i].Forgejo != nil {
				bindings[i].Forgejo.URL = gitSrv.URL + bindings[i].Forgejo.URL
			}
		}
	}
	f := &fakeNorca{}
	cfg.Bindings, cfg.Client = bindings, f
	if cfg.Debounce == 0 {
		cfg.Debounce = 30 * time.Millisecond
	}
	d := NewDaemon(cfg)
	t.Cleanup(d.reconciler.Close)
	srv := httptest.NewServer(d.server.Handler)
	t.Cleanup(srv.Close)
	return &daemonFixture{url: srv.URL, norca: f, fetches: &fetches, auth: auth, path: path}
}

func (f *daemonFixture) post(t *testing.T, query, event, secret, payload string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.url+"/v1/gitops/webhook"+query, strings.NewReader(payload))
	req.Header.Set("X-Forgejo-Event", event)
	req.Header.Set("X-Forgejo-Signature", hmacHex([]byte(payload), secret))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func push(sha string) string {
	return `{"ref":"refs/heads/main","after":"` + sha + `"}`
}

func TestDaemon_DebounceRunsTheLatestPushOnce(t *testing.T) {
	f := newDaemonFixture(t, DaemonConfig{Debounce: 100 * time.Millisecond})
	for _, c := range []string{"a", "b", sha} {
		if status := f.post(t, "", "push", "secret", push(c)); status != 200 {
			t.Fatalf("status %d", status)
		}
	}
	waitFor(t, func() bool { return f.norca.count("POST") == 1 })
	time.Sleep(200 * time.Millisecond)
	if n := atomic.LoadInt32(f.fetches); n != 1 || f.norca.count("POST") != 1 {
		t.Fatalf("fetches %d, deployments %d", n, f.norca.count("POST"))
	}

	// The binding reached the servers and the provider with its credentials
	// and its commit.
	if got := f.auth.Load(); got != "token git-token" {
		t.Fatalf("git auth %v", got)
	}
	if p := f.path.Load().(string); !strings.Contains(p, sha) {
		t.Fatalf("path %s", p)
	}
	if !strings.Contains(strings.Join(f.norca.calls, " "), "namespace=production") {
		t.Fatalf("calls %v", f.norca.calls)
	}
	if d := f.norca.deploy["Description"].(string); !strings.Contains(d, sha) {
		t.Fatalf("description %s", d)
	}
}

func TestDaemon_APushDuringARunRunsAfterIt(t *testing.T) {
	f := newDaemonFixture(t, DaemonConfig{})
	r := NewReconciler(nil, f.norca, 10*time.Millisecond)
	defer r.Close()
	d := &debouncer{running: true, pending: &WebhookEvent{}}
	r.debouncers["x"] = d
	r.fire("x", d)
	if !d.running || d.pending == nil {
		t.Fatal("the running reconciliation must keep the event")
	}
}

func TestDaemon_ForgedAndForeignRequests(t *testing.T) {
	f := newDaemonFixture(t, DaemonConfig{})
	if status := f.post(t, "", "push", "wrong", push(sha)); status != 401 {
		t.Fatalf("status %d", status)
	}
	if status := f.post(t, "?binding=other", "push", "secret", push(sha)); status != 404 {
		t.Fatalf("status %d", status)
	}
	// Another repository under the same webhook secret.
	foreign := `{"ref":"refs/heads/main","after":"` + sha + `","repository":{"clone_url":"https://git.example/other/repo.git"}}`
	if status := f.post(t, "", "push", "secret", foreign); status != 200 {
		t.Fatalf("status %d", status)
	}
	for _, event := range []string{"ping", "pull_request"} {
		f.post(t, "", event, "secret", `{"zen":"x"}`)
	}
	f.post(t, "", "push", "secret", `{"ref":"refs/heads/main","after":"0000000000000000000000000000000000000000"}`)
	time.Sleep(150 * time.Millisecond)
	if n := atomic.LoadInt32(f.fetches); n != 0 {
		t.Fatalf("%d fetches", n)
	}
}

func TestDaemon_SameRepositoryWithAnotherHostDeploys(t *testing.T) {
	f := newDaemonFixture(t, DaemonConfig{})
	own := `{"ref":"refs/heads/main","after":"` + sha + `","repository":{"clone_url":"https://public.example/O/R.git"}}`
	f.post(t, "", "push", "secret", own)
	waitFor(t, func() bool { return f.norca.count("POST") == 1 })
}

func TestDaemon_ASharedURLFindsTheBindingOfTheSecret(t *testing.T) {
	f := newDaemonFixture(t, DaemonConfig{},
		config.GitOpsBinding{Name: "one", ReleaseName: "one", Forgejo: &config.RepoConfig{URL: "/o/r.git", Branch: "main"},
			Webhook: &config.WebhookConfig{SecretKey: "s1"}},
		config.GitOpsBinding{Name: "two", ReleaseName: "two", Forgejo: &config.RepoConfig{URL: "/o/r.git", Branch: "main"},
			Webhook: &config.WebhookConfig{SecretKey: "s2"}})
	if status := f.post(t, "", "push", "s2", push(sha)); status != 200 {
		t.Fatalf("status %d", status)
	}
	waitFor(t, func() bool { return f.norca.count("POST /v1/release/two/") == 1 })
	if f.norca.count("POST /v1/release/one/") != 0 {
		t.Fatal("the other binding must not run")
	}
}

func TestDaemon_ProviderOfTheSignatureMustFitTheBinding(t *testing.T) {
	f := newDaemonFixture(t, DaemonConfig{})
	payload := push(sha)
	req, _ := http.NewRequest(http.MethodPost, f.url+"/webhook", strings.NewReader(payload))
	req.Header.Set("X-Gitlab-Token", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestDaemon_LogsAndStatusNeedTheToken(t *testing.T) {
	f := newDaemonFixture(t, DaemonConfig{APIToken: "api"})
	get := func(path, token string) int {
		req, _ := http.NewRequest(http.MethodGet, f.url+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, path := range []string{"/v1/gitops/logs", "/v1/gitops/status"} {
		if s := get(path, ""); s != 401 {
			t.Fatalf("%s without token: %d", path, s)
		}
		if s := get(path, "wrong"); s != 401 {
			t.Fatalf("%s wrong token: %d", path, s)
		}
		if s := get(path, "api"); s != 200 {
			t.Fatalf("%s: %d", path, s)
		}
	}
}

func TestDaemon_LogsWithoutATokenAreLoopbackOnly(t *testing.T) {
	d := NewDaemon(DaemonConfig{Client: &fakeNorca{}})
	defer d.reconciler.Close()
	called := false
	h := d.protected(func(http.ResponseWriter, *http.Request) { called = true })
	req := httptest.NewRequest(http.MethodGet, "/v1/gitops/logs", nil)
	req.RemoteAddr = "10.0.0.5:1234"
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != 403 || called {
		t.Fatalf("code %d", rec.Code)
	}
	req.RemoteAddr = "127.0.0.1:1234"
	h(httptest.NewRecorder(), req)
	if !called {
		t.Fatal("loopback must pass")
	}
}

func TestReconciler_CloseTwice(t *testing.T) {
	r := NewReconciler(nil, nil, 0)
	r.Close()
	r.Close()
	r.Schedule("x", &WebhookEvent{})
}

func TestReconciler_TagEventFetchesTheTag(t *testing.T) {
	opts := &FetchOptions{Branch: "main", Ref: "x"}
	applyEvent(opts, &WebhookEvent{Tag: "v1", CommitSHA: "abc"})
	if opts.Tag != "v1" || opts.Branch != "" || opts.Ref != "" || opts.CommitSHA != "" {
		t.Fatalf("got %+v", opts)
	}
	opts = &FetchOptions{Branch: "release/v2"}
	applyEvent(opts, &WebhookEvent{Branch: "release/v3", CommitSHA: "abc"})
	if opts.Branch != "release/v3" || opts.CommitSHA != "abc" {
		t.Fatalf("got %+v", opts)
	}
}

func TestMatchEvent(t *testing.T) {
	ping, _ := ParseWebhookPayload("github", "ping", []byte(`{"zen":"x"}`))
	if MatchEvent(ping, nil, nil, nil, "main") || MatchEvent(ping, []string{"push"}, []string{"main"}, nil, "main") {
		t.Error("a ping deploys nothing")
	}
	del, _ := ParseWebhookPayload("github", "push", []byte(`{"ref":"refs/heads/main","after":"0000000000000000000000000000000000000000"}`))
	if !del.Deleted || MatchEvent(del, []string{"push"}, []string{"main"}, nil, "main") {
		t.Error("a deleted branch deploys nothing")
	}
	rel, _ := ParseWebhookPayload("github", "release", []byte(`{"action":"published","release":{"tag_name":"v1"}}`))
	if rel.Tag != "v1" || !MatchEvent(rel, []string{"release"}, nil, []string{"v*"}, "main") {
		t.Errorf("a release must match: %+v", rel)
	}
	if MatchEvent(rel, nil, nil, []string{"v*"}, "main") {
		t.Error("a release needs the release event")
	}
	draft, _ := ParseWebhookPayload("github", "release", []byte(`{"action":"edited","release":{"tag_name":"v1"}}`))
	if MatchEvent(draft, []string{"release"}, nil, []string{"v*"}, "main") {
		t.Error("only a published release counts")
	}
	other, _ := ParseWebhookPayload("github", "push", []byte(`{"ref":"refs/heads/dev","after":"a"}`))
	if MatchEvent(other, nil, nil, nil, "") {
		t.Error("the default branch is main")
	}
	deep, _ := ParseWebhookPayload("github", "push", []byte(`{"ref":"refs/heads/release/a/b","after":"a"}`))
	if MatchEvent(deep, nil, []string{"release/*"}, nil, "") {
		t.Error("* stops at a /")
	}
}

func TestWebhookEventType(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-Gitlab-Event", "Tag Push Hook")
	if got := WebhookEventType("gitlab", r); got != "push" {
		t.Fatalf("got %s", got)
	}
	r.Header.Set("X-Gitlab-Event", "Release Hook")
	if got := WebhookEventType("gitlab", r); got != "release" {
		t.Fatalf("got %s", got)
	}
	r = httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-Gitea-Event", "push")
	if got := WebhookEventType("forgejo", r); got != "push" {
		t.Fatalf("got %s", got)
	}
}

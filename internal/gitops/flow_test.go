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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"

	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

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

func TestFetch_CommitOfAnEventIsRequestedAndReported(t *testing.T) {
	var path string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write(tarball(t, "", map[string]string{"r/stack.hcl": stackHCL}))
	}))
	defer s.Close()
	res, err := FetchStackInMemory(context.Background(), &FetchOptions{Provider: "forgejo", URL: s.URL + "/o/r.git", Branch: "main", CommitSHA: sha})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(path, sha) || res.CommitSHA != sha {
		t.Fatalf("path %s commit %s", path, res.CommitSHA)
	}
	// The commit is what is fetched, the branch what it is recorded under.
	if res.Ref != "main" || string(res.Files["stack.hcl"]) != stackHCL {
		t.Fatalf("ref %q files %v", res.Ref, res.Files)
	}

	// A commit alone is of no branch that could be named.
	res, err = FetchStackInMemory(context.Background(), &FetchOptions{Provider: "forgejo", URL: s.URL + "/o/r.git", CommitSHA: sha})
	if err != nil {
		t.Fatal(err)
	}
	if res.Ref != "" || res.CommitSHA != sha {
		t.Fatalf("ref %q commit %s", res.Ref, res.CommitSHA)
	}
}

// The directory of a stack has its stack.hcl at its root.
func TestFetch_NeedsAStack(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball(t, sha, map[string]string{"r/metadata.hcl": "pack {}", "r/stacks/a/stack.hcl": stackHCL, "r/stacks/a/values/prod.vars.hcl": "x = 1"}))
	}))
	defer s.Close()
	opts := &FetchOptions{Provider: "forgejo", URL: s.URL + "/o/r.git", Branch: "main"}
	if _, err := FetchStackInMemory(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "no stack.hcl") {
		t.Fatalf("got %v", err)
	}
	opts.Subpath = "stacks/a"
	res, err := FetchStackInMemory(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 2 || string(res.Files["stack.hcl"]) != stackHCL || string(res.Files["values/prod.vars.hcl"]) != "x = 1" {
		t.Fatalf("files %v", res.Files)
	}
}

func TestFetch_CommitOfTheArchive(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball(t, sha, map[string]string{"r/stack.hcl": stackHCL}))
	}))
	defer s.Close()
	res, err := FetchStackInMemory(context.Background(), &FetchOptions{Provider: "forgejo", URL: s.URL + "/o/r.git", Branch: "main"})
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
		_, _ = w.Write(tarball(t, "", map[string]string{"r/stack.hcl": stackHCL}))
	}))
	defer other.Close()
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "SECRET" {
			t.Error("the first request needs the token")
		}
		http.Redirect(w, r, strings.Replace(other.URL, "127.0.0.1", "localhost", 1)+"/s3", http.StatusFound)
	}))
	defer gl.Close()
	if _, err := FetchStackInMemory(context.Background(), &FetchOptions{Provider: "gitlab", URL: gl.URL + "/g/p.git", Auth: &AuthCredentials{Token: "SECRET"}}); err != nil {
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
	for in, want := range map[string]string{"": "", ".": "", "/": "", "/stacks/a/": "stacks/a", "stacks/./a": "stacks/a"} {
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
	var space stackSpace
	if err := space.add(maxStackBytes + 1); err == nil {
		t.Fatal("too many bytes")
	}
	space = stackSpace{files: maxStackFiles}
	if err := space.add(1); err == nil {
		t.Fatal("too many files")
	}
	big := tarball(t, "", map[string]string{"r/big": strings.Repeat("x", maxStackBytes+1)})
	if _, _, err := extractTarGzStream(bytes.NewReader(big), ""); err == nil {
		t.Fatal("an oversized stack directory must be refused")
	}
	// Files outside the subpath take no memory and count for nothing.
	files, _, err := extractTarGzStream(bytes.NewReader(big), "stacks")
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
		if err := os.MkdirAll(filepath.Join(dir, "stacks", "a"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stacks", "a", "stack.hcl"), []byte(stackAt(version)), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Add("stacks/a/stack.hcl"); err != nil {
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

// stackAt is the stack.hcl of a commit of a test repository: the version
// of its module tells the commits apart.
func stackAt(version string) string {
	return strings.ReplaceAll(stackHCL, "0.1.0", version)
}

func TestFetch_GoGitFallback(t *testing.T) {
	dir, first, second := localRepo(t)
	url := "file://" + filepath.ToSlash(dir)
	branch := branchOf(t, dir)
	for name, tc := range map[string]struct {
		opts    FetchOptions
		version string
		commit  string
		ref     string
	}{
		"branch":         {FetchOptions{Branch: branch}, "2.0.0", second, branch},
		"tag":            {FetchOptions{Tag: "v1"}, "1.0.0", first, "v1"},
		"commit":         {FetchOptions{Branch: branch, CommitSHA: first}, "1.0.0", first, branch},
		"only a commit":  {FetchOptions{CommitSHA: first}, "1.0.0", first, ""},
		"ref":            {FetchOptions{Ref: "v1"}, "1.0.0", first, "v1"},
		"nothing named":  {FetchOptions{}, "2.0.0", second, branch},
		"a zero commit":  {FetchOptions{Branch: branch, CommitSHA: "0000000000000000000000000000000000000000"}, "2.0.0", second, branch},
		"commit and tag": {FetchOptions{Tag: "v1", CommitSHA: first}, "1.0.0", first, "v1"},
	} {
		t.Run(name, func(t *testing.T) {
			opts := tc.opts
			opts.URL, opts.Subpath = url, "stacks/a"
			res, err := FetchStackInMemory(context.Background(), &opts)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(res.Files["stack.hcl"]); got != stackAt(tc.version) || res.CommitSHA != tc.commit {
				t.Fatalf("stack %q commit %s", got, res.CommitSHA)
			}
			// The ref is what the origin of the stack names: the branch,
			// the tag or the revision, never a guess.
			if res.Ref != tc.ref {
				t.Fatalf("ref %q, want %q", res.Ref, tc.ref)
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
	_, err := FetchStackInMemory(context.Background(), &FetchOptions{Provider: "forgejo", URL: s.URL + "/o/r.git"})
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
		_, _ = w.Write(tarball(t, "", map[string]string{"r/stack.hcl": stackHCL}))
	}))
	t.Cleanup(gitSrv.Close)

	// The bindings only plan: an archive has no signature, and a binding
	// that applies needs one (verify_test.go has those).
	if len(bindings) == 0 {
		bindings = []config.GitOpsBinding{{Name: "b", Namespace: "production", Policy: planOnly(),
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

func planOnly() *config.GitOpsPolicy {
	return &config.GitOpsPolicy{Mode: config.GitOpsModePlanOnly}
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
	waitFor(t, func() bool { return f.norca.count(planCall) == 1 })
	time.Sleep(200 * time.Millisecond)
	if n := atomic.LoadInt32(f.fetches); n != 1 || f.norca.count(planCall) != 1 {
		t.Fatalf("fetches %d, plans %d", n, f.norca.count(planCall))
	}

	// The binding reached the servers and the provider with its credentials
	// and its commit.
	if got := f.auth.Load(); got != "token git-token" {
		t.Fatalf("git auth %v", got)
	}
	if p := f.path.Load().(string); !strings.Contains(p, sha) {
		t.Fatalf("path %s", p)
	}
	if calls := f.norca.called(); calls[0] != planCall+"?namespace=production" {
		t.Fatalf("calls %v", calls)
	}
	// The servers are told where the stack came from: the address without
	// the credentials of the binding, the branch and the pushed commit.
	origin := f.norca.asked()[0].Origin
	if origin["commit"] != sha || origin["ref"] != "main" || !strings.HasSuffix(origin["repository"], "/o/r.git") || origin["signer"] != "" {
		t.Fatalf("origin %v", origin)
	}
	if strings.Contains(origin["repository"], "git-token") {
		t.Fatalf("origin %v", origin)
	}
	// A binding that only plans starts no operation.
	if n := f.norca.count(applyCall); n != 0 {
		t.Fatalf("%d applies", n)
	}
}

// The log of the daemon names the stack and the commit of a run, and its
// status says what the binding does.
func TestDaemon_LogAndStatusOfARun(t *testing.T) {
	logger := NewRingLogger(50)
	f := newDaemonFixture(t, DaemonConfig{Logger: logger, APIToken: "api"})
	if status := f.post(t, "", "push", "secret", push(sha)); status != 200 {
		t.Fatalf("status %d", status)
	}
	waitFor(t, func() bool {
		all := logger.All()
		return len(all) > 0 && all[len(all)-1].Phase == "SUCCESS"
	})

	var phases []string
	for _, e := range logger.All() {
		phases = append(phases, e.Phase)
		if e.BindingName != "b" || e.CommitSHA != sha {
			t.Errorf("entry %+v", e)
		}
		if (e.Phase == "PLAN" || e.Phase == "SUCCESS") && e.Stack != "demo" {
			t.Errorf("entry %+v", e)
		}
	}
	got := strings.Join(phases, " ")
	if !strings.HasPrefix(got, "TRIGGER FETCH PLAN") || !strings.HasSuffix(got, "PLAN SUCCESS") {
		t.Fatalf("phases %s", got)
	}
	last := logger.All()[len(logger.All())-1]
	if !strings.Contains(last.Details, "stack demo at commit 7f8b91a24d8c planned, nothing applied") {
		t.Fatalf("details %s", last.Details)
	}

	req, _ := http.NewRequest(http.MethodGet, f.url+"/v1/gitops/status", nil)
	req.Header.Set("Authorization", "Bearer api")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var status []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if len(status) != 1 {
		t.Fatalf("status %v", status)
	}
	st := status[0]
	if st["name"] != "b" || st["namespace"] != "production" || st["mode"] != "plan-only" || st["prune"] != false ||
		st["status"] != "SUCCESS" || st["last_commit"] != sha || st["last_stack"] != "demo" {
		t.Fatalf("status %v", st)
	}
	if _, has := st["release_name"]; has {
		t.Fatalf("status %v", st)
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
	waitFor(t, func() bool { return f.norca.count(planCall) == 1 })
}

func TestDaemon_ASharedURLFindsTheBindingOfTheSecret(t *testing.T) {
	f := newDaemonFixture(t, DaemonConfig{},
		config.GitOpsBinding{Name: "one", Namespace: "one", Policy: planOnly(), Forgejo: &config.RepoConfig{URL: "/o/r.git", Branch: "main"},
			Webhook: &config.WebhookConfig{SecretKey: "s1"}},
		config.GitOpsBinding{Name: "two", Namespace: "two", Policy: planOnly(), Forgejo: &config.RepoConfig{URL: "/o/r.git", Branch: "main"},
			Webhook: &config.WebhookConfig{SecretKey: "s2"}})
	if status := f.post(t, "", "push", "s2", push(sha)); status != 200 {
		t.Fatalf("status %d", status)
	}
	waitFor(t, func() bool { return f.norca.count(planCall+"?namespace=two") == 1 })
	if f.norca.count(planCall+"?namespace=one") != 0 {
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
	if resp.StatusCode != http.StatusUnauthorized {
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

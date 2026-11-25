package gitops

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/go-git/go-billy/v6/memfs"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/object"
	githttp "github.com/go-git/go-git/v6/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v6/plumbing/transport/ssh"
	"github.com/go-git/go-git/v6/storage/memory"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

const (
	// fetchTimeout bounds a fetch.
	fetchTimeout = 3 * time.Minute

	// The limits of a stack directory in memory, those of the archive the
	// servers take. A repository is fetched by anyone who can push to it.
	maxStackBytes = 64 << 20
	maxStackFiles = 4096
)

var (
	commitRe   = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	zeroCommit = regexp.MustCompile(`^0+$`)

	// sensitiveHeaders must not follow a redirect to another host.
	sensitiveHeaders = []string{"Authorization", "PRIVATE-TOKEN"}
)

// FetchStackInMemory fetches the stack directory of a repository purely into
// memory: the files below the subpath, which has the stack.hcl at its root.
func FetchStackInMemory(ctx context.Context, opts *FetchOptions) (*FetchResult, error) {
	if opts.URL == "" {
		return nil, errors.New("repository URL is required")
	}
	subpath, err := cleanSubpath(opts.Subpath)
	if err != nil {
		return nil, err
	}

	if err := CheckVerifyKeys(opts.VerifyKeys); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	ref := resolveRef(opts)
	provider := strings.ToLower(opts.Provider)
	if provider == "" {
		provider = detectProvider(opts.URL)
	}

	// 1. Direct HTTP tarball archive streaming, without packfile overhead.
	// An archive has no signatures: a verified stack is always cloned.
	var archiveErr error
	if len(opts.VerifyKeys) == 0 {
		var files map[string][]byte
		var commit string
		files, commit, archiveErr = fetchArchiveStream(ctx, provider, opts, ref, subpath)
		if archiveErr == nil && len(files) > 0 {
			return buildFetchResult(provider, namedRef(opts, defaultBranch), subpath, commitOf(opts, commit), files)
		}
	}

	// 2. Fallback: generic in-memory clone with go-git/v6.
	c, err := fetchWithGoGit(ctx, opts, subpath)
	if err != nil {
		if archiveErr != nil {
			return nil, fmt.Errorf("in-memory fetch failed for %s (%s): archive: %w; clone: %w", SanitizeURL(opts.URL), ref, archiveErr, err)
		}
		return nil, fmt.Errorf("in-memory fetch failed for %s (%s): %w", SanitizeURL(opts.URL), ref, err)
	}
	res, err := buildFetchResult(provider, namedRef(opts, c.head), subpath, c.commit, c.files)
	if err != nil {
		return nil, err
	}
	res.Signer, res.Signed = c.signer, c.signed
	return res, nil
}

// defaultBranch is what an archive is asked for when nothing else is named.
const defaultBranch = "main"

// resolveRef returns what to fetch: a commit, then a ref, a tag or a branch.
func resolveRef(opts *FetchOptions) string {
	if wantedCommit(opts) != "" {
		return opts.CommitSHA
	}
	return namedRef(opts, defaultBranch)
}

// namedRef returns the ref, the tag or the branch the options name. They
// name none when they name only a commit, and fallback is what was fetched
// when they name nothing.
func namedRef(opts *FetchOptions, fallback string) string {
	switch {
	case opts.Ref != "":
		return opts.Ref
	case opts.Tag != "":
		return opts.Tag
	case opts.Branch != "":
		return opts.Branch
	case wantedCommit(opts) != "":
		return ""
	}
	return fallback
}

func wantedCommit(opts *FetchOptions) string {
	if zeroCommit.MatchString(opts.CommitSHA) {
		return ""
	}
	return opts.CommitSHA
}

func commitOf(opts *FetchOptions, fetched string) string {
	if c := wantedCommit(opts); c != "" {
		return c
	}
	return fetched
}

// cleanSubpath normalizes a subpath relative to the repository root and
// refuses one that leaves it.
func cleanSubpath(subpath string) (string, error) {
	cleaned := path.Clean("/" + strings.ReplaceAll(subpath, `\`, "/"))
	if slices.Contains(strings.Split(subpath, "/"), "..") {
		return "", fmt.Errorf("subpath %q leaves the repository", subpath)
	}
	return strings.TrimPrefix(cleaned, "/"), nil
}

// repoHostPath returns the host and path of a repository URL, also of the scp
// form git@host:owner/repo.git.
func repoHostPath(repoURL string) (string, string) {
	if !strings.Contains(repoURL, "://") {
		if at := strings.Index(repoURL, "@"); at >= 0 {
			repoURL = repoURL[at+1:]
		}
		host, p, ok := strings.Cut(repoURL, ":")
		if !ok {
			return "", ""
		}
		return strings.ToLower(host), p
	}
	u, err := url.Parse(repoURL)
	if err != nil {
		return "", ""
	}
	return strings.ToLower(u.Hostname()), u.Path
}

// repoPath returns the owner/repo path of a URL in a comparable form.
func repoPath(repoURL string) string {
	_, p := repoHostPath(repoURL)
	return strings.ToLower(strings.TrimSuffix(strings.Trim(p, "/"), ".git"))
}

// detectProvider guesses the provider from the host, never from the path of
// a repository.
func detectProvider(repoURL string) string {
	host, _ := repoHostPath(repoURL)
	switch {
	case host == "github.com" || strings.HasSuffix(host, ".github.com"):
		return "github"
	case strings.Contains(host, "gitlab"):
		return "gitlab"
	case strings.Contains(host, "gitea"):
		return "gitea"
	}
	return "forgejo"
}

// userinfoRe is the user part of a URL that can't be parsed.
var userinfoRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*://)[^/?#]*@`)

// SanitizeURL removes the credentials of a URL, for a log, an error and the
// origin the servers record. The user of an SSH URL is part of its address
// (ssh://git@host/...) and stays; its password does not.
func SanitizeURL(repoURL string) string {
	u, err := url.Parse(repoURL)
	switch {
	case err != nil:
		// A credential may be why it can't be parsed.
		return userinfoRe.ReplaceAllString(repoURL, "$1")
	case u.User == nil:
		return repoURL
	case u.Scheme == "ssh":
		if _, secret := u.User.Password(); !secret {
			return repoURL
		}
		u.User = url.User(u.User.Username())
	default:
		u.User = nil
	}
	return u.String()
}

func buildHTTPClient(tlsCfg *TLSConfig) *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
	}

	if tlsCfg != nil {
		tConfig := &tls.Config{
			InsecureSkipVerify: tlsCfg.InsecureSkipVerify, //nolint:gosec // G402: --insecure-skip-tls-verify is the user's explicit choice
		}
		if tlsCfg.CACertPEM != "" {
			pool, _ := x509.SystemCertPool()
			if pool == nil {
				pool = x509.NewCertPool()
			}
			pool.AppendCertsFromPEM([]byte(tlsCfg.CACertPEM))
			tConfig.RootCAs = pool
		}
		transport.TLSClientConfig = tConfig
	}

	return &http.Client{
		Transport:     transport,
		Timeout:       fetchTimeout,
		CheckRedirect: stripCredentialsOnRedirect,
	}
}

// stripCredentialsOnRedirect drops the credentials when a redirect leaves the
// first host (GitHub to AWS S3) or goes from https to http. net/http only
// does this for the standard headers, not for PRIVATE-TOKEN.
func stripCredentialsOnRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	first := via[0].URL
	if req.URL.Host != first.Host || (first.Scheme == "https" && req.URL.Scheme != "https") {
		for _, h := range sensitiveHeaders {
			req.Header.Del(h)
		}
	}
	return nil
}

func fetchArchiveStream(ctx context.Context, provider string, opts *FetchOptions, ref, subpath string) (map[string][]byte, string, error) {
	u, err := url.Parse(opts.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, "", errors.New("no archive API for this URL")
	}
	archiveURL, headers, err := buildArchiveURLAndHeaders(provider, opts, ref)
	if err != nil {
		return nil, "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, archiveURL, nil)
	if err != nil {
		return nil, "", err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := buildHTTPClient(opts.TLS).Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("archive endpoint returned HTTP %d", resp.StatusCode)
	}
	return extractTarGzStream(resp.Body, subpath)
}

func buildArchiveURLAndHeaders(provider string, opts *FetchOptions, ref string) (string, map[string]string, error) {
	headers := make(map[string]string)
	u, err := url.Parse(opts.URL)
	if err != nil {
		return "", nil, err
	}

	token := ""
	if opts.Auth != nil {
		if opts.Auth.Token != "" {
			token = opts.Auth.Token
		} else if opts.Auth.Password != "" {
			token = opts.Auth.Password
		}
	}

	host := u.Host
	trimmedPath := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	baseURL := fmt.Sprintf("%s://%s", u.Scheme, host)

	switch provider {
	case "forgejo", "gitea":
		// Forgejo & Gitea: /api/v1/repos/{owner}/{repo}/archive/{ref}.tar.gz
		archiveURL := fmt.Sprintf("%s/api/v1/repos/%s/archive/%s.tar.gz", baseURL, trimmedPath, url.PathEscape(ref))
		if token != "" {
			headers["Authorization"] = "token " + token
		}
		return archiveURL, headers, nil

	case "github":
		// GitHub: api.github.com/repos/{owner}/{repo}/tarball/{ref}. A GitHub
		// Enterprise Server has its API on its own host: the token of a
		// repository must never go to any other.
		apiBase := opts.APIURL
		if apiBase == "" {
			if u.Hostname() == "github.com" {
				apiBase = "https://api.github.com"
			} else {
				apiBase = baseURL + "/api/v3"
			}
		}
		archiveURL := fmt.Sprintf("%s/repos/%s/tarball/%s", strings.TrimSuffix(apiBase, "/"), trimmedPath, url.PathEscape(ref))
		headers["Accept"] = "application/vnd.github+json"
		if token != "" {
			headers["Authorization"] = "Bearer " + token
		}
		return archiveURL, headers, nil

	case "gitlab":
		// GitLab: /api/v4/projects/{encoded_project_path}/repository/archive.tar.gz?sha={ref}
		project := strings.Trim(opts.ProjectPath, "/")
		if project == "" {
			project = trimmedPath
		}
		apiBase := opts.APIURL
		if apiBase == "" {
			apiBase = baseURL + "/api/v4"
		}
		archiveURL := fmt.Sprintf("%s/projects/%s/repository/archive.tar.gz?sha=%s", strings.TrimSuffix(apiBase, "/"), url.PathEscape(project), url.QueryEscape(ref))
		if token != "" {
			headers["PRIVATE-TOKEN"] = token
		}
		return archiveURL, headers, nil
	}

	return "", nil, fmt.Errorf("unsupported archive provider: %s", provider)
}

// stackSpace counts what a stack directory takes in memory.
type stackSpace struct {
	files int
	bytes int64
}

func (s *stackSpace) add(size int64) error {
	s.files++
	s.bytes += size
	if s.files > maxStackFiles {
		return fmt.Errorf("the stack directory has more than %d files", maxStackFiles)
	}
	if s.bytes > maxStackBytes {
		return fmt.Errorf("the stack directory is larger than %d MiB", maxStackBytes>>20)
	}
	return nil
}

// extractTarGzStream reads the files below subpath, which cleanSubpath has
// normalized. git archive stores the commit in the global PAX header.
func extractTarGzStream(body io.Reader, subpath string) (map[string][]byte, string, error) {
	gz, err := gzip.NewReader(body)
	if err != nil {
		return nil, "", err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	files := make(map[string][]byte)
	commit := ""
	var space stackSpace

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", err
		}

		if hdr.Typeflag == tar.TypeXGlobalHeader {
			if c := hdr.PAXRecords["comment"]; commitRe.MatchString(c) {
				commit = c
			}
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		// Most archives have a top-level directory prefix: repo-ref/file
		name := path.Clean(strings.ReplaceAll(hdr.Name, `\`, "/"))
		if strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") {
			continue
		}
		relPath := name
		if _, rest, ok := strings.Cut(name, "/"); ok {
			relPath = rest
		}
		relPath, ok := belowSubpath(relPath, subpath)
		if !ok {
			continue
		}

		if err := space.add(hdr.Size); err != nil {
			return nil, "", err
		}
		// The size in the header is what the reader returns at most.
		content, err := io.ReadAll(tr)
		if err != nil {
			return nil, "", err
		}
		files[relPath] = content
	}

	return files, commit, nil
}

// belowSubpath returns the path of a file relative to subpath, if it is in it.
func belowSubpath(name, subpath string) (string, bool) {
	if subpath == "" {
		return name, name != ""
	}
	rel, ok := strings.CutPrefix(name, subpath+"/")
	return rel, ok && rel != ""
}

// sshAuth builds the SSH credentials of a repository. The known hosts stay in
// memory; without them go-git uses the known_hosts files of the user.
func sshAuth(auth *AuthCredentials, repoURL string) (client.SSHAuth, error) {
	user := auth.Username
	if user == "" {
		user = "git"
		if u, err := url.Parse(repoURL); err == nil && u.User != nil && u.User.Username() != "" {
			user = u.User.Username()
		}
	}
	keys, err := gitssh.NewPublicKeys(user, []byte(auth.SSHPrivateKey), auth.Password)
	if err != nil {
		return nil, fmt.Errorf("SSH private key: %w", err)
	}
	if auth.KnownHosts != "" {
		callback, err := knownHostsCallback(auth.KnownHosts)
		if err != nil {
			return nil, err
		}
		keys.HostKeyCallback = callback
	}
	return keys, nil
}

// knownHostsCallback checks host keys against known_hosts lines in memory.
func knownHostsCallback(lines string) (gossh.HostKeyCallback, error) {
	type entry struct {
		hosts []string
		key   gossh.PublicKey
	}
	var entries []entry
	rest := []byte(lines)
	for len(rest) > 0 {
		_, hosts, key, _, next, err := gossh.ParseKnownHosts(rest)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("known_hosts: %w", err)
		}
		entries = append(entries, entry{hosts: hosts, key: key})
		rest = next
	}
	if len(entries) == 0 {
		return nil, errors.New("known_hosts: no host keys")
	}
	return func(hostname string, remote net.Addr, key gossh.PublicKey) error {
		want := knownhosts.Normalize(hostname)
		for _, e := range entries {
			if string(e.key.Marshal()) != string(key.Marshal()) {
				continue
			}
			for _, h := range e.hosts {
				if knownhosts.Normalize(h) == want {
					return nil
				}
			}
		}
		return fmt.Errorf("host key of %s is not in known_hosts", hostname)
	}, nil
}

func gitClientOptions(opts *FetchOptions) ([]client.Option, error) {
	var clientOpts []client.Option
	if a := opts.Auth; a != nil {
		if a.SSHPrivateKey != "" {
			auth, err := sshAuth(a, opts.URL)
			if err != nil {
				return nil, err
			}
			clientOpts = append(clientOpts, client.WithSSHAuth(auth))
		}
		user, pass := a.Username, a.Password
		if a.Token != "" {
			if user == "" {
				user = "oauth2"
			}
			pass = a.Token
		}
		if user != "" || pass != "" {
			clientOpts = append(clientOpts, client.WithHTTPAuth(&githttp.BasicAuth{Username: user, Password: pass}))
		}
	}
	if opts.TLS != nil {
		if opts.TLS.InsecureSkipVerify {
			clientOpts = append(clientOpts, client.WithInsecureSkipTLS())
		}
		if opts.TLS.CACertPEM != "" {
			clientOpts = append(clientOpts, client.WithCABundle([]byte(opts.TLS.CACertPEM)))
		}
	}
	return clientOpts, nil
}

// cloned is what a clone fetched.
type cloned struct {
	files  map[string][]byte
	commit string
	// head is the branch the clone checked out when the options name
	// nothing: the default branch of the repository.
	head string
	// signer and signed say who signed what, when it was verified.
	signer, signed string
}

func fetchWithGoGit(ctx context.Context, opts *FetchOptions, subpath string) (*cloned, error) {
	clientOpts, err := gitClientOptions(opts)
	if err != nil {
		return nil, err
	}
	cloneOpts := &git.CloneOptions{URL: opts.URL, ClientOptions: clientOpts}

	// A branch or tag needs only its tip; any other revision needs the
	// history to be found in.
	revision := wantedCommit(opts)
	if revision == "" {
		revision = opts.Ref
	}
	if revision == "" {
		cloneOpts.SingleBranch = true
		cloneOpts.Depth = 1
		cloneOpts.Tags = git.NoTags
		switch {
		case opts.Tag != "":
			cloneOpts.ReferenceName = plumbing.NewTagReferenceName(opts.Tag)
		case opts.Branch != "":
			cloneOpts.ReferenceName = plumbing.NewBranchReferenceName(opts.Branch)
		}
	} else {
		cloneOpts.Tags = git.AllTags
	}

	// Clone strictly into RAM.
	repo, err := git.CloneContext(ctx, memory.NewStorage(), memfs.New(), cloneOpts)
	if err != nil {
		return nil, err
	}

	res := &cloned{}
	var hash plumbing.Hash
	if revision == "" {
		head, err := repo.Head()
		if err != nil {
			return nil, err
		}
		hash = head.Hash()
		if head.Name().IsBranch() {
			res.head = head.Name().Short()
		}
	} else {
		var last error
		for _, candidate := range []string{revision, "refs/remotes/origin/" + revision, "refs/tags/" + revision} {
			h, err := repo.ResolveRevision(plumbing.Revision(candidate))
			if err == nil {
				hash, last = *h, nil
				break
			}
			last = err
		}
		if last != nil {
			return nil, fmt.Errorf("revision %s: %w", revision, last)
		}
	}

	// The head of an annotated tag can be the tag.
	var commit *object.Commit
	if tag, err := repo.TagObject(hash); err == nil {
		commit, err = tag.Commit()
		if err != nil {
			return nil, err
		}
	} else if commit, err = repo.CommitObject(hash); err != nil {
		return nil, err
	}
	res.commit = commit.Hash.String()

	if len(opts.VerifyKeys) > 0 {
		tagName := opts.Tag
		if tagName == "" && wantedCommit(opts) == "" {
			tagName = opts.Ref
		}
		if res.signer, res.signed, err = verifySignature(repo, commit, tagName, opts.VerifyKeys); err != nil {
			return nil, fmt.Errorf("signature verification failed: %w", err)
		}
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}

	files := make(map[string][]byte)
	var space stackSpace
	err = tree.Files().ForEach(func(f *object.File) error {
		relPath, ok := belowSubpath(f.Name, subpath)
		if !ok {
			return nil
		}
		if err := space.add(f.Size); err != nil {
			return err
		}
		reader, err := f.Reader()
		if err != nil {
			return err
		}
		defer reader.Close()

		data, err := io.ReadAll(reader)
		if err != nil {
			return err
		}
		files[relPath] = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	res.files = files
	return res, nil
}

func buildFetchResult(provider, ref, subpath, commit string, files map[string][]byte) (*FetchResult, error) {
	if len(files) == 0 {
		return nil, errors.New("no files found in the repository or its subpath")
	}

	// The stack is at the root of the subpath: that is where the servers
	// are told it is.
	if _, ok := files[stackFile]; !ok {
		return nil, fmt.Errorf("no %s in %q of the repository: the subpath is the directory of a stack", stackFile, subpath)
	}

	return &FetchResult{Provider: provider, CommitSHA: commit, Ref: ref, Files: files}, nil
}

// FetchOptionsFor returns the fetch options of a binding whose secrets are
// resolved.
func FetchOptionsFor(b *config.GitOpsBinding) *FetchOptions {
	repo := b.EffectiveRepo()
	opts := &FetchOptions{
		Provider: b.EffectiveProvider(),
		URL:      repo.URL,
		Branch:   repo.Branch,
		Ref:      repo.Ref,
		Subpath:  repo.Subpath,
		APIURL:   b.EffectiveAPIURL(),
		Auth: &AuthCredentials{
			Token:         repo.AuthSecret,
			SSHPrivateKey: repo.SSHPrivateKey,
			KnownHosts:    repo.KnownHosts,
		},
		TLS: &TLSConfig{
			InsecureSkipVerify: repo.InsecureSkipVerify,
			CACertPEM:          repo.CACertPEM,
		},
	}
	if b.GitLab != nil {
		opts.ProjectPath = b.GitLab.ProjectPath
	}
	if b.Verify.Active() {
		opts.VerifyKeys = b.Verify.PublicKeys
	}
	return opts
}

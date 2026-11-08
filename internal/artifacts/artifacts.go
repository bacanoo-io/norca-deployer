// Package artifacts resolves the files the deployer copies to the nodes: the
// norca binary and the CNI plugin archive. Downloads happen on the deployer
// machine (the nodes need no internet access), are verified with SHA-256 and
// cached in the state directory.
package artifacts

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// File is a local file ready to copy to a node.
type File struct {
	Path   string
	SHA256 string
}

// Resolver resolves and caches artifacts per platform. It is safe for
// concurrent use by the per-node goroutines.
type Resolver struct {
	plan     *config.DeploymentPlan
	cacheDir string
	client   *http.Client
	// log is where the downloads are reported (stdout by default).
	log io.Writer
	// retryDelay is the wait before the first retry of a download that
	// failed for a passing reason; it doubles with each retry.
	retryDelay time.Duration

	mu      sync.Mutex
	results map[string]*result
}

// SetLog sends the reports of the downloads to w instead of stdout: a
// command whose stdout is its output (a script) reports them on stderr.
func (r *Resolver) SetLog(w io.Writer) { r.log = w }

type result struct {
	once sync.Once
	file *File
	err  error
}

func NewResolver(plan *config.DeploymentPlan, cacheDir string) *Resolver {
	return &Resolver{
		plan:       plan,
		cacheDir:   cacheDir,
		client:     &http.Client{Timeout: 10 * time.Minute},
		log:        os.Stdout,
		retryDelay: 2 * time.Second,
		results:    map[string]*result{},
	}
}

// downloadAttempts is how many times a download is tried when it fails for a
// passing reason: a network error, or an answer 5xx or 429 of the server.
const downloadAttempts = 4

func (r *Resolver) memo(key string, fn func() (*File, error)) (*File, error) {
	r.mu.Lock()
	res, ok := r.results[key]
	if !ok {
		res = &result{}
		r.results[key] = res
	}
	r.mu.Unlock()
	res.once.Do(func() { res.file, res.err = fn() })
	return res.file, res.err
}

// NorcaBinary returns the norca binary for a platform.
func (r *Resolver) NorcaBinary(ctx context.Context, goos, arch string) (*File, error) {
	return r.ReleaseBinary(ctx, "norca", goos, arch)
}

// ReleaseBinary returns a binary of the release archive for a platform.
func (r *Resolver) ReleaseBinary(ctx context.Context, name, goos, arch string) (*File, error) {
	return r.memo(name+"/"+goos+"/"+arch, func() (*File, error) {
		rel := r.plan.Release
		binName := name
		if goos == "windows" {
			binName = name + ".exe"
		}
		switch rel.Source {
		case config.ReleaseLocalPath:
			src := r.plan.ResolveLocalPath(expand(rel.LocalBinaryPath, goos, arch))
			if isArchive(src) {
				if err := verifyOptional(src, rel.ChecksumSHA256); err != nil {
					return nil, err
				}
				dest := filepath.Join(r.cacheDir, "norca", "local", goos+"_"+arch, binName)
				if err := extractBinary(src, binName, dest); err != nil {
					return nil, err
				}
				return fileWithDigest(dest)
			}
			if err := verifyOptional(src, rel.ChecksumSHA256); err != nil {
				return nil, err
			}
			return fileWithDigest(src)

		case config.ReleaseOfficial, config.ReleaseCustomURL:
			var archive string
			rf, err := r.withSums(func(fresh bool) (*remoteFile, error) {
				rf, err := r.releaseArchive(ctx, goos, arch, fresh)
				if err != nil {
					return rf, err
				}
				archive = filepath.Join(rf.dir, rf.name)
				return rf, r.download(ctx, rf.url, archive, rf.sha256)
			})
			if err != nil {
				return nil, err
			}
			dest := filepath.Join(rf.dir, goos+"_"+arch, binName)
			if err := extractBinary(archive, binName, dest); err != nil {
				return nil, err
			}
			return fileWithDigest(dest)
		}
		return nil, fmt.Errorf("release source %q does not provide a binary", rel.Source)
	})
}

// remoteFile is a file of a release on the internet: its URL, its name, the
// digest it must have, and the cache directory of its release. cachedSums is
// true when the digest was read from a checksum file of the cache, not one
// downloaded now.
type remoteFile struct {
	url, name, sha256, dir string
	cachedSums             bool
}

// errChecksum marks a downloaded file whose digest is not the expected one.
var errChecksum = errors.New("checksum mismatch")

// withSums runs fetch, which resolves a remote file and downloads it. When
// the digest came from a cached checksum file and the download does not
// match it, the checksum file may be out of date (a release republished
// under the same version): fetch runs once more with a fresh one.
func (r *Resolver) withSums(fetch func(fresh bool) (*remoteFile, error)) (*remoteFile, error) {
	rf, err := fetch(false)
	if err != nil && rf != nil && rf.cachedSums && errors.Is(err, errChecksum) {
		fmt.Fprintf(r.log, "    The cached checksums of %s do not match, downloading them again\n", rf.name)
		return fetch(true)
	}
	return rf, err
}

// sums returns the digest of name from the checksum file of a release at
// dest, downloading the file from url unless the cache already has one that
// lists name. The files of a released version do not change, so a cached
// list is used as it is (fresh forces the download): once the files are
// cached, an apply does not depend on the release server.
func (r *Resolver) sums(ctx context.Context, url, dest, name string, fresh bool) (string, bool, error) {
	if !fresh {
		if sum, err := lookupSum(dest, name); err == nil {
			return sum, true, nil
		}
	}
	if err := r.download(ctx, url, dest, ""); err != nil {
		return "", false, err
	}
	sum, err := lookupSum(dest, name)
	return sum, false, err
}

// releaseArchive resolves the release archive of a platform (sources
// official and custom-url). Without release.checksum_sha256 the digest comes
// from the SHA256SUMS file of the release, downloaded once and cached.
func (r *Resolver) releaseArchive(ctx context.Context, goos, arch string, fresh bool) (*remoteFile, error) {
	rel := r.plan.Release
	version := strings.TrimPrefix(rel.Version, "v")
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	archiveName := fmt.Sprintf("norca_%s_%s_%s%s", version, goos, arch, ext)
	sumsName := fmt.Sprintf("norca_%s_SHA256SUMS", version)
	base := strings.TrimRight(rel.DownloadBaseURL, "/") + "/" + rel.Version + "/"
	dir := filepath.Join(r.cacheDir, "norca", version)

	rf := &remoteFile{url: base + archiveName, name: archiveName, sha256: strings.ToLower(rel.ChecksumSHA256), dir: dir}
	if rf.sha256 == "" {
		var err error
		if rf.sha256, rf.cachedSums, err = r.sums(ctx, base+sumsName, filepath.Join(dir, sumsName), archiveName, fresh); err != nil {
			return nil, err
		}
	}
	return rf, nil
}

// CNIArchive returns the CNI plugin archive (.tgz) for a Linux architecture.
func (r *Resolver) CNIArchive(ctx context.Context, arch string) (*File, error) {
	return r.memo("cni/"+arch, func() (*File, error) {
		c := r.plan.CNI
		switch c.Source {
		case config.ReleaseLocalPath:
			src := r.plan.ResolveLocalPath(expand(c.LocalArchivePath, "linux", arch))
			if err := verifyOptional(src, c.ChecksumSHA256); err != nil {
				return nil, err
			}
			return fileWithDigest(src)
		default:
			var dest string
			rf, err := r.withSums(func(fresh bool) (*remoteFile, error) {
				rf, err := r.cniArchive(ctx, arch, fresh)
				if err != nil {
					return rf, err
				}
				dest = filepath.Join(rf.dir, rf.name)
				return rf, r.download(ctx, rf.url, dest, rf.sha256)
			})
			if err != nil {
				return nil, err
			}
			return &File{Path: dest, SHA256: rf.sha256}, nil
		}
	})
}

// cniArchive resolves the CNI plugin archive of an architecture (sources
// official and custom-url). Without cni.checksum_sha256 the digest comes
// from the .sha256 file next to the archive, downloaded once and cached.
func (r *Resolver) cniArchive(ctx context.Context, arch string, fresh bool) (*remoteFile, error) {
	c := r.plan.CNI
	name := fmt.Sprintf("cni-plugins-linux-%s-%s.tgz", arch, c.Version)
	url := strings.TrimRight(c.DownloadBaseURL, "/") + "/" + c.Version + "/" + name
	dir := filepath.Join(r.cacheDir, "cni", c.Version)
	rf := &remoteFile{url: url, name: name, sha256: strings.ToLower(c.ChecksumSHA256), dir: dir}
	if rf.sha256 == "" {
		var err error
		if rf.sha256, rf.cachedSums, err = r.sums(ctx, url+".sha256", filepath.Join(dir, name+".sha256"), name, fresh); err != nil {
			return nil, err
		}
	}
	return rf, nil
}

// Download is a file that a machine downloads itself (the install script of
// a scaled machine): its URL and the SHA-256 it must have.
type Download struct {
	URL    string
	SHA256 string
	// Archive is true for a release archive that holds the binary (a
	// .tar.gz); false for the file itself.
	Archive bool
}

// ReleaseDownload returns the URL and the digest of the release archive of a
// platform, the one ReleaseBinary downloads and extracts, without
// downloading it. Only the sources official and custom-url have one; the
// SHA256SUMS of the release is downloaded when the plan has no checksum.
func (r *Resolver) ReleaseDownload(ctx context.Context, goos, arch string) (*Download, error) {
	switch r.plan.Release.Source {
	case config.ReleaseOfficial, config.ReleaseCustomURL:
	default:
		return nil, fmt.Errorf("release source %q has no download URL", r.plan.Release.Source)
	}
	rf, err := r.releaseArchive(ctx, goos, arch, false)
	if err != nil {
		return nil, err
	}
	return &Download{URL: rf.url, SHA256: rf.sha256, Archive: true}, nil
}

// CNIDownload returns the URL and the digest of the CNI plugin archive of a
// Linux architecture, the one CNIArchive downloads, without downloading it.
// The source local-path has none.
func (r *Resolver) CNIDownload(ctx context.Context, arch string) (*Download, error) {
	if r.plan.CNI.Source == config.ReleaseLocalPath {
		return nil, fmt.Errorf("cni source %q has no download URL", r.plan.CNI.Source)
	}
	rf, err := r.cniArchive(ctx, arch, false)
	if err != nil {
		return nil, err
	}
	return &Download{URL: rf.url, SHA256: rf.sha256, Archive: true}, nil
}

func expand(p, goos, arch string) string {
	return strings.NewReplacer("{os}", goos, "{arch}", arch).Replace(p)
}

func isArchive(p string) bool {
	return strings.HasSuffix(p, ".tar.gz") || strings.HasSuffix(p, ".tgz") || strings.HasSuffix(p, ".zip")
}

// download fetches url into dest unless dest already has the wanted digest.
// An empty want accepts any content (used for checksum files).
func (r *Resolver) download(ctx context.Context, url, dest, want string) error {
	if want != "" {
		if got, err := FileSHA256(dest); err == nil && got == want {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return err
	}
	fmt.Fprintf(r.log, "    Downloading %s\n", url)
	delay := r.retryDelay
	for attempt := 1; ; attempt++ {
		err := r.fetch(ctx, url, dest, want)
		var passing *passingError
		if err == nil || !errors.As(err, &passing) || attempt == downloadAttempts {
			return err
		}
		fmt.Fprintf(r.log, "    %v; trying again in %s (attempt %d of %d)\n", err, delay, attempt+1, downloadAttempts)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(delay):
		}
		delay *= 2
	}
}

// passingError is a failure of a download that may not happen again: a
// network error, or an answer 5xx or 429 of the server (a 504 of GitHub, for
// example). Any other answer, and a file that does not match its digest,
// is final.
type passingError struct{ err error }

func (e *passingError) Error() string { return e.err.Error() }
func (e *passingError) Unwrap() error { return e.err }

// fetch makes one attempt of a download.
func (r *Resolver) fetch(ctx context.Context, url, dest, want string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("download %s: %w", url, err)
		}
		return &passingError{fmt.Errorf("download %s: %w", url, err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("download %s: HTTP %s", url, resp.Status)
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			return &passingError{err}
		}
		return err
	}
	tmp := dest + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(f, h), resp.Body)
	// A Close error of a written file is a file that is not complete.
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		if copyErr != nil && ctx.Err() == nil {
			// The connection broke while the body was read.
			return &passingError{fmt.Errorf("download %s: %w", url, copyErr)}
		}
		return fmt.Errorf("download %s: %w", url, errors.Join(copyErr, closeErr))
	}
	if got := hex.EncodeToString(h.Sum(nil)); want != "" && got != want {
		_ = os.Remove(tmp)
		return fmt.Errorf("download %s: sha256 %s does not match the expected %s: %w", url, got, want, errChecksum)
	}
	return os.Rename(tmp, dest)
}

// lookupSum finds the digest of a file name in a sha256sum style file.
func lookupSum(sumsPath, name string) (string, error) {
	f, err := os.Open(sumsPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			return strings.ToLower(fields[0]), nil
		}
		// Single-entry checksum files may name the file differently.
		if len(fields) >= 1 && len(fields[0]) == 64 && !strings.Contains(sc.Text(), " ") {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("%s has no checksum for %s", filepath.Base(sumsPath), name)
}

func verifyOptional(p, want string) error {
	if want == "" {
		if _, err := os.Stat(p); err != nil {
			return err
		}
		return nil
	}
	got, err := FileSHA256(p)
	if err != nil {
		return err
	}
	if got != strings.ToLower(want) {
		return fmt.Errorf("%s: sha256 %s does not match the expected %s", p, got, want)
	}
	return nil
}

func fileWithDigest(p string) (*File, error) {
	sum, err := FileSHA256(p)
	if err != nil {
		return nil, err
	}
	return &File{Path: p, SHA256: sum}, nil
}

// FileSHA256 returns the hex SHA-256 of a local file.
func FileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractBinary extracts the entry named binName from a release archive.
func extractBinary(archive, binName, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return err
	}
	var src io.Reader
	var closer func()
	if strings.HasSuffix(archive, ".zip") {
		zr, err := zip.OpenReader(archive)
		if err != nil {
			return err
		}
		defer zr.Close()
		for _, f := range zr.File {
			if path.Base(f.Name) == binName && !f.FileInfo().IsDir() {
				rc, err := f.Open()
				if err != nil {
					return err
				}
				src, closer = rc, func() { _ = rc.Close() }
				break
			}
		}
	} else {
		f, err := os.Open(archive)
		if err != nil {
			return err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		tr := tar.NewReader(gz)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			if hdr.Typeflag == tar.TypeReg && path.Base(hdr.Name) == binName {
				src, closer = tr, func() {}
				break
			}
		}
	}
	if src == nil {
		return fmt.Errorf("%s has no %s", filepath.Base(archive), binName)
	}
	defer closer()
	tmp := dest + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0700) //nolint:gosec // G302: a binary the deployer copies to the nodes must be executable
	if err != nil {
		return err
	}
	// The archive is checksummed, but a binary that inflates without bound
	// would still fill the disk before anyone looks at it.
	n, err := io.Copy(out, io.LimitReader(src, maxBinaryBytes+1))
	if err == nil && n > maxBinaryBytes {
		err = fmt.Errorf("%s in %s is larger than %d MiB", binName, filepath.Base(archive), maxBinaryBytes>>20)
	}
	// Closing a written file can report a late write error: a binary that is
	// not complete must not be renamed into place.
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// maxBinaryBytes bounds the size of a binary extracted from an archive.
const maxBinaryBytes = 1 << 30

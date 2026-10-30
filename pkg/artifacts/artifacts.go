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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bacanoo-io/norca-deployer/pkg/config"
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

	mu      sync.Mutex
	results map[string]*result
}

type result struct {
	once sync.Once
	file *File
	err  error
}

func NewResolver(plan *config.DeploymentPlan, cacheDir string) *Resolver {
	return &Resolver{
		plan:     plan,
		cacheDir: cacheDir,
		client:   &http.Client{Timeout: 10 * time.Minute},
		results:  map[string]*result{},
	}
}

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
func (r *Resolver) NorcaBinary(goos, arch string) (*File, error) {
	return r.ReleaseBinary("norca", goos, arch)
}

// ReleaseBinary returns a binary of the release archive for a platform.
func (r *Resolver) ReleaseBinary(name, goos, arch string) (*File, error) {
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
			version := strings.TrimPrefix(rel.Version, "v")
			ext := ".tar.gz"
			if goos == "windows" {
				ext = ".zip"
			}
			archiveName := fmt.Sprintf("norca_%s_%s_%s%s", version, goos, arch, ext)
			sumsName := fmt.Sprintf("norca_%s_SHA256SUMS", version)
			base := strings.TrimRight(rel.DownloadBaseURL, "/") + "/" + rel.Version + "/"
			dir := filepath.Join(r.cacheDir, "norca", version)

			archive := filepath.Join(dir, archiveName)
			want := strings.ToLower(rel.ChecksumSHA256)
			if want == "" {
				sums := filepath.Join(dir, sumsName)
				if err := r.download(base+sumsName, sums, ""); err != nil {
					return nil, err
				}
				var err error
				if want, err = lookupSum(sums, archiveName); err != nil {
					return nil, err
				}
			}
			if err := r.download(base+archiveName, archive, want); err != nil {
				return nil, err
			}
			dest := filepath.Join(dir, goos+"_"+arch, binName)
			if err := extractBinary(archive, binName, dest); err != nil {
				return nil, err
			}
			return fileWithDigest(dest)
		}
		return nil, fmt.Errorf("release source %q does not provide a binary", rel.Source)
	})
}

// CNIArchive returns the CNI plugin archive (.tgz) for a Linux architecture.
func (r *Resolver) CNIArchive(arch string) (*File, error) {
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
			name := fmt.Sprintf("cni-plugins-linux-%s-%s.tgz", arch, c.Version)
			url := strings.TrimRight(c.DownloadBaseURL, "/") + "/" + c.Version + "/" + name
			dir := filepath.Join(r.cacheDir, "cni", c.Version)
			want := strings.ToLower(c.ChecksumSHA256)
			if want == "" {
				sumFile := filepath.Join(dir, name+".sha256")
				if err := r.download(url+".sha256", sumFile, ""); err != nil {
					return nil, err
				}
				var err error
				if want, err = lookupSum(sumFile, name); err != nil {
					return nil, err
				}
			}
			dest := filepath.Join(dir, name)
			if err := r.download(url, dest, want); err != nil {
				return nil, err
			}
			return &File{Path: dest, SHA256: want}, nil
		}
	})
}

func expand(p, goos, arch string) string {
	return strings.NewReplacer("{os}", goos, "{arch}", arch).Replace(p)
}

func isArchive(p string) bool {
	return strings.HasSuffix(p, ".tar.gz") || strings.HasSuffix(p, ".tgz") || strings.HasSuffix(p, ".zip")
}

// download fetches url into dest unless dest already has the wanted digest.
// An empty want accepts any content (used for checksum files).
func (r *Resolver) download(url, dest, want string) error {
	if want != "" {
		if got, err := FileSHA256(dest); err == nil && got == want {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return err
	}
	fmt.Printf("    Downloading %s\n", url)
	resp, err := r.client.Get(url)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %s", url, resp.Status)
	}
	tmp := dest + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("download %s: %w", url, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); want != "" && got != want {
		os.Remove(tmp)
		return fmt.Errorf("download %s: sha256 %s does not match the expected %s", url, got, want)
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
				src, closer = rc, func() { rc.Close() }
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
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0700)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	out.Close()
	return os.Rename(tmp, dest)
}

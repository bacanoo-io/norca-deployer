// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package gitops

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// readStackArchive reads an archive as the servers do (readStackArchive in
// rigger/stackfiles.go of Norca): within the bounds, every entry a regular
// file with a valid path that is there once, and one manifest that names the
// directory of the stack. It returns the entries in their order.
func readStackArchive(data []byte) (names []string, files map[string]string, dir string, err error) {
	if len(data) > 16<<20 {
		return nil, nil, "", fmt.Errorf("the archive is %d bytes", len(data))
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, nil, "", err
	}
	defer gz.Close()

	tr := tar.NewReader(io.LimitReader(gz, 64<<20+1))
	files = map[string]string{}
	manifests := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, "", err
		}
		if _, seen := files[h.Name]; h.Typeflag != tar.TypeReg || !fs.ValidPath(h.Name) || seen {
			return nil, nil, "", fmt.Errorf("the entry %q is not a file of a stack", h.Name)
		}
		if len(files) >= 4096 {
			return nil, nil, "", errors.New("more than 4096 files")
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return nil, nil, "", err
		}
		names = append(names, h.Name)
		if h.Name == ".rigger-stack.json" {
			var m struct {
				Dir string `json:"dir"`
			}
			if err := json.Unmarshal(body, &m); err != nil {
				return nil, nil, "", err
			}
			dir = m.Dir
			manifests++
			continue
		}
		files[h.Name] = string(body)
	}
	if manifests != 1 || !fs.ValidPath(dir) {
		return nil, nil, "", fmt.Errorf("%d manifests, directory %q", manifests, dir)
	}
	return names, files, dir, nil
}

func TestStackArchive_IsWhatTheServersRead(t *testing.T) {
	stack := map[string][]byte{
		"stack.hcl":               []byte(stackHCL),
		"rigger.lock.hcl":         []byte("lock"),
		"values/prod.vars.hcl":    []byte("replicas = 3\n"),
		"modules/web/module.hcl":  []byte("module"),
		"modules/web/empty.nomad": {},
	}
	hidden := map[string][]byte{
		".env":                    []byte("TOKEN=secret"),
		".rigger/generated.json":  []byte("secret"),
		".rigger-stack.json":      []byte(`{"dir":"elsewhere"}`),
		"modules/.git/config":     []byte("x"),
		"modules/web/.gitkeep":    {},
		"values/.cache/a/b.hcl":   []byte("x"),
		".github/workflows/x.yml": []byte("x"),
	}
	all := maps.Clone(stack)
	maps.Copy(all, hidden)

	archive, err := StackArchive(all)
	if err != nil {
		t.Fatal(err)
	}
	names, files, dir, err := readStackArchive(archive)
	if err != nil {
		t.Fatalf("the servers would refuse the archive: %v", err)
	}

	// The stack is at the root of the archive.
	if dir != "." {
		t.Errorf("directory %q", dir)
	}
	// The files of the stack at their paths, and nothing whose name starts
	// with a dot: not a file, not a directory, not a manifest of the
	// repository's own.
	if len(files) != len(stack) {
		t.Errorf("files %v", slices.Sorted(maps.Keys(files)))
	}
	for name, want := range stack {
		if got, ok := files[name]; !ok || got != string(want) {
			t.Errorf("%s: %q, in the archive %t", name, got, ok)
		}
	}
	// The manifest comes first, then the files by name.
	want := append([]string{".rigger-stack.json"}, slices.Sorted(maps.Keys(stack))...)
	if !slices.Equal(names, want) {
		t.Errorf("entries %v, want %v", names, want)
	}

	// The same files give the same archive: the servers record its digest.
	again, err := StackArchive(maps.Clone(all))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(archive, again) {
		t.Error("the same files gave another archive")
	}
}

func TestStackArchive_Bounds(t *testing.T) {
	// The bounds are those of the servers.
	if serverStackLimits != (stackLimits{archiveBytes: 16 << 20, bytes: 64 << 20, files: 4096}) {
		t.Fatalf("limits %+v", serverStackLimits)
	}

	files := func(n, size int) map[string][]byte {
		out := map[string][]byte{"stack.hcl": []byte(stackHCL)}
		for i := range n - 1 {
			out[fmt.Sprintf("files/%04d.txt", i)] = bytes.Repeat([]byte("x"), size)
		}
		return out
	}
	// Bytes that do not compress.
	noise := make([]byte, 64<<10)
	src := rand.NewChaCha8([32]byte{1})
	_, _ = src.Read(noise)

	for name, tc := range map[string]struct {
		files  map[string][]byte
		limits stackLimits
		want   string // empty: taken
	}{
		"as many files as allowed":                       {files(8, 1), stackLimits{archiveBytes: 1 << 20, bytes: 1 << 20, files: 8}, ""},
		"one file more":                                  {files(9, 1), stackLimits{archiveBytes: 1 << 20, bytes: 1 << 20, files: 8}, "more than 8 files"},
		"hidden files do not count":                      {map[string][]byte{"stack.hcl": {}, ".a": {}, ".b": {}}, stackLimits{archiveBytes: 1 << 20, bytes: 1 << 20, files: 1}, ""},
		"too large uncompressed":                         {files(4, 16<<10), stackLimits{archiveBytes: 1 << 20, bytes: 32 << 10, files: 8}, "bytes uncompressed"},
		"too large compressed":                           {map[string][]byte{"stack.hcl": noise}, stackLimits{archiveBytes: 32 << 10, bytes: 1 << 20, files: 8}, "bytes compressed"},
		"within both sizes":                              {map[string][]byte{"stack.hcl": noise}, stackLimits{archiveBytes: 128 << 10, bytes: 128 << 10, files: 8}, ""},
		"the manifest and the tar count as uncompressed": {map[string][]byte{"stack.hcl": bytes.Repeat([]byte("x"), 1024)}, stackLimits{archiveBytes: 1 << 20, bytes: 1024, files: 8}, "bytes uncompressed"},
	} {
		t.Run(name, func(t *testing.T) {
			archive, err := buildStackArchive(tc.files, tc.limits)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if _, _, _, err := readStackArchive(archive); err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestStackArchive_Refusals(t *testing.T) {
	for name, files := range map[string]map[string][]byte{
		"no files":              {},
		"hidden files only":     {".env": []byte("x")},
		"a path that leaves":    {"stack.hcl": {}, "../x": {}},
		"a rooted path":         {"stack.hcl": {}, "/etc/passwd": {}},
		"an empty element":      {"stack.hcl": {}, "a//b": {}},
		"a path through a dot":  {"stack.hcl": {}, "a/./b": {}},
		"a path with backslash": {"stack.hcl": {}, `a\..\b`: {}},
	} {
		t.Run(name, func(t *testing.T) {
			if archive, err := StackArchive(files); err == nil {
				t.Fatalf("must be refused, got %d bytes", len(archive))
			}
		})
	}
}

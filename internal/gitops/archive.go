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
	"slices"
	"strings"
)

// A stack is sent to the servers as files: the archive that the Files of a
// stack request hold. Its format is defined by rigger/stackfiles.go in the
// Norca repository (Stack.Archive writes it, readStackArchive reads it): a
// gzip compressed tar of the files of the stack at their paths, and a
// manifest that names the directory of the stack in the archive. The
// deployer imports no package of Rigger or Norca, so it writes the format
// itself.

// stackManifestName is the name of the manifest in the archive.
const stackManifestName = ".rigger-stack.json"

// stackFile is the file a directory must have at its root to be a stack.
const stackFile = "stack.hcl"

// stackManifest is the manifest of an archive.
type stackManifest struct {
	// Dir is the directory of the stack in the archive.
	Dir string `json:"dir"`
}

// stackLimits bound an archive: its size compressed, the size of the tar in
// it, and its number of files.
type stackLimits struct {
	archiveBytes int
	bytes        int64
	files        int
}

// serverStackLimits are the bounds of the servers. They read the tar
// through a reader of that many bytes, so its headers count too.
var serverStackLimits = stackLimits{archiveBytes: 16 << 20, bytes: 64 << 20, files: 4096}

// StackArchive packs the files of a stack directory, by their paths below
// it, into the archive the servers take; the stack is at its root. Files
// and directories whose name starts with a dot are left out, as Rigger
// leaves them out: they are no part of a stack, and a repository keeps
// secrets in some (.env). The same files give the same archive.
func StackArchive(files map[string][]byte) ([]byte, error) {
	return buildStackArchive(files, serverStackLimits)
}

func buildStackArchive(files map[string][]byte, limits stackLimits) ([]byte, error) {
	names := make([]string, 0, len(files))
	for name := range files {
		// A path the servers would refuse, or read as another one.
		if !fs.ValidPath(name) || strings.Contains(name, `\`) {
			return nil, fmt.Errorf("%q is not the path of a file of a stack", name)
		}
		if !hiddenPath(name) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, errors.New("the stack has no files")
	}
	if len(names) > limits.files {
		return nil, fmt.Errorf("the stack has more than %d files", limits.files)
	}
	slices.Sort(names)

	manifest, err := json.Marshal(stackManifest{Dir: "."})
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	plain := &countingWriter{w: gz}
	tw := tar.NewWriter(plain)

	err = writeArchiveFile(tw, stackManifestName, manifest)
	for _, name := range names {
		if err != nil || plain.n > limits.bytes {
			break
		}
		err = writeArchiveFile(tw, name, files[name])
	}
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = gz.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("packing the stack: %w", err)
	}

	if plain.n > limits.bytes {
		return nil, fmt.Errorf("the archive of the stack is more than %d bytes uncompressed", limits.bytes)
	}
	if buf.Len() > limits.archiveBytes {
		return nil, fmt.Errorf("the archive of the stack is %d bytes compressed, more than %d", buf.Len(), limits.archiveBytes)
	}
	return buf.Bytes(), nil
}

// hiddenPath reports whether a file or one of its directories has a name
// that starts with a dot.
func hiddenPath(name string) bool {
	for element := range strings.SplitSeq(name, "/") {
		if strings.HasPrefix(element, ".") {
			return true
		}
	}
	return false
}

func writeArchiveFile(tw *tar.Writer, name string, data []byte) error {
	hdr := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data))}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// countingWriter counts the bytes written through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

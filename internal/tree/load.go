// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tree

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
)

// The kinds of the plain files of a tree, by the extension of their names
// (design §8): the extension decides the kind, the directory only sorts.
const (
	KindJob             = "job"
	KindNamespace       = "namespace"
	KindQuota           = "quota"
	KindACLPolicy       = "acl-policy"
	KindVariable        = "variable"
	KindObjectKind      = "object-kind"
	KindAdmissionPolicy = "admission-policy"
	KindExternalService = "external-service"
	KindCustomObject    = "custom-object"
	// KindSecret is a secret manifest of namespaces/<ns>/secrets/, which
	// becomes a secret block of the stack of its namespace.
	KindSecret = "secret"
)

// extensions maps the extension of a plain file to its kind, the longest
// first where one ends another.
var extensions = []struct{ ext, kind string }{
	{".norca.hcl", KindJob},
	{".namespace.hcl", KindNamespace},
	{".quota.hcl", KindQuota},
	{".acl.hcl", KindACLPolicy},
	{".var.hcl", KindVariable},
	{".kind.hcl", KindObjectKind},
	{".admission.hcl", KindAdmissionPolicy},
	{".external.hcl", KindExternalService},
	{".object.hcl", KindCustomObject},
}

// globalKinds are the kinds global/ holds, namespaceKinds those of
// namespaces/<ns>/ (design §4).
var (
	globalKinds    = []string{KindNamespace, KindQuota, KindACLPolicy, KindAdmissionPolicy, KindObjectKind}
	namespaceKinds = []string{KindJob, KindVariable, KindExternalService, KindCustomObject}
)

// kindDirs is the directory of each kind, for the warning about a file in
// the directory of another kind.
var kindDirs = map[string]string{
	KindJob: "jobs", KindNamespace: "namespaces", KindQuota: "quotas", KindACLPolicy: "acl",
	KindVariable: "variables", KindObjectKind: "object-kinds", KindAdmissionPolicy: "admission-policies",
	KindExternalService: "services", KindCustomObject: "objects",
}

// notSupported are the directories of kinds Rigger has no resource kind for
// yet (design §4.1, Kinds): a file in them is refused, never ignored.
var notSupported = []string{
	"global/acl/roles", "global/acl/auth-methods", "global/acl/binding-rules", "global/node-pools",
	"global/network-policies", "kv", "modules", "stacks",
}

// namespaceNotSupported are such directories inside namespaces/<ns>/.
var namespaceNotSupported = []string{"network-policies", "intentions", "volumes"}

// reservedFiles are file names of the design that this version does not
// read yet; a tree that has one is refused rather than half understood.
var reservedFiles = []string{"_ignore.hcl"}

// namespaceName is the rule of the name of a namespace directory: the
// release tree-<ns> must be a release name (at most 63 characters).
var namespaceName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,57}$`)

// The limits of the length of a path below the root (design §4, G10).
const (
	pathWarnLength = 200
	pathMaxLength  = 240
)

// File is a file of a tree.
type File struct {
	// Path is the path below the root of the tree, with slashes.
	Path string
	// Kind is the kind of the file (KindJob, ...).
	Kind string
	// Name is the name of the object: the file name before its extension,
	// or for a secret its path below secrets/.
	Name string
	Data []byte
}

// Namespace is the files of a directory namespaces/<ns>/.
type Namespace struct {
	Name    string
	Files   []File
	Secrets []File
}

// Tree is a tree as it was read: its manifest and its files.
type Tree struct {
	Manifest   *Manifest
	Global     []File
	Namespaces []*Namespace
	// Warnings are what the reader found odd but read anyway.
	Warnings []string
}

// Load reads a tree: its manifest, the raw list of its paths, which is
// checked before any file is read (design §4, file-system hazards), and its
// plain files.
func Load(fsys fs.FS) (*Tree, error) {
	paths, warnings, err := walk(fsys)
	if err != nil {
		return nil, err
	}

	m, err := LoadManifest(fsys)
	if err != nil {
		return nil, err
	}

	t := &Tree{Manifest: m, Warnings: warnings}
	namespaces := map[string]*Namespace{}

	var errs []error

	for _, p := range paths {
		if err := t.add(fsys, p, namespaces); err != nil {
			errs = append(errs, err)
		}
	}

	for _, name := range slices.Sorted(maps.Keys(namespaces)) {
		t.Namespaces = append(t.Namespaces, namespaces[name])
	}

	return t, errors.Join(errs...)
}

// add sorts one file into the tree.
func (t *Tree) add(fsys fs.FS, p string, namespaces map[string]*Namespace) error {
	parts := strings.Split(p, "/")

	if len(parts) == 1 {
		// The manifest and the files for people at the root.
		return nil
	}

	for _, dir := range notSupported {
		if p == dir || strings.HasPrefix(p, dir+"/") {
			return fmt.Errorf("%s: %s is not supported yet", p, dir+"/")
		}
	}

	switch parts[0] {
	case "global":
		f, err := plainFile(fsys, p, globalKinds)
		if err != nil {
			return err
		}

		t.warnDir(f, parts[1:])
		t.Global = append(t.Global, f)

		return nil
	case "namespaces":
		return t.addNamespaced(fsys, p, parts, namespaces)
	}

	return fmt.Errorf("%s: %s/ is not a directory of a tree (global/, namespaces/)", p, parts[0])
}

// addNamespaced sorts a file of namespaces/<ns>/ into its namespace.
func (t *Tree) addNamespaced(fsys fs.FS, p string, parts []string, namespaces map[string]*Namespace) error {
	if len(parts) < 3 {
		return fmt.Errorf("%s: a file of namespaces/ is in the directory of its namespace", p)
	}

	name := parts[1]
	if !namespaceName.MatchString(name) {
		return fmt.Errorf("%s: namespace %q: lower case letters, digits and -, at most 58, so that its release tree-%s is a release name", p, name, name)
	}

	if slices.Contains(namespaceNotSupported, parts[2]) {
		return fmt.Errorf("%s: namespaces/%s/%s/ is not supported yet", p, name, parts[2])
	}

	ns := namespaces[name]
	if ns == nil {
		ns = &Namespace{Name: name}
		namespaces[name] = ns
	}

	if parts[2] == "secrets" {
		secret, ok := strings.CutSuffix(strings.Join(parts[3:], "/"), ".hcl")
		if !ok || secret == "" {
			return fmt.Errorf("%s: a secret is a file <path>.hcl below secrets/", p)
		}

		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}

		ns.Secrets = append(ns.Secrets, File{Path: p, Kind: KindSecret, Name: secret, Data: data})

		return nil
	}

	f, err := plainFile(fsys, p, namespaceKinds)
	if err != nil {
		return err
	}

	t.warnDir(f, parts[2:])
	ns.Files = append(ns.Files, f)

	return nil
}

// plainFile reads a plain file whose kind must be one of allowed.
func plainFile(fsys fs.FS, p string, allowed []string) (File, error) {
	base := path.Base(p)
	kind, name := kindOf(base)

	switch {
	case kind == "":
		return File{}, fmt.Errorf("%s: the extension names no kind (.norca.hcl, .namespace.hcl, .quota.hcl, .acl.hcl, .var.hcl, .kind.hcl, .admission.hcl, .external.hcl, .object.hcl)", p)
	case !slices.Contains(allowed, kind):
		return File{}, fmt.Errorf("%s: a %s does not belong in %s/", p, kind, strings.SplitN(p, "/", 2)[0])
	case name == "":
		return File{}, fmt.Errorf("%s: the file name before the extension is the name of the object", p)
	}

	data, err := fs.ReadFile(fsys, p)
	if err != nil {
		return File{}, err
	}

	return File{Path: p, Kind: kind, Name: name, Data: data}, nil
}

// kindOf returns the kind of a file name and the name before its extension;
// "" for a name whose extension names no kind.
func kindOf(base string) (kind, name string) {
	for _, e := range extensions {
		if n, ok := strings.CutSuffix(base, e.ext); ok {
			return e.kind, n
		}
	}

	return "", ""
}

// warnDir warns about a file whose directory names another kind: the
// extension decides, the directory only sorts (design §4, naming rules).
func (t *Tree) warnDir(f File, dirs []string) {
	if len(dirs) < 2 {
		return
	}

	dir := dirs[0]
	if want := kindDirs[f.Kind]; want != "" && dir != want && slices.Contains(slices.Collect(maps.Values(kindDirs)), dir) {
		t.Warnings = append(t.Warnings, fmt.Sprintf("%s: a %s in %s/ (the extension decides the kind)", f.Path, f.Kind, dir))
	}
}

// walk lists the files of a tree and checks the raw paths before any file
// is read: no symbolic link, no name that breaks a checkout on another file
// system (case, characters, names and length that Windows refuses).
func walk(fsys fs.FS) ([]string, []string, error) {
	var (
		paths    []string
		warnings []string
		errs     []error
	)

	lower := map[string]string{}

	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if p == "." {
			return nil
		}

		base := d.Name()
		if ignored(base) {
			if slices.Contains(reservedFiles, base) {
				errs = append(errs, fmt.Errorf("%s: %s is reserved and not supported yet", p, base))
			}

			if d.IsDir() {
				return fs.SkipDir
			}

			return nil
		}

		if d.Type()&fs.ModeSymlink != 0 {
			errs = append(errs, fmt.Errorf("%s: a symbolic link (the tree has none)", p))

			return nil
		}

		if msg := badName(base); msg != "" {
			errs = append(errs, fmt.Errorf("%s: %s", p, msg))
		}

		switch n := len(p); {
		case n > pathMaxLength:
			errs = append(errs, fmt.Errorf("%s: the path is %d characters long, at most %d", p, n, pathMaxLength))
		case n > pathWarnLength:
			warnings = append(warnings, fmt.Sprintf("%s: the path is %d characters long (more than %d breaks some checkouts)", p, n, pathWarnLength))
		}

		key := strings.ToLower(p)
		if other, ok := lower[key]; ok {
			errs = append(errs, fmt.Errorf("%s and %s differ only by case", other, p))
		}

		lower[key] = p

		if !d.IsDir() {
			paths = append(paths, p)
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	return paths, warnings, errors.Join(errs...)
}

// ignored reports whether a name is left out of the tree: drafts and
// snippets start with _ or . (design §4, naming rules).
func ignored(base string) bool {
	return strings.HasPrefix(base, "_") || strings.HasPrefix(base, ".")
}

// windowsReserved are the device names Windows refuses as file names, with
// any extension.
var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// badName says why a file or directory name breaks a checkout on Windows;
// "" when it does not.
func badName(base string) string {
	if i := strings.IndexAny(base, `<>:"|?*\`); i >= 0 {
		return fmt.Sprintf("the character %q is not allowed in a name", base[i])
	}

	for _, r := range base {
		if r < 0x20 {
			return "a control character is not allowed in a name"
		}
	}

	if strings.HasSuffix(base, ".") || strings.HasSuffix(base, " ") {
		return "a name does not end with a dot or a space"
	}

	stem, _, _ := strings.Cut(base, ".")
	if windowsReserved[strings.ToUpper(stem)] {
		return fmt.Sprintf("%s is a reserved name on Windows", stem)
	}

	return ""
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tree

import (
	"fmt"
	"slices"
	"strings"
)

// kindOfResource maps the resource types of the release API to the kind
// identifiers of a tree (design §8.1); they are the same but for objects.
var kindOfResource = map[string]string{
	"quota": KindQuota, "namespace": KindNamespace, "acl-policy": KindACLPolicy, "variable": KindVariable,
	"object-kind": KindObjectKind, "admission-policy": KindAdmissionPolicy, "external-service": KindExternalService,
	"object": KindCustomObject, "job": KindJob,
}

// allKinds are the kind identifiers a tree knows today.
var allKinds = []string{
	KindACLPolicy, KindQuota, KindNamespace, KindAdmissionPolicy, KindObjectKind,
	KindVariable, KindSecret, KindExternalService, KindJob, KindCustomObject,
}

// kindGroups are the groups of design §8.1, as far as their kinds exist.
var kindGroups = map[string][]string{
	"acl":  {KindACLPolicy},
	"data": {KindVariable, KindSecret},
	"foundation": {
		KindACLPolicy, KindQuota, KindNamespace, KindAdmissionPolicy, KindObjectKind,
		KindVariable, KindSecret, KindExternalService,
	},
	"all": allKinds,
}

// resourceKind is the kind identifier of a resource type of the release
// API; an unknown type is itself.
func resourceKind(resourceType string) string {
	if k, ok := kindOfResource[resourceType]; ok {
		return k
	}

	return resourceType
}

// KindSelector turns kind identifiers and groups into a set; empty is nil,
// which allows every kind. An unknown word is an error: a typo must never
// switch a limit off (design §8.1, strict by default).
func KindSelector(words []string) (map[string]bool, error) {
	if len(words) == 0 {
		return nil, nil
	}

	set := map[string]bool{}

	for _, w := range words {
		w = strings.TrimSpace(w)

		switch {
		case slices.Contains(allKinds, w):
			set[w] = true
		case kindGroups[w] != nil:
			for _, k := range kindGroups[w] {
				set[k] = true
			}
		default:
			return nil, fmt.Errorf("%q is no kind (%s) and no group (acl, data, foundation, all)", w, strings.Join(allKinds, ", "))
		}
	}

	return set, nil
}

// IntersectKinds is the kinds both sets allow; nil allows all.
func IntersectKinds(a, b map[string]bool) map[string]bool {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}

	out := map[string]bool{}

	for k := range a {
		if b[k] {
			out[k] = true
		}
	}

	return out
}

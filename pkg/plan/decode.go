// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package plan

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"

	"github.com/hashicorp/hcl"
	"gopkg.in/yaml.v3"
)

// LabelledBlock marks a struct whose HCL blocks carry a label that is its
// name: region "eu-1" { ... } decodes as an item with name "eu-1", as a
// YAML list item with name: eu-1. The struct needs a name field.
type LabelledBlock interface {
	LabelledBlock()
}

// labelledBlock is the type of LabelledBlock.
var labelledBlock = reflect.TypeFor[LabelledBlock]()

// DecodeInto decodes a document of another format of the deployer (the
// manifest of a tree) into target, a pointer to a struct with yaml tags,
// the way a plan is decoded: HCL is shaped after the type of target and
// decoded through the YAML decoder, so both formats share keys and checks.
// Unlike a plan, an unknown key is an error in both formats. JSON is read
// as YAML.
func DecodeInto(data []byte, format Format, target any) error {
	if t := reflect.TypeOf(target); t == nil || t.Kind() != reflect.Pointer || t.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("decoding into %T: the target is no pointer to a struct", target)
	}

	switch format {
	case FormatHCL:
		var raw map[string]any
		if err := hcl.Decode(&raw, string(data)); err != nil {
			return fmt.Errorf("invalid HCL syntax: %w", err)
		}

		shaped, err := shape(raw, reflect.TypeOf(target).Elem(), "")
		if err != nil {
			return err
		}

		if data, err = yaml.Marshal(shaped); err != nil {
			return err
		}
	case FormatYAML, FormatJSON, "":
	default:
		return fmt.Errorf("unknown format %q (yaml, hcl or json)", format)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	if err := dec.Decode(target); err != nil && !errors.Is(err, io.EOF) {
		return err
	}

	return nil
}

package config

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/hashicorp/hcl"
	"gopkg.in/yaml.v3"
)

// decodeHCL loads an HCL plan. HCL uses the same keys as YAML: nested objects
// are blocks and lists of objects are repeated blocks. The generic HCL decoder
// returns every block as a list of objects, so the result is first shaped
// after the plan types and then decoded through the YAML decoder, which keeps
// one decoding path (and its defaults and checks) for both formats.
func decodeHCL(data []byte, plan *DeploymentPlan) error {
	var raw map[string]any
	if err := hcl.Decode(&raw, string(data)); err != nil {
		return err
	}
	shaped, err := shape(raw, reflect.TypeOf(plan).Elem(), "")
	if err != nil {
		return err
	}
	out, err := yaml.Marshal(shaped)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(out, plan)
}

// shape converts a generic HCL value to the form of the target type.
func shape(v any, t reflect.Type, path string) (any, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		obj, err := single(v, path)
		if err != nil {
			return nil, err
		}
		res := map[string]any{}
		for key, val := range obj {
			f, ok := fieldByTag(t, key)
			if !ok {
				return nil, fmt.Errorf("%s: unknown key %q", where(path), key)
			}
			s, err := shape(val, f.Type, join(path, key))
			if err != nil {
				return nil, err
			}
			res[key] = s
		}
		return res, nil
	case reflect.Map:
		obj, err := single(v, path)
		if err != nil {
			return nil, err
		}
		res := map[string]any{}
		for key, val := range obj {
			s, err := shape(val, t.Elem(), join(path, key))
			if err != nil {
				return nil, err
			}
			res[key] = s
		}
		return res, nil
	case reflect.Slice:
		list, ok := v.([]any)
		if !ok {
			// Repeated blocks arrive as []map[string]interface{}.
			if maps, isMaps := v.([]map[string]any); isMaps {
				for _, m := range maps {
					list = append(list, m)
				}
			} else {
				return nil, fmt.Errorf("%s: expected a list", where(path))
			}
		}
		if t.Elem() == reflect.TypeFor[GitOpsBinding]() {
			var err error
			if list, err = expandLabelledBindings(list, path); err != nil {
				return nil, err
			}
		}
		res := make([]any, 0, len(list))
		for i, item := range list {
			s, err := shape(item, t.Elem(), fmt.Sprintf("%s[%d]", path, i))
			if err != nil {
				return nil, err
			}
			res = append(res, s)
		}
		return res, nil
	case reflect.Interface:
		return generic(v), nil
	default:
		return v, nil
	}
}

// single returns the one object of a block. A block decodes as a list of
// objects; more than one means the block was repeated where only one is allowed.
func single(v any, path string) (map[string]any, error) {
	switch o := v.(type) {
	case map[string]any:
		return o, nil
	case []map[string]any:
		if len(o) == 1 {
			return o[0], nil
		}
		return nil, fmt.Errorf("%s: block given %d times", where(path), len(o))
	}
	return nil, fmt.Errorf("%s: expected a block", where(path))
}

// generic unwraps the single-object lists of free-form values (pack values).
func generic(v any) any {
	switch o := v.(type) {
	case []map[string]any:
		if len(o) == 1 {
			return generic(o[0])
		}
		res := make([]any, len(o))
		for i, m := range o {
			res[i] = generic(m)
		}
		return res
	case map[string]any:
		res := map[string]any{}
		for k, val := range o {
			res[k] = generic(val)
		}
		return res
	case []any:
		res := make([]any, len(o))
		for i, val := range o {
			res[i] = generic(val)
		}
		return res
	}
	return v
}

func fieldByTag(t reflect.Type, key string) (reflect.StructField, bool) {
	for f := range t.Fields() {
		name, opts, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if f.Anonymous && opts == "inline" {
			// Keys of an inlined struct are keys of this block.
			if inner, ok := fieldByTag(f.Type, key); ok {
				return inner, true
			}
			continue
		}
		if name == key {
			return f, true
		}
	}
	return reflect.StructField{}, false
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func where(path string) string {
	if path == "" {
		return "plan"
	}
	return path
}

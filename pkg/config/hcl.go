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
	var raw map[string]interface{}
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
func shape(v interface{}, t reflect.Type, path string) (interface{}, error) {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		obj, err := single(v, path)
		if err != nil {
			return nil, err
		}
		res := map[string]interface{}{}
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
		res := map[string]interface{}{}
		for key, val := range obj {
			s, err := shape(val, t.Elem(), join(path, key))
			if err != nil {
				return nil, err
			}
			res[key] = s
		}
		return res, nil
	case reflect.Slice:
		list, ok := v.([]interface{})
		if !ok {
			// Repeated blocks arrive as []map[string]interface{}.
			if maps, isMaps := v.([]map[string]interface{}); isMaps {
				for _, m := range maps {
					list = append(list, m)
				}
			} else {
				return nil, fmt.Errorf("%s: expected a list", where(path))
			}
		}
		res := make([]interface{}, 0, len(list))
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
func single(v interface{}, path string) (map[string]interface{}, error) {
	switch o := v.(type) {
	case map[string]interface{}:
		return o, nil
	case []map[string]interface{}:
		if len(o) == 1 {
			return o[0], nil
		}
		return nil, fmt.Errorf("%s: block given %d times", where(path), len(o))
	}
	return nil, fmt.Errorf("%s: expected a block", where(path))
}

// generic unwraps the single-object lists of free-form values (pack values).
func generic(v interface{}) interface{} {
	switch o := v.(type) {
	case []map[string]interface{}:
		if len(o) == 1 {
			return generic(o[0])
		}
		res := make([]interface{}, len(o))
		for i, m := range o {
			res[i] = generic(m)
		}
		return res
	case map[string]interface{}:
		res := map[string]interface{}{}
		for k, val := range o {
			res[k] = generic(val)
		}
		return res
	case []interface{}:
		res := make([]interface{}, len(o))
		for i, val := range o {
			res[i] = generic(val)
		}
		return res
	}
	return v
}

func fieldByTag(t reflect.Type, key string) (reflect.StructField, bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
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

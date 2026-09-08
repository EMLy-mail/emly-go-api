package remoteconfig

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
)

// UnknownFieldPaths walks data against the Document schema (recursively,
// following every struct/map/slice field) and returns the JSON-pointer-style
// paths of keys present in data that no field in the schema recognizes.
//
// Parse itself ignores unknown fields on purpose (client spec §7: "a newer
// server can add fields without breaking older updaters"), so this is not
// part of validation. It exists for the admin create-revision endpoint (API
// design doc §7.2 step 2), which reports these as warnings: an operator's
// typo in a field name ("/updater/certficate") is otherwise silently
// dropped by the canonical round-trip with no trace in the response.
func UnknownFieldPaths(data []byte) ([]string, error) {
	var raw interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	var unknown []string
	walkUnknown(raw, reflect.TypeOf(Document{}), "", &unknown)
	return unknown, nil
}

// walkUnknown checks an object value against a struct type's known JSON
// field names, recursing into recognized fields. Non-struct/map targets (a
// PatchDoc's arbitrary content, a []string of server names, a plain scalar)
// have nothing further to check and are left alone.
func walkUnknown(value interface{}, t reflect.Type, path string, out *[]string) {
	t = deref(t)
	obj, isObj := value.(map[string]interface{})
	if !isObj || t == nil {
		return
	}

	switch t.Kind() {
	case reflect.Struct:
		fields := jsonFieldTypes(t)
		for key, v := range obj {
			childPath := path + "/" + key
			ft, known := fields[key]
			if !known {
				*out = append(*out, childPath)
				continue
			}
			walkUnknownValue(v, ft, childPath, out)
		}
	case reflect.Map:
		elemType := t.Elem()
		for key, v := range obj {
			walkUnknownValue(v, elemType, path+"/"+key, out)
		}
	}
}

func walkUnknownValue(v interface{}, t reflect.Type, path string, out *[]string) {
	t = deref(t)
	if t == nil {
		return
	}
	switch t.Kind() {
	case reflect.Struct:
		walkUnknown(v, t, path, out)
	case reflect.Map:
		if obj, ok := v.(map[string]interface{}); ok {
			elemType := t.Elem()
			for key, vv := range obj {
				walkUnknownValue(vv, elemType, path+"/"+key, out)
			}
		}
	case reflect.Slice:
		if arr, ok := v.([]interface{}); ok {
			elemType := t.Elem()
			for i, vv := range arr {
				walkUnknownValue(vv, elemType, path+"/"+strconv.Itoa(i), out)
			}
		}
	}
}

func deref(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}

// jsonFieldTypes maps a struct type's JSON field names to their Go type,
// per the same rule encoding/json itself uses to pick a field's wire name.
func jsonFieldTypes(t reflect.Type) map[string]reflect.Type {
	out := make(map[string]reflect.Type, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "" {
			name = f.Name
		}
		if name == "-" {
			continue
		}
		out[name] = f.Type
	}
	return out
}

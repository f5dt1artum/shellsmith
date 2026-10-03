package command

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// This file adds a config-file entry point to the declarative parser:
// (*Parser).ParseWithConfigFiles reads a set of JSON or YAML files, merges
// them into the same config map ParseWithSources already accepts, and
// delegates the actual parsing — including option type conversion and
// required checks — to ParseWithSources. The command tree, help and
// completion outputs are unaffected.

// Sentinel errors describing config-file failures. Each is wrapped in a
// *ConfigError that also carries the offending path; callers distinguish
// classes with errors.Is and recover the path with errors.As.
var (
	// ErrUnsupportedConfigFormat indicates a file whose extension is not
	// .json, .yaml or .yml (matched case-insensitively).
	ErrUnsupportedConfigFormat = errors.New("command: unsupported config file format")

	// ErrConfigIO indicates a config file that could not be opened or
	// read.
	ErrConfigIO = errors.New("command: config file I/O error")

	// ErrInvalidConfig indicates a config file whose syntax or structure
	// is invalid: a JSON or YAML syntax error, a non-object root, a
	// nested object, a nested array, a null array element, or a
	// duplicated top-level key.
	ErrInvalidConfig = errors.New("command: invalid config file")
)

// ConfigError wraps one of the config-file sentinels and reports the path
// of the file that failed.
type ConfigError struct {
	// Path is the config file path the failure refers to.
	Path string

	kind error
	msg  string
}

func (e *ConfigError) Error() string { return e.msg }
func (e *ConfigError) Unwrap() error { return e.kind }

// ParseWithConfigFiles decodes args with an additional config layer built
// from the JSON or YAML files at paths. The file format is selected by the
// file extension — .json, .yaml or .yml, case-insensitively — and every
// file's top level must be an object whose keys correspond to Option
// ConfigKey values. A scalar yields one raw value and an array of scalars
// yields several in order: strings are kept verbatim, booleans normalize
// to "true" or "false", and numbers convert to decimal text. Keys no
// option binds are ignored.
//
// paths are ordered by ascending priority: a key present in a later file
// replaces the whole value group from earlier files, while a key absent
// from a later file inherits the earlier value. A null or empty-array
// entry clears the key, so it is no longer supplied by the config layer
// and falls back to lower layers and the option default. The overall
// precedence is args, then environ, then the merged files, then the
// option's Default — exactly ParseWithSources with the merged map.
//
// Every file is fully read and structurally validated before any merging
// or parsing happens. On failure the returned result is nil and the error
// is a *ConfigError wrapping ErrUnsupportedConfigFormat, ErrConfigIO or
// ErrInvalidConfig with Path set; no partially merged state is exposed.
// After merging, conversion and required-check failures behave exactly as
// in ParseWithSources: a *ParseError whose Source is SourceConfig and
// whose Name and Value point at the effective key and value. Values a
// higher layer already supplies are never converted, but their files must
// still be syntactically and structurally valid.
//
// The process environment is never read; neither args, paths nor environ
// (nor the slices inside them) is modified or retained; nothing is logged
// or printed. The method is safe for concurrent use on a shared Parser.
func (p *Parser) ParseWithConfigFiles(args []string, paths []string, environ map[string]string) (*Result, error) {
	files := make([]map[string][]string, 0, len(paths))
	for _, path := range paths {
		values, err := loadConfigFile(path)
		if err != nil {
			return nil, err
		}
		files = append(files, values)
	}

	// Merge ascending so later files overwrite earlier ones per key. A
	// nil group marks a cleared key (null or empty array): it removes
	// the key from lower files instead of supplying values.
	merged := make(map[string][]string)
	for _, file := range files {
		for key, values := range file {
			if values == nil {
				delete(merged, key)
				continue
			}
			merged[key] = values
		}
	}
	return p.ParseWithSources(args, merged, environ)
}

// loadConfigFile reads and validates one config file, returning its raw
// value groups keyed by top-level key. A nil group marks a key cleared by
// a null or empty array.
func loadConfigFile(path string) (map[string][]string, error) {
	var parse func([]byte) (map[string][]string, error)
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		parse = parseJSONConfig
	case ".yaml", ".yml":
		parse = parseYAMLConfig
	default:
		return nil, &ConfigError{Path: path, kind: ErrUnsupportedConfigFormat,
			msg: fmt.Sprintf("command: unsupported config file format %q", path)}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &ConfigError{Path: path, kind: ErrConfigIO,
			msg: fmt.Sprintf("command: cannot read config file %q: %v", path, err)}
	}
	values, err := parse(data)
	if err != nil {
		return nil, &ConfigError{Path: path, kind: ErrInvalidConfig,
			msg: fmt.Sprintf("command: invalid config file %q: %v", path, err)}
	}
	return values, nil
}

// parseJSONConfig decodes one JSON config document. The top level must be
// an object; duplicate keys are rejected, which a plain map decode cannot
// detect, so the object is streamed token by token.
func parseJSONConfig(data []byte) (map[string][]string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("top-level value must be an object")
	}

	out := make(map[string][]string)
	for dec.More() {
		ktok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := ktok.(string)
		if !ok {
			return nil, errors.New("object key must be a string")
		}
		var value any
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("duplicate top-level key %q", key)
		}
		values, err := jsonConfigValues(value)
		if err != nil {
			return nil, fmt.Errorf("key %q: %v", key, err)
		}
		out[key] = values
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("unexpected content after top-level object")
	}
	return out, nil
}

// jsonConfigValues converts one decoded JSON value to its raw value group.
// A nil group means the key is cleared.
func jsonConfigValues(value any) ([]string, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case string:
		return []string{v}, nil
	case bool:
		return []string{strconv.FormatBool(v)}, nil
	case json.Number:
		return []string{normalizeJSONNumber(v)}, nil
	case []any:
		if len(v) == 0 {
			return nil, nil
		}
		values := make([]string, 0, len(v))
		for _, elem := range v {
			switch e := elem.(type) {
			case string:
				values = append(values, e)
			case bool:
				values = append(values, strconv.FormatBool(e))
			case json.Number:
				values = append(values, normalizeJSONNumber(e))
			case nil:
				return nil, errors.New("null array element is not allowed")
			default:
				return nil, errors.New("array elements must be scalars")
			}
		}
		return values, nil
	default:
		return nil, errors.New("object values are not allowed")
	}
}

// normalizeJSONNumber renders a JSON number as decimal text. Integer
// literals keep their exact text; fractions and exponents are expanded
// through float64.
func normalizeJSONNumber(n json.Number) string {
	s := n.String()
	if !strings.ContainsAny(s, ".eE") {
		return s
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return s
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// parseYAMLConfig decodes one YAML config document. The top level must be
// a mapping with scalar keys and scalar or scalar-sequence values.
func parseYAMLConfig(data []byte) (map[string][]string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, errors.New("empty document")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("top-level value must be a mapping")
	}

	out := make(map[string][]string)
	for i := 0; i+1 < len(root.Content); i += 2 {
		keyNode, valueNode := root.Content[i], root.Content[i+1]
		key, ok, err := yamlConfigKey(keyNode)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("duplicate top-level key %q", key)
		}
		values, err := yamlConfigValues(valueNode)
		if err != nil {
			return nil, fmt.Errorf("key %q: %v", key, err)
		}
		out[key] = values
	}
	return out, nil
}

// yamlConfigKey renders a top-level mapping key. A null key contributes no
// entry; a non-scalar key is a structural error.
func yamlConfigKey(n *yaml.Node) (string, bool, error) {
	if n.Kind != yaml.ScalarNode {
		return "", false, errors.New("mapping keys must be scalars")
	}
	if n.Tag == "!!null" {
		return "", false, nil
	}
	key, err := yamlScalarText(n)
	if err != nil {
		return "", false, err
	}
	return key, true, nil
}

// yamlConfigValues converts one YAML value node to its raw value group. A
// nil group means the key is cleared.
func yamlConfigValues(n *yaml.Node) ([]string, error) {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag == "!!null" {
			return nil, nil
		}
		text, err := yamlScalarText(n)
		if err != nil {
			return nil, err
		}
		return []string{text}, nil
	case yaml.SequenceNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		values := make([]string, 0, len(n.Content))
		for _, elem := range n.Content {
			if elem.Kind != yaml.ScalarNode {
				return nil, errors.New("array elements must be scalars")
			}
			if elem.Tag == "!!null" {
				return nil, errors.New("null array element is not allowed")
			}
			text, err := yamlScalarText(elem)
			if err != nil {
				return nil, err
			}
			values = append(values, text)
		}
		return values, nil
	default:
		return nil, errors.New("nested objects are not allowed")
	}
}

// yamlScalarText renders a scalar node as raw config text: strings
// verbatim, booleans normalized to "true"/"false", numbers as decimal
// text, and any other scalar (such as a timestamp) as its source text.
func yamlScalarText(n *yaml.Node) (string, error) {
	var v any
	if err := n.Decode(&v); err != nil {
		return "", err
	}
	switch t := v.(type) {
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	case int:
		return strconv.Itoa(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case uint64:
		return strconv.FormatUint(t, 10), nil
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	default:
		return n.Value, nil
	}
}

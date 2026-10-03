package command

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// This file adds a configuration-file entry point to the parser:
// (*Parser).ParseConfigFiles loads a set of JSON or YAML files, merges
// them into a single config layer, and hands the result to the existing
// ParseWithSources machinery, so option type conversion and required
// checks keep their established semantics.

// Sentinel errors describing configuration-file failures. Each is wrapped
// in a *ConfigError that carries the offending path; callers distinguish
// classes with errors.Is and recover the path with errors.As.
var (
	// ErrUnsupportedConfigFormat indicates a file whose extension is not
	// .json, .yaml or .yml (matched case-insensitively).
	ErrUnsupportedConfigFormat = errors.New("command: unsupported config file format")

	// ErrConfigIO indicates a config file that could not be opened or
	// read.
	ErrConfigIO = errors.New("command: config file I/O error")

	// ErrInvalidConfig indicates a config file whose syntax or structure
	// is not a valid configuration: a non-object root, an object value, a
	// nested array, a null array element, or a duplicated top-level key.
	ErrInvalidConfig = errors.New("command: invalid config file")
)

// ConfigError wraps one of the configuration-file sentinels and identifies
// the file that failed.
type ConfigError struct {
	// Path is the config file path exactly as passed to ParseConfigFiles.
	Path string

	kind error
	msg  string
}

func (e *ConfigError) Error() string { return e.msg }
func (e *ConfigError) Unwrap() error { return e.kind }

// ParseConfigFiles decodes args with additional value layers loaded from
// the given JSON or YAML configuration files. Precedence is fixed,
// command line first: args, then environ, then the files in path order
// (a later file outranks an earlier one), then the option's Default.
//
// Each file's format is selected by its extension, case-insensitively:
// .json, .yaml or .yml. The top level of a file must be an object whose
// keys correspond to Option.ConfigKey values; unknown keys are ignored.
// A scalar value supplies one raw value and an array of scalars supplies
// several in order: strings are kept verbatim, booleans are canonicalized
// to "true" or "false", and numbers are rendered as decimal text. A null
// value or an empty array clears the key: values from lower-priority
// files are removed and the key is no longer supplied by the config
// layer, so the option falls back to lower layers or its default. When
// the same key appears in several files the higher-priority file
// replaces the whole value group; keys a file does not mention are
// inherited from lower-priority files.
//
// Object values, nested arrays, null array elements, duplicated
// top-level keys and non-object roots are invalid configurations. Every
// file is fully read and structurally validated before any merging
// happens: on the first failure the returned result is nil and the error
// is a *ConfigError wrapping ErrUnsupportedConfigFormat, ErrConfigIO or
// ErrInvalidConfig, with no partial merge handed to the caller.
//
// After merging, conversion and required checks are exactly those of
// ParseWithSources: an effective config value that fails type conversion
// yields a *ParseError wrapping ErrInvalidValue with SourceConfig, while
// values shadowed by a higher layer are never converted (their files
// must still be syntactically and structurally valid).
//
// ParseConfigFiles never reads the process environment, does not modify
// or retain args, paths or environ, produces no output, and is safe for
// concurrent use on a shared Parser.
func (p *Parser) ParseConfigFiles(args []string, paths []string, environ map[string]string) (*Result, error) {
	if len(paths) == 0 {
		return p.ParseWithSources(args, nil, environ)
	}

	// Phase 1: read and validate every file. Nothing is merged until all
	// of them have succeeded, so a failure cannot leak a partial result.
	files := make([]map[string][]string, len(paths))
	for i, path := range paths {
		entries, err := loadConfigFile(path)
		if err != nil {
			return nil, err
		}
		files[i] = entries
	}

	// Phase 2: merge in ascending priority order. A non-empty group
	// replaces the lower-priority group wholesale; an empty group (null
	// or empty array) removes the key from the config layer entirely.
	merged := make(map[string][]string)
	for _, entries := range files {
		for key, values := range entries {
			if len(values) == 0 {
				delete(merged, key)
				continue
			}
			merged[key] = values
		}
	}

	// Phase 3: reuse the established layering, conversion and required
	// semantics.
	return p.ParseWithSources(args, merged, environ)
}

// loadConfigFile reads and validates one configuration file, returning
// its top-level entries as raw value groups. A key mapped to an empty
// slice was explicitly cleared (null or empty array).
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

	entries, err := parse(data)
	if err != nil {
		return nil, &ConfigError{Path: path, kind: ErrInvalidConfig,
			msg: fmt.Sprintf("command: invalid config file %q: %v", path, err)}
	}
	return entries, nil
}

// parseJSONConfig decodes one JSON configuration document. The decoder is
// driven token by token so that duplicated top-level keys and disallowed
// value shapes are reported instead of silently accepted.
func parseJSONConfig(data []byte) (map[string][]string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("syntax error: %v", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("top level must be an object")
	}

	entries := make(map[string][]string)
	for dec.More() {
		ktok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("syntax error: %v", err)
		}
		key, ok := ktok.(string)
		if !ok {
			return nil, fmt.Errorf("expected object key, got %v", ktok)
		}
		if _, dup := entries[key]; dup {
			return nil, fmt.Errorf("duplicate top-level key %q", key)
		}

		vtok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("syntax error: %v", err)
		}
		if d, ok := vtok.(json.Delim); ok {
			if d != '[' {
				return nil, fmt.Errorf("key %q: object values are not allowed", key)
			}
			values, err := jsonArray(dec, key)
			if err != nil {
				return nil, err
			}
			entries[key] = values
			continue
		}
		raw, isNull, err := jsonScalar(vtok)
		if err != nil {
			return nil, fmt.Errorf("key %q: %v", key, err)
		}
		if isNull {
			entries[key] = nil
			continue
		}
		entries[key] = []string{raw}
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("syntax error: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("unexpected content after top-level object")
	}
	return entries, nil
}

// jsonArray consumes one JSON array of scalars, including the closing
// delimiter. Nested arrays, objects and null elements are rejected.
func jsonArray(dec *json.Decoder, key string) ([]string, error) {
	values := []string{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("syntax error: %v", err)
		}
		raw, isNull, err := jsonScalar(tok)
		if err != nil {
			return nil, fmt.Errorf("key %q: %v", key, err)
		}
		if isNull {
			return nil, fmt.Errorf("key %q: null array elements are not allowed", key)
		}
		values = append(values, raw)
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("syntax error: %v", err)
	}
	return values, nil
}

// jsonScalar renders one JSON scalar token as a raw config value. A nil
// token is JSON null, reported via isNull; anything else (a delimiter) is
// a structural error.
func jsonScalar(tok any) (raw string, isNull bool, err error) {
	switch v := tok.(type) {
	case nil:
		return "", true, nil
	case string:
		return v, false, nil
	case bool:
		return strconv.FormatBool(v), false, nil
	case json.Number:
		text, err := decimalText(string(v))
		if err != nil {
			return "", false, fmt.Errorf("invalid number %q", string(v))
		}
		return text, false, nil
	default:
		return "", false, errors.New("nested arrays and objects are not allowed")
	}
}

// parseYAMLConfig decodes one YAML configuration document into the same
// entry shape as parseJSONConfig. An empty file yields an empty object.
func parseYAMLConfig(data []byte) (map[string][]string, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if err == io.EOF {
			return map[string][]string{}, nil
		}
		return nil, fmt.Errorf("syntax error: %v", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("multiple documents are not allowed")
	}

	root := &doc
	if root.Kind == yaml.DocumentNode {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("top level must be an object")
	}

	entries := make(map[string][]string)
	for i := 0; i < len(root.Content); i += 2 {
		kn, vn := root.Content[i], resolveAlias(root.Content[i+1])
		if kn.Kind != yaml.ScalarNode {
			return nil, errors.New("non-scalar top-level keys are not allowed")
		}
		key := kn.Value
		if _, dup := entries[key]; dup {
			return nil, fmt.Errorf("duplicate top-level key %q", key)
		}

		switch vn.Kind {
		case yaml.ScalarNode:
			raw, isNull, err := yamlScalar(vn)
			if err != nil {
				return nil, fmt.Errorf("key %q: %v", key, err)
			}
			if isNull {
				entries[key] = nil
				continue
			}
			entries[key] = []string{raw}
		case yaml.SequenceNode:
			values := []string{}
			for _, item := range vn.Content {
				item = resolveAlias(item)
				if item.Kind != yaml.ScalarNode {
					return nil, fmt.Errorf("key %q: nested arrays and objects are not allowed", key)
				}
				raw, isNull, err := yamlScalar(item)
				if err != nil {
					return nil, fmt.Errorf("key %q: %v", key, err)
				}
				if isNull {
					return nil, fmt.Errorf("key %q: null array elements are not allowed", key)
				}
				values = append(values, raw)
			}
			entries[key] = values
		default:
			return nil, fmt.Errorf("key %q: object values are not allowed", key)
		}
	}
	return entries, nil
}

// resolveAlias follows a YAML alias to its target node.
func resolveAlias(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}

// yamlScalar renders one YAML scalar node as a raw config value: strings
// are kept verbatim, booleans are canonicalized to "true" or "false",
// numbers become decimal text, and null is reported via isNull.
func yamlScalar(n *yaml.Node) (raw string, isNull bool, err error) {
	switch n.Tag {
	case "!!null":
		return "", true, nil
	case "!!bool":
		var b bool
		if err := n.Decode(&b); err != nil {
			return "", false, fmt.Errorf("invalid boolean %q", n.Value)
		}
		return strconv.FormatBool(b), false, nil
	case "!!int", "!!float":
		var v any
		if err := n.Decode(&v); err != nil {
			return "", false, fmt.Errorf("invalid number %q", n.Value)
		}
		switch num := v.(type) {
		case int:
			return strconv.Itoa(num), false, nil
		case int64:
			return strconv.FormatInt(num, 10), false, nil
		case uint64:
			return strconv.FormatUint(num, 10), false, nil
		case float64:
			return strconv.FormatFloat(num, 'f', -1, 64), false, nil
		default:
			return "", false, fmt.Errorf("invalid number %q", n.Value)
		}
	default:
		// !!str and any other scalar tag (e.g. timestamps) keep the
		// source text verbatim.
		return n.Value, false, nil
	}
}

// decimalText renders a JSON number as decimal text. Integers are kept
// verbatim; fractional and exponent forms are expanded (1e3 becomes
// "1000", 1.50 becomes "1.5").
func decimalText(s string) (string, error) {
	if isIntegerText(s) {
		return s, nil
	}
	f, _, err := big.ParseFloat(s, 10, 256, big.ToNearestEven)
	if err != nil {
		return "", err
	}
	return f.Text('f', -1), nil
}

// isIntegerText reports whether s is a plain decimal integer.
func isIntegerText(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '-' || s[0] == '+' {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

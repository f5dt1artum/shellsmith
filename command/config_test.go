package command

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func writeConfigFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func configParser(t *testing.T) *Parser {
	t.Helper()
	p, err := NewParser([]Option{
		{Long: "host", Type: TypeString, Default: "localhost", ConfigKey: "host", EnvVar: "APP_HOST"},
		{Long: "port", Type: TypeInt, Default: "8080", ConfigKey: "port", EnvVar: "APP_PORT"},
		{Long: "timeout", Type: TypeDuration, ConfigKey: "timeout"},
		{Long: "verbose", Type: TypeBool, ConfigKey: "verbose"},
		{Long: "tag", Type: TypeString, Repeatable: true, ConfigKey: "tags"},
		{Long: "num", Type: TypeInt, Repeatable: true, ConfigKey: "nums"},
		{Long: "need", Type: TypeString, Required: true, ConfigKey: "need"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseWithConfigFilesJSON(t *testing.T) {
	p := configParser(t)
	path := writeConfigFile(t, "app.json", `{
		"host": "example.com",
		"port": 9090,
		"timeout": "250ms",
		"verbose": true,
		"tags": ["a", "b"],
		"nums": [1, -2, 3],
		"need": "yes",
		"unknown-key": "ignored"
	}`)
	r, err := p.ParseWithConfigFiles(nil, []string{path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.String("host"); got != "example.com" {
		t.Fatalf("host = %q", got)
	}
	if got := r.Int("port"); got != 9090 {
		t.Fatalf("port = %d", got)
	}
	if got := r.Duration("timeout"); got != 250*time.Millisecond {
		t.Fatalf("timeout = %v", got)
	}
	if !r.Bool("verbose") {
		t.Fatal("verbose = false")
	}
	if got := r.Strings("tag"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("tag = %v", got)
	}
	if got := r.Ints("num"); !reflect.DeepEqual(got, []int{1, -2, 3}) {
		t.Fatalf("num = %v", got)
	}
	if r.Source("host") != SourceConfig || r.Source("tag") != SourceConfig {
		t.Fatalf("sources: host=%v tag=%v", r.Source("host"), r.Source("tag"))
	}
}

func TestParseWithConfigFilesYAML(t *testing.T) {
	p := configParser(t)
	path := writeConfigFile(t, "app.yaml", `
host: yaml-host
port: 7070
verbose: false
tags:
  - x
  - "y z"
need: ok
`)
	r, err := p.ParseWithConfigFiles(nil, []string{path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.String("host"); got != "yaml-host" {
		t.Fatalf("host = %q", got)
	}
	if got := r.Int("port"); got != 7070 {
		t.Fatalf("port = %d", got)
	}
	if r.Bool("verbose") {
		t.Fatal("verbose = true")
	}
	if got := r.Strings("tag"); !reflect.DeepEqual(got, []string{"x", "y z"}) {
		t.Fatalf("tag = %v", got)
	}
}

func TestParseWithConfigFilesYmlAndCaseInsensitiveExt(t *testing.T) {
	p := configParser(t)
	yml := writeConfigFile(t, "a.YML", "need: ok\nhost: from-yml\n")
	js := writeConfigFile(t, "b.JSON", `{"port": 1234}`)
	r, err := p.ParseWithConfigFiles(nil, []string{yml, js}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.String("host"); got != "from-yml" {
		t.Fatalf("host = %q", got)
	}
	if got := r.Int("port"); got != 1234 {
		t.Fatalf("port = %d", got)
	}
}

func TestParseWithConfigFilesNumberNormalization(t *testing.T) {
	p := configParser(t)
	js := writeConfigFile(t, "n.json", `{"port": 1e3, "need": "x", "nums": [2.0, 30]}`)
	r, err := p.ParseWithConfigFiles(nil, []string{js}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Int("port"); got != 1000 {
		t.Fatalf("port = %d", got)
	}
	if got := r.Ints("num"); !reflect.DeepEqual(got, []int{2, 30}) {
		t.Fatalf("num = %v", got)
	}

	yml := writeConfigFile(t, "n.yaml", "port: 0x10\nneed: x\n")
	r, err = p.ParseWithConfigFiles(nil, []string{yml}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Int("port"); got != 16 {
		t.Fatalf("yaml hex port = %d", got)
	}
}

func TestParseWithConfigFilesMergeAndClear(t *testing.T) {
	p := configParser(t)
	low := writeConfigFile(t, "low.json", `{
		"host": "low-host",
		"port": 1000,
		"tags": ["low"],
		"verbose": true,
		"need": "low"
	}`)
	high := writeConfigFile(t, "high.yaml", `
host: high-host
tags: [high-a, high-b]
verbose: null
port: []
`)
	r, err := p.ParseWithConfigFiles(nil, []string{low, high}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.String("host"); got != "high-host" {
		t.Fatalf("host = %q, want whole-group replace", got)
	}
	if got := r.Strings("tag"); !reflect.DeepEqual(got, []string{"high-a", "high-b"}) {
		t.Fatalf("tag = %v, want whole-group replace", got)
	}
	if r.Source("verbose") != SourceDefault || r.Bool("verbose") {
		t.Fatalf("verbose cleared by null: source=%v value=%v", r.Source("verbose"), r.Bool("verbose"))
	}
	if r.Source("port") != SourceDefault || r.Int("port") != 8080 {
		t.Fatalf("port cleared by empty array: source=%v value=%d", r.Source("port"), r.Int("port"))
	}
	if got := r.String("need"); got != "low" {
		t.Fatalf("need = %q, want inherited from low file", got)
	}
}

func TestParseWithConfigFilesPrecedence(t *testing.T) {
	p := configParser(t)
	path := writeConfigFile(t, "app.json", `{"host": "cfg", "port": 1, "need": "cfg"}`)
	r, err := p.ParseWithConfigFiles([]string{"--host", "cli"}, []string{path},
		map[string]string{"APP_HOST": "env", "APP_PORT": "2"})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.String("host"); got != "cli" || r.Source("host") != SourceCommandLine {
		t.Fatalf("host = %q source=%v, want command line", got, r.Source("host"))
	}
	if got := r.Int("port"); got != 2 || r.Source("port") != SourceEnvironment {
		t.Fatalf("port = %d source=%v, want environment", got, r.Source("port"))
	}
	if got := r.String("need"); got != "cfg" || r.Source("need") != SourceConfig {
		t.Fatalf("need = %q source=%v, want config", got, r.Source("need"))
	}
}

func TestParseWithConfigFilesConversionErrorPointsAtEffectiveValue(t *testing.T) {
	p := configParser(t)
	low := writeConfigFile(t, "low.json", `{"port": 9999, "need": "x"}`)
	high := writeConfigFile(t, "high.json", `{"port": "not-a-number"}`)
	_, err := p.ParseWithConfigFiles(nil, []string{low, high}, nil)
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *ParseError", err)
	}
	if !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("err = %v, want ErrInvalidValue", err)
	}
	if pe.Name != "port" || pe.Value != "not-a-number" || pe.Source != SourceConfig {
		t.Fatalf("ParseError = %+v", pe)
	}

	// A higher layer wins: the bad config value is never converted.
	r, err := p.ParseWithConfigFiles([]string{"--port", "5"}, []string{low, high}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Int("port"); got != 5 {
		t.Fatalf("port = %d", got)
	}
	r, err = p.ParseWithConfigFiles(nil, []string{low, high}, map[string]string{"APP_PORT": "6"})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Int("port"); got != 6 {
		t.Fatalf("port = %d", got)
	}
}

func TestParseWithConfigFilesRequiredFromConfig(t *testing.T) {
	p := configParser(t)
	path := writeConfigFile(t, "app.json", `{"need": "from-config"}`)
	r, err := p.ParseWithConfigFiles(nil, []string{path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.String("need"); got != "from-config" {
		t.Fatalf("need = %q", got)
	}

	// A cleared required key falls back below the config layer and fails.
	clear := writeConfigFile(t, "clear.json", `{"need": null}`)
	_, err = p.ParseWithConfigFiles(nil, []string{path, clear}, nil)
	if !errors.Is(err, ErrRequired) {
		t.Fatalf("err = %v, want ErrRequired", err)
	}
}

func TestParseWithConfigFilesErrors(t *testing.T) {
	p := configParser(t)
	good := writeConfigFile(t, "good.json", `{"need": "x"}`)

	cases := []struct {
		name    string
		paths   []string
		want    error
		wantPth string
	}{
		{"unsupported extension", []string{writeConfigFile(t, "a.toml", "need = 1")}, ErrUnsupportedConfigFormat, ""},
		{"unsupported no extension", []string{writeConfigFile(t, "config", "{}")}, ErrUnsupportedConfigFormat, ""},
		{"missing file", []string{filepath.Join(t.TempDir(), "nope.json")}, ErrConfigIO, ""},
		{"json syntax", []string{writeConfigFile(t, "bad.json", `{"need":`)}, ErrInvalidConfig, ""},
		{"json trailing", []string{writeConfigFile(t, "trail.json", `{} {}`)}, ErrInvalidConfig, ""},
		{"json array root", []string{writeConfigFile(t, "arr.json", `[1, 2]`)}, ErrInvalidConfig, ""},
		{"json scalar root", []string{writeConfigFile(t, "scalar.json", `"x"`)}, ErrInvalidConfig, ""},
		{"json nested object", []string{writeConfigFile(t, "nest.json", `{"need": {"a": 1}}`)}, ErrInvalidConfig, ""},
		{"json nested array", []string{writeConfigFile(t, "narr.json", `{"need": [["a"]]}`)}, ErrInvalidConfig, ""},
		{"json null element", []string{writeConfigFile(t, "nullelem.json", `{"need": ["a", null]}`)}, ErrInvalidConfig, ""},
		{"json duplicate key", []string{writeConfigFile(t, "dup.json", `{"need": "a", "need": "b"}`)}, ErrInvalidConfig, ""},
		{"yaml syntax", []string{writeConfigFile(t, "bad.yaml", "need: [unclosed")}, ErrInvalidConfig, ""},
		{"yaml scalar root", []string{writeConfigFile(t, "scalar.yaml", "just a string")}, ErrInvalidConfig, ""},
		{"yaml null root", []string{writeConfigFile(t, "null.yaml", "~")}, ErrInvalidConfig, ""},
		{"yaml empty document", []string{writeConfigFile(t, "empty.yaml", "")}, ErrInvalidConfig, ""},
		{"yaml nested mapping", []string{writeConfigFile(t, "nest.yaml", "need:\n  a: 1\n")}, ErrInvalidConfig, ""},
		{"yaml nested sequence", []string{writeConfigFile(t, "nseq.yaml", "need:\n  - [a]\n")}, ErrInvalidConfig, ""},
		{"yaml null element", []string{writeConfigFile(t, "nullelem.yaml", "need: [a, ~]")}, ErrInvalidConfig, ""},
		{"yaml duplicate key", []string{writeConfigFile(t, "dup.yaml", "need: a\nneed: b\n")}, ErrInvalidConfig, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A valid file first proves earlier files do not rescue a
			// later failure and no partial merge leaks out.
			r, err := p.ParseWithConfigFiles(nil, append([]string{good}, tc.paths...), nil)
			if r != nil {
				t.Fatalf("result = %v, want nil", r)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			var ce *ConfigError
			if !errors.As(err, &ce) {
				t.Fatalf("err = %v, want *ConfigError", err)
			}
			if ce.Path != tc.paths[0] {
				t.Fatalf("ConfigError.Path = %q, want %q", ce.Path, tc.paths[0])
			}
		})
	}
}

func TestParseWithConfigFilesUnknownKeysIgnored(t *testing.T) {
	p := configParser(t)
	path := writeConfigFile(t, "app.json", `{"nothing-bound": 1, "need": "x"}`)
	if _, err := p.ParseWithConfigFiles(nil, []string{path}, map[string]string{"UNRELATED": "y"}); err != nil {
		t.Fatal(err)
	}
}

func TestParseWithConfigFilesDoesNotMutateInputs(t *testing.T) {
	p := configParser(t)
	path := writeConfigFile(t, "app.json", `{"host": "cfg", "need": "x"}`)
	args := []string{"--port", "3"}
	paths := []string{path}
	env := map[string]string{"APP_HOST": "env"}
	if _, err := p.ParseWithConfigFiles(args, paths, env); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(args, []string{"--port", "3"}) {
		t.Fatalf("args mutated: %v", args)
	}
	if !reflect.DeepEqual(paths, []string{path}) {
		t.Fatalf("paths mutated: %v", paths)
	}
	if !reflect.DeepEqual(env, map[string]string{"APP_HOST": "env"}) {
		t.Fatalf("environ mutated: %v", env)
	}
	if got := os.Getenv("APP_HOST"); got != "" {
		t.Fatalf("process environment touched: %q", got)
	}
}

func TestParseWithConfigFilesConcurrent(t *testing.T) {
	p := configParser(t)
	path := writeConfigFile(t, "app.json", `{"host": "cfg", "need": "x"}`)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := p.ParseWithConfigFiles([]string{"--port", fmt.Sprint(i)}, []string{path}, nil)
			if err != nil {
				t.Error(err)
				return
			}
			if r.Int("port") != i || r.String("host") != "cfg" {
				t.Errorf("port=%d host=%q", r.Int("port"), r.String("host"))
			}
		}(i)
	}
	wg.Wait()
}

func TestParseWithConfigFilesNoFiles(t *testing.T) {
	p := configParser(t)
	r, err := p.ParseWithConfigFiles([]string{"--need", "cli"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Source("host") != SourceDefault || r.String("host") != "localhost" {
		t.Fatalf("host source=%v value=%q", r.Source("host"), r.String("host"))
	}
}

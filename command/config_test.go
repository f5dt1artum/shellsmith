package command

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// writeConfigFile creates a config file under t.TempDir() and returns its
// path.
func writeConfigFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseConfigFilesJSON(t *testing.T) {
	p := boundParser(t)
	// Unknown keys are ignored.
	path := writeConfigFile(t, "app.json", `{
		"host": "cfg-host",
		"port": 9090,
		"timeout": "250ms",
		"verbose": true,
		"tags": ["a", "b"],
		"nums": [1, 2, 3],
		"need": "from-file",
		"unrelated": "ignored"
	}`)

	r, err := p.ParseConfigFiles(nil, []string{path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.String("host"); got != "cfg-host" {
		t.Fatalf("host = %q", got)
	}
	if got := r.Int("port"); got != 9090 {
		t.Fatalf("port = %d", got)
	}
	if got := r.Duration("timeout"); got != 250*time.Millisecond {
		t.Fatalf("timeout = %v", got)
	}
	if !r.Bool("verbose") || r.Source("verbose") != SourceConfig {
		t.Fatalf("verbose = %v source=%v", r.Bool("verbose"), r.Source("verbose"))
	}
	if got := r.Strings("tag"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("tags = %v", got)
	}
	if got := r.Ints("num"); !reflect.DeepEqual(got, []int{1, 2, 3}) {
		t.Fatalf("nums = %v", got)
	}
	if got := r.String("need"); got != "from-file" {
		t.Fatalf("need = %q", got)
	}
}

func TestParseConfigFilesYAML(t *testing.T) {
	p := boundParser(t)
	path := writeConfigFile(t, "app.yaml", `
host: yaml-host
port: 9091
timeout: 1s
verbose: false
tags:
  - x
  - "y z"
nums: [4, 5]
need: yaml-need
`)
	r, err := p.ParseConfigFiles(nil, []string{path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.String("host"); got != "yaml-host" {
		t.Fatalf("host = %q", got)
	}
	if got := r.Int("port"); got != 9091 {
		t.Fatalf("port = %d", got)
	}
	if got := r.Duration("timeout"); got != time.Second {
		t.Fatalf("timeout = %v", got)
	}
	if r.Bool("verbose") {
		t.Fatal("verbose should be false")
	}
	if got := r.Strings("tag"); !reflect.DeepEqual(got, []string{"x", "y z"}) {
		t.Fatalf("tags = %v", got)
	}
	if got := r.Ints("num"); !reflect.DeepEqual(got, []int{4, 5}) {
		t.Fatalf("nums = %v", got)
	}
}

func TestParseConfigFilesExtensionCaseInsensitive(t *testing.T) {
	p := boundParser(t)
	jsonPath := writeConfigFile(t, "a.JSON", `{"need": "x"}`)
	if _, err := p.ParseConfigFiles(nil, []string{jsonPath}, nil); err != nil {
		t.Fatalf("a.JSON: %v", err)
	}
	for _, name := range []string{"b.YAML", "c.Yml", "d.yAmL"} {
		path := writeConfigFile(t, name, "need: x\n")
		if _, err := p.ParseConfigFiles(nil, []string{path}, nil); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestParseConfigFilesPrecedence(t *testing.T) {
	p := boundParser(t)
	low := writeConfigFile(t, "low.json", `{
		"host": "low-host", "port": 1000, "timeout": "1s",
		"tags": ["low"], "need": "low"
	}`)
	high := writeConfigFile(t, "high.yaml", `
host: high-host
tags: [high1, high2]
`)
	r, err := p.ParseConfigFiles(
		[]string{"--port", "42"},
		[]string{low, high},
		map[string]string{"APP_TIMEOUT": "3s"},
	)
	if err != nil {
		t.Fatal(err)
	}
	// Command line beats everything.
	if got := r.Int("port"); got != 42 || r.Source("port") != SourceCommandLine {
		t.Fatalf("port = %d source=%v", got, r.Source("port"))
	}
	// Environment beats files.
	if got := r.Duration("timeout"); got != 3*time.Second || r.Source("timeout") != SourceEnvironment {
		t.Fatalf("timeout = %v source=%v", got, r.Source("timeout"))
	}
	// Later file replaces the whole group of the earlier file.
	if got := r.String("host"); got != "high-host" {
		t.Fatalf("host = %q", got)
	}
	if got := r.Strings("tag"); !reflect.DeepEqual(got, []string{"high1", "high2"}) {
		t.Fatalf("tags = %v", got)
	}
	// Keys absent from the later file are inherited from the earlier one.
	if got := r.String("need"); got != "low" || r.Source("need") != SourceConfig {
		t.Fatalf("need = %q source=%v", got, r.Source("need"))
	}
	// Untouched keys fall back to defaults.
	if got := r.Int("port"); r.Source("verbose") != SourceDefault || got != 42 {
		t.Fatalf("verbose source=%v", r.Source("verbose"))
	}
}

func TestParseConfigFilesNullClears(t *testing.T) {
	p := boundParser(t)
	low := writeConfigFile(t, "low.json", `{"host": "low-host", "tags": ["a"], "need": "x"}`)
	high := writeConfigFile(t, "high.json", `{"host": null, "tags": []}`)

	r, err := p.ParseConfigFiles(nil, []string{low, high}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Null and empty array remove the key from the config layer, so the
	// defaults apply again.
	if got := r.String("host"); got != "localhost" || r.Source("host") != SourceDefault {
		t.Fatalf("host = %q source=%v", got, r.Source("host"))
	}
	if got := r.Strings("tag"); len(got) != 0 || r.Source("tag") != SourceDefault {
		t.Fatalf("tags = %v source=%v", got, r.Source("tag"))
	}
	if got := r.String("need"); got != "x" {
		t.Fatalf("need = %q", got)
	}

	// YAML null spellings behave the same.
	highYAML := writeConfigFile(t, "high.yaml", "host: ~\n")
	r, err = p.ParseConfigFiles(nil, []string{low, highYAML}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.String("host"); got != "localhost" {
		t.Fatalf("host = %q", got)
	}
}

func TestParseConfigFilesNumberForms(t *testing.T) {
	p, err := NewParser([]Option{
		{Long: "i", Type: TypeString, ConfigKey: "i"},
		{Long: "f", Type: TypeString, ConfigKey: "f"},
		{Long: "e", Type: TypeString, ConfigKey: "e"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := writeConfigFile(t, "n.json", `{"i": 120, "f": 1.50, "e": 1.2e3}`)
	r, err := p.ParseConfigFiles(nil, []string{path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.String("i"); got != "120" {
		t.Fatalf("i = %q", got)
	}
	if got := r.String("f"); got != "1.5" {
		t.Fatalf("f = %q", got)
	}
	if got := r.String("e"); got != "1200" {
		t.Fatalf("e = %q", got)
	}

	yamlPath := writeConfigFile(t, "n.yaml", "i: 0x78\nf: 1.50\ne: 1.2e3\n")
	r, err = p.ParseConfigFiles(nil, []string{yamlPath}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.String("i"); got != "120" {
		t.Fatalf("yaml i = %q", got)
	}
	if got := r.String("f"); got != "1.5" {
		t.Fatalf("yaml f = %q", got)
	}
	if got := r.String("e"); got != "1200" {
		t.Fatalf("yaml e = %q", got)
	}
}

func TestParseConfigFilesErrors(t *testing.T) {
	p := boundParser(t)
	good := writeConfigFile(t, "good.json", `{"need": "x"}`)

	cases := []struct {
		name    string
		content string
		ext     string
		want    error
	}{
		{"json syntax", `{"need": }`, ".json", ErrInvalidConfig},
		{"json root array", `[1, 2]`, ".json", ErrInvalidConfig},
		{"json root scalar", `"hi"`, ".json", ErrInvalidConfig},
		{"json object value", `{"need": {"a": 1}}`, ".json", ErrInvalidConfig},
		{"json nested array", `{"tags": [["a"]]}`, ".json", ErrInvalidConfig},
		{"json null element", `{"tags": ["a", null]}`, ".json", ErrInvalidConfig},
		{"json duplicate key", `{"need": "a", "need": "b"}`, ".json", ErrInvalidConfig},
		{"json trailing", `{"need": "a"} {"x": 1}`, ".json", ErrInvalidConfig},
		{"yaml syntax", "need: [unclosed\n", ".yaml", ErrInvalidConfig},
		{"yaml root array", "- a\n- b\n", ".yaml", ErrInvalidConfig},
		{"yaml root scalar", "just text\n", ".yaml", ErrInvalidConfig},
		{"yaml object value", "need:\n  a: 1\n", ".yaml", ErrInvalidConfig},
		{"yaml nested array", "tags:\n  - [a]\n", ".yaml", ErrInvalidConfig},
		{"yaml null element", "tags:\n  - a\n  - null\n", ".yaml", ErrInvalidConfig},
		{"yaml duplicate key", "need: a\nneed: b\n", ".yaml", ErrInvalidConfig},
		{"yaml multi document", "need: a\n---\nneed: b\n", ".yaml", ErrInvalidConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := writeConfigFile(t, "bad"+tc.ext, tc.content)
			// The good file comes first so a partial merge would be
			// visible; the failure must still yield a nil result.
			r, err := p.ParseConfigFiles(nil, []string{good, bad}, nil)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if r != nil {
				t.Fatalf("result = %v, want nil", r)
			}
			var ce *ConfigError
			if !errors.As(err, &ce) {
				t.Fatalf("err is not a *ConfigError: %T", err)
			}
			if ce.Path != bad {
				t.Fatalf("Path = %q, want %q", ce.Path, bad)
			}
		})
	}
}

func TestParseConfigFilesUnsupportedFormat(t *testing.T) {
	p := boundParser(t)
	path := writeConfigFile(t, "app.toml", "need = \"x\"")
	r, err := p.ParseConfigFiles(nil, []string{path}, nil)
	if !errors.Is(err, ErrUnsupportedConfigFormat) {
		t.Fatalf("err = %v", err)
	}
	if r != nil {
		t.Fatalf("result = %v, want nil", r)
	}
	var ce *ConfigError
	if !errors.As(err, &ce) || ce.Path != path {
		t.Fatalf("ConfigError = %+v", ce)
	}
}

func TestParseConfigFilesIOError(t *testing.T) {
	p := boundParser(t)
	missing := filepath.Join(t.TempDir(), "missing.json")
	r, err := p.ParseConfigFiles(nil, []string{missing}, nil)
	if !errors.Is(err, ErrConfigIO) {
		t.Fatalf("err = %v", err)
	}
	if r != nil {
		t.Fatalf("result = %v, want nil", r)
	}
	var ce *ConfigError
	if !errors.As(err, &ce) || ce.Path != missing {
		t.Fatalf("ConfigError = %+v", ce)
	}
}

func TestParseConfigFilesConversionError(t *testing.T) {
	p := boundParser(t)
	path := writeConfigFile(t, "bad.json", `{"port": "not-a-number", "need": "x"}`)
	r, err := p.ParseConfigFiles(nil, []string{path}, nil)
	if !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("err = %v", err)
	}
	if r != nil {
		t.Fatalf("result = %v, want nil", r)
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err is not a *ParseError: %T", err)
	}
	if pe.Name != "port" || pe.Value != "not-a-number" || pe.Source != SourceConfig {
		t.Fatalf("ParseError = %+v", pe)
	}
}

func TestParseConfigFilesShadowedValueNotConverted(t *testing.T) {
	p := boundParser(t)
	// The file's port value is invalid, but the command line supplies
	// port, so the config value is never converted. The file itself is
	// syntactically and structurally valid, so parsing succeeds.
	path := writeConfigFile(t, "app.json", `{"port": "not-a-number", "need": "x"}`)
	r, err := p.ParseConfigFiles([]string{"--port", "1"}, []string{path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Int("port"); got != 1 {
		t.Fatalf("port = %d", got)
	}

	// Same when the environment shadows the file.
	r, err = p.ParseConfigFiles(nil, []string{path}, map[string]string{"APP_PORT": "2"})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Int("port"); got != 2 {
		t.Fatalf("port = %d", got)
	}
}

func TestParseConfigFilesNoMutation(t *testing.T) {
	p := boundParser(t)
	path := writeConfigFile(t, "app.json", `{"host": "cfg", "need": "x"}`)
	args := []string{"--verbose"}
	paths := []string{path}
	environ := map[string]string{"APP_TIMEOUT": "1s"}
	if _, err := p.ParseConfigFiles(args, paths, environ); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(args, []string{"--verbose"}) {
		t.Fatalf("args mutated: %v", args)
	}
	if !reflect.DeepEqual(paths, []string{path}) {
		t.Fatalf("paths mutated: %v", paths)
	}
	if !reflect.DeepEqual(environ, map[string]string{"APP_TIMEOUT": "1s"}) {
		t.Fatalf("environ mutated: %v", environ)
	}
}

func TestParseConfigFilesConcurrent(t *testing.T) {
	p := boundParser(t)
	path := writeConfigFile(t, "app.json", `{"host": "cfg", "need": "x"}`)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := p.ParseConfigFiles(nil, []string{path}, nil)
			if err != nil {
				t.Error(err)
				return
			}
			if r.String("host") != "cfg" {
				t.Errorf("host = %q", r.String("host"))
			}
		}()
	}
	wg.Wait()
}

func TestParseConfigFilesEmptyYAML(t *testing.T) {
	p := boundParser(t)
	path := writeConfigFile(t, "empty.yaml", "# only a comment\n")
	r, err := p.ParseConfigFiles([]string{"--need", "x"}, []string{path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.String("host"); got != "localhost" {
		t.Fatalf("host = %q", got)
	}
}

func TestParseConfigFilesNoPaths(t *testing.T) {
	p := boundParser(t)
	r, err := p.ParseConfigFiles([]string{"--need", "x"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.String("need"); got != "x" {
		t.Fatalf("need = %q", got)
	}
}

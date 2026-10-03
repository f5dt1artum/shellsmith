package command

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func boundParser(t *testing.T) *Parser {
	t.Helper()
	p, err := NewParser([]Option{
		{Long: "host", Short: "h", Type: TypeString, Default: "localhost", ConfigKey: "host", EnvVar: "APP_HOST"},
		{Long: "port", Short: "p", Type: TypeInt, Default: "8080", ConfigKey: "port", EnvVar: "APP_PORT"},
		{Long: "timeout", Type: TypeDuration, ConfigKey: "timeout", EnvVar: "APP_TIMEOUT"},
		{Long: "verbose", Short: "v", Type: TypeBool, ConfigKey: "verbose", EnvVar: "APP_VERBOSE"},
		{Long: "tag", Type: TypeString, Repeatable: true, ConfigKey: "tags", EnvVar: "APP_TAG"},
		{Long: "num", Type: TypeInt, Repeatable: true, ConfigKey: "nums"},
		{Long: "need", Type: TypeString, Required: true, ConfigKey: "need", EnvVar: "APP_NEED"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseWithSourcesDefaults(t *testing.T) {
	p := boundParser(t)
	r, err := p.ParseWithSources([]string{"--need", "x"}, nil, map[string]string{"UNRELATED": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Source("host") != SourceDefault || r.String("host") != "localhost" {
		t.Fatalf("host source=%v value=%q", r.Source("host"), r.String("host"))
	}
	if r.Source("port") != SourceDefault || r.Int("port") != 8080 {
		t.Fatalf("port source=%v value=%d", r.Source("port"), r.Int("port"))
	}
	if r.Source("timeout") != SourceDefault || r.Duration("timeout") != 0 {
		t.Fatalf("timeout source=%v", r.Source("timeout"))
	}
	if r.Source("unknown") != SourceNone {
		t.Fatalf("unknown source = %v, want SourceNone", r.Source("unknown"))
	}
}

func TestSourcePrecedence(t *testing.T) {
	p := boundParser(t)
	config := map[string][]string{
		"host":    {"cfg-host"},
		"port":    {"9000"},
		"timeout": {"700ms"},
	}
	environ := map[string]string{
		"APP_HOST": "env-host",
		"APP_PORT": "7000",
	}
	r, err := p.ParseWithSources([]string{"--host", "cli-host", "--need", "x"}, config, environ)
	if err != nil {
		t.Fatal(err)
	}

	// Command line beats environment and config.
	if r.Source("host") != SourceCommandLine || r.String("host") != "cli-host" {
		t.Fatalf("host source=%v value=%q", r.Source("host"), r.String("host"))
	}
	// Environment beats config.
	if r.Source("port") != SourceEnvironment || r.Int("port") != 7000 {
		t.Fatalf("port source=%v value=%d", r.Source("port"), r.Int("port"))
	}
	// Config beats the default.
	if r.Source("timeout") != SourceConfig || r.Duration("timeout") != 700*time.Millisecond {
		t.Fatalf("timeout source=%v value=%v", r.Source("timeout"), r.Duration("timeout"))
	}

	// Higher layers are never converted or mixed in when a higher layer
	// supplies the option.
	bad := map[string][]string{"port": {"not-a-number"}}
	r2, err := p.ParseWithSources([]string{"--port", "1", "--need", "x"}, bad,
		map[string]string{"APP_NEED": "y"})
	if err != nil {
		t.Fatalf("invalid lower-layer values must be ignored when covered: %v", err)
	}
	if r2.Int("port") != 1 || r2.Source("port") != SourceCommandLine {
		t.Fatalf("port = %d source %v", r2.Int("port"), r2.Source("port"))
	}
}

func TestConfigMultipleValues(t *testing.T) {
	p := boundParser(t)
	config := map[string][]string{
		"host": {"first", "middle", "last"},
		"tags": {"a", "b", "c"},
		// Earlier invalid values for a non-repeatable option are never
		// reached: only the last element is taken.
		"port": {"not-a-number", "6500"},
	}
	r, err := p.ParseWithSources([]string{"--need", "x"}, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Source("host") != SourceConfig || r.String("host") != "last" {
		t.Fatalf("host = %q source %v, want last config element", r.String("host"), r.Source("host"))
	}
	if r.Source("tag") != SourceConfig || !reflect.DeepEqual(r.Strings("tag"), []string{"a", "b", "c"}) {
		t.Fatalf("tags = %v source %v", r.Strings("tag"), r.Source("tag"))
	}
	if r.Int("port") != 6500 {
		t.Fatalf("port = %d, want last element 6500", r.Int("port"))
	}

	// A bad last element fails non-repeatable conversion.
	_, err = p.ParseWithSources([]string{"--need", "x"},
		map[string][]string{"port": {"6500", "bad"}}, nil)
	var pe *ParseError
	if !errors.As(err, &pe) || !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("err = %v, want ErrInvalidValue", err)
	}
	if pe.Name != "port" || pe.Value != "bad" || pe.Source != SourceConfig || pe.Token != "" {
		t.Fatalf("error fields = %+v", pe)
	}

	// Any bad element fails repeatable conversion, carrying that raw value.
	_, err = p.ParseWithSources([]string{"--need", "x"},
		map[string][]string{"nums": {"1", "", "3"}}, nil)
	if !errors.As(err, &pe) || !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("err = %v, want ErrInvalidValue", err)
	}
	if pe.Name != "num" || pe.Value != "" || pe.Source != SourceConfig || pe.Token != "" {
		t.Fatalf("error fields = %+v", pe)
	}
}

func TestEnvironmentSingleValue(t *testing.T) {
	p := boundParser(t)
	r, err := p.ParseWithSources([]string{"--need", "x"}, nil,
		map[string]string{"APP_TAG": "envtag", "APP_VERBOSE": "true"})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Strings("tag"); !reflect.DeepEqual(got, []string{"envtag"}) {
		t.Fatalf("tags = %v, want single environment value", got)
	}
	if r.Source("tag") != SourceEnvironment || !r.Bool("verbose") || r.Source("verbose") != SourceEnvironment {
		t.Fatalf("source = tag:%v verbose:%v", r.Source("tag"), r.Source("verbose"))
	}
}

func TestLayersReplaceWholesale(t *testing.T) {
	p := boundParser(t)
	config := map[string][]string{"tags": {"cfg-a", "cfg-b"}}

	// Environment replaces all config values rather than prepending.
	r, err := p.ParseWithSources([]string{"--need", "x"}, config, map[string]string{"APP_TAG": "env-only"})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Strings("tag"); !reflect.DeepEqual(got, []string{"env-only"}) {
		t.Fatalf("env must replace config wholesale, got %v", got)
	}

	// Command line replaces environment wholesale.
	r, err = p.ParseWithSources([]string{"--tag", "cli", "--need", "x"}, config,
		map[string]string{"APP_TAG": "env-only"})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Strings("tag"); !reflect.DeepEqual(got, []string{"cli"}) {
		t.Fatalf("cli must replace env wholesale, got %v", got)
	}
	if r.Count("tag") != 1 || !r.Provided("tag") {
		t.Fatal("slice accessor follows layer, but Count must stay command-line-only")
	}
}

func TestEmptyValuesAreExplicit(t *testing.T) {
	p, err := NewParser([]Option{
		{Long: "s", Type: TypeString, Default: "dflt", ConfigKey: "s", EnvVar: "S"},
		{Long: "n", Type: TypeInt, ConfigKey: "n", EnvVar: "N"},
		{Long: "b", Type: TypeBool, ConfigKey: "b", EnvVar: "B"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Empty environment value is an explicit value per type.
	r, err := p.ParseWithSources(nil, nil, map[string]string{"S": ""})
	if err != nil {
		t.Fatal(err)
	}
	if r.String("s") != "" || r.Source("s") != SourceEnvironment {
		t.Fatalf("empty env string: value=%q source=%v", r.String("s"), r.Source("s"))
	}
	for _, key := range []string{"N", "B"} {
		_, err := p.ParseWithSources(nil, nil, map[string]string{key: ""})
		var pe *ParseError
		if !errors.As(err, &pe) || !errors.Is(err, ErrInvalidValue) || pe.Source != SourceEnvironment || pe.Token != "" {
			t.Fatalf("empty env %s: err = %v, want env invalid-value", key, err)
		}
	}

	// Empty config elements are explicit values too.
	r, err = p.ParseWithSources(nil, map[string][]string{"s": {""}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.String("s") != "" || r.Source("s") != SourceConfig {
		t.Fatalf("empty config element: value=%q source=%v", r.String("s"), r.Source("s"))
	}
	_, err = p.ParseWithSources(nil, map[string][]string{"n": {""}}, nil)
	var pe *ParseError
	if !errors.As(err, &pe) || !errors.Is(err, ErrInvalidValue) || pe.Source != SourceConfig || pe.Name != "n" {
		t.Fatalf("empty config int element: err = %v", err)
	}

	// A present but empty config slice means "not supplied".
	r, err = p.ParseWithSources(nil, map[string][]string{"s": {}, "n": {}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Source("s") != SourceDefault || r.String("s") != "dflt" {
		t.Fatalf("empty config slice must fall back to default: source=%v value=%q", r.Source("s"), r.String("s"))
	}
	if r.Source("n") != SourceDefault {
		t.Fatalf("empty config slice: source = %v, want default", r.Source("n"))
	}
}

func TestRequiredSatisfiedByAnyLayer(t *testing.T) {
	p := boundParser(t)

	// Config satisfies required.
	r, err := p.ParseWithSources(nil, map[string][]string{"need": {"from-config"}}, nil)
	if err != nil {
		t.Fatalf("config must satisfy required: %v", err)
	}
	if r.String("need") != "from-config" || r.Source("need") != SourceConfig {
		t.Fatalf("need = %q source %v", r.String("need"), r.Source("need"))
	}

	// Environment satisfies required.
	r, err = p.ParseWithSources(nil, nil, map[string]string{"APP_NEED": "from-env"})
	if err != nil {
		t.Fatalf("env must satisfy required: %v", err)
	}
	if r.String("need") != "from-env" || r.Source("need") != SourceEnvironment {
		t.Fatalf("need = %q source %v", r.String("need"), r.Source("need"))
	}

	// A present-but-empty config slice does not.
	_, err = p.ParseWithSources(nil, map[string][]string{"need": {}}, nil)
	if !errors.Is(err, ErrRequired) {
		t.Fatalf("empty config slice satisfied required: %v", err)
	}

	// A default still does not.
	pd, _ := NewParser([]Option{{Long: "x", Required: true, Default: "d", ConfigKey: "x"}}, nil)
	if _, err := pd.ParseWithSources(nil, nil, nil); !errors.Is(err, ErrRequired) {
		t.Fatalf("default satisfied required: %v", err)
	}
}

func TestProvidedAndCountStayCommandLineOnly(t *testing.T) {
	p := boundParser(t)
	r, err := p.ParseWithSources(
		[]string{"--host", "cli", "--need", "x"},
		map[string][]string{"port": {"9000"}, "tags": {"a", "b"}},
		map[string]string{"APP_TIMEOUT": "1s"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Provided("host") || r.Count("host") != 1 {
		t.Fatal("command-line option should be provided")
	}
	if r.Provided("port") || r.Count("port") != 0 || r.Int("port") != 9000 {
		t.Fatalf("config option: provided=%v count=%d value=%d", r.Provided("port"), r.Count("port"), r.Int("port"))
	}
	if r.Provided("timeout") || r.Count("timeout") != 0 || r.Duration("timeout") != time.Second {
		t.Fatalf("env option: provided=%v count=%d", r.Provided("timeout"), r.Count("timeout"))
	}
	if r.Provided("tag") || r.Count("tag") != 0 {
		t.Fatalf("config tags must not count as command-line occurrences")
	}
	if got := r.Strings("tag"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("tag effective values = %v", got)
	}
}

func TestUnknownBindingsIgnored(t *testing.T) {
	p := boundParser(t)
	_, err := p.ParseWithSources([]string{"--need", "x"},
		map[string][]string{"unrelated": {"1"}, "other": nil},
		map[string]string{"UNCLE_VAR": "x"})
	if err != nil {
		t.Fatalf("unknown bindings must be ignored: %v", err)
	}
}

func TestProcessEnvironmentNeverRead(t *testing.T) {
	const varName = "COMMAND_TEST_APP_HOST"
	os.Setenv(varName, "process-env-value")
	defer os.Unsetenv(varName)

	p, err := NewParser([]Option{{Long: "host", Default: "dflt", EnvVar: varName}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.ParseWithSources(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.String("host") != "dflt" || r.Source("host") != SourceDefault {
		t.Fatalf("parser read process environment: %q source %v", r.String("host"), r.Source("host"))
	}
	// Plain Parse ignores bindings as well.
	r2, err := p.Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Source("host") != SourceDefault {
		t.Fatalf("Parse honored EnvVar binding")
	}
}

func TestParseErrorFieldsForLayeredFailures(t *testing.T) {
	p := boundParser(t)

	_, err := p.ParseWithSources([]string{"--need", "x"}, nil, map[string]string{"APP_PORT": "abc"})
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *ParseError", err)
	}
	if !errors.Is(err, ErrInvalidValue) || pe.Name != "port" || pe.Value != "abc" ||
		pe.Source != SourceEnvironment || pe.Token != "" {
		t.Fatalf("env error fields = %+v", pe)
	}

	r, err := p.ParseWithSources([]string{"--need", "x"}, map[string][]string{"port": {"abc"}}, nil)
	if r != nil {
		t.Fatalf("partial result returned on failure: %v", r)
	}
	if !errors.As(err, &pe) || pe.Name != "port" || pe.Value != "abc" || pe.Source != SourceConfig {
		t.Fatalf("config error fields = %+v", pe)
	}

	// Command-line conversion failures keep SourceNone and the token.
	_, err = p.ParseWithSources([]string{"--port=abc", "--need", "x"}, nil, nil)
	if !errors.As(err, &pe) || pe.Source != SourceNone || pe.Token != "--port=abc" {
		t.Fatalf("cli error fields = %+v", pe)
	}
}

func TestNewParserRejectsDuplicateBindings(t *testing.T) {
	cases := []struct {
		label string
		opts  []Option
	}{
		{"duplicate config key", []Option{
			{Long: "a", ConfigKey: "shared"},
			{Long: "b", ConfigKey: "shared"},
		}},
		{"duplicate env var", []Option{
			{Long: "a", EnvVar: "SHARED"},
			{Long: "b", EnvVar: "SHARED"},
		}},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			p, err := NewParser(c.opts, nil)
			if p != nil {
				t.Fatalf("returned parser %v on invalid spec", p)
			}
			var se *SpecError
			if !errors.As(err, &se) || !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("err = %v, want *SpecError wrapping ErrInvalidSpec", err)
			}
			if se.Name != "b" {
				t.Fatalf("SpecError.Name = %q, want second option b", se.Name)
			}
		})
	}

	// Empty bindings never collide, and config and env namespaces are
	// independent.
	opts := []Option{
		{Long: "a", ConfigKey: "", EnvVar: ""},
		{Long: "b", ConfigKey: "", EnvVar: ""},
		{Long: "c", ConfigKey: "same", EnvVar: "same"},
		{Long: "d", ConfigKey: "d", EnvVar: "d"},
	}
	if _, err := NewParser(opts, nil); err != nil {
		t.Fatalf("empty bindings and cross-namespace reuse must be valid: %v", err)
	}
}

func TestParseWithSourcesDoesNotMutateInputs(t *testing.T) {
	p := boundParser(t)
	args := []string{"--host", "cli", "--need", "x"}
	config := map[string][]string{
		"port": {"9000", "9001"},
		"tags": {"a", "b"},
	}
	environ := map[string]string{"APP_PORT": "7000", "APP_VERBOSE": "true"}

	argsCopy := append([]string(nil), args...)
	configCopy := map[string][]string{}
	for k, v := range config {
		configCopy[k] = append([]string(nil), v...)
	}
	envCopy := map[string]string{}
	for k, v := range environ {
		envCopy[k] = v
	}

	if _, err := p.ParseWithSources(args, config, environ); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(args, argsCopy) {
		t.Fatalf("args mutated: %v", args)
	}
	if !reflect.DeepEqual(config, configCopy) {
		t.Fatalf("config mutated: %v", config)
	}
	if !reflect.DeepEqual(environ, envCopy) {
		t.Fatalf("environ mutated: %v", environ)
	}

	// Also on failure. Timeout has no CLI/environ value, so a bad config
	// element is reached and rejected.
	bad := map[string][]string{"timeout": {"oops"}}
	badCopy := map[string][]string{"timeout": {"oops"}}
	if _, err := p.ParseWithSources(args, bad, environ); err == nil {
		t.Fatal("expected conversion failure")
	}
	if !reflect.DeepEqual(bad, badCopy) {
		t.Fatalf("config mutated on failure: %v", bad)
	}
	if !reflect.DeepEqual(args, argsCopy) {
		t.Fatalf("args mutated on failure: %v", args)
	}
}

func TestParseWithSourcesConcurrentUse(t *testing.T) {
	p := boundParser(t)
	config := map[string][]string{"tags": {"a", "b"}}
	environ := map[string]string{"APP_PORT": "7000"}

	const goroutines = 32
	const iterations = 100
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				args := []string{"--need", fmt.Sprintf("g%d", g), "--tag", "cli"}
				r, err := p.ParseWithSources(args, config, environ)
				if err != nil {
					errCh <- err
					return
				}
				if r.String("need") != fmt.Sprintf("g%d", g) {
					errCh <- fmt.Errorf("need = %q", r.String("need"))
					return
				}
				if r.Int("port") != 7000 || r.Source("port") != SourceEnvironment {
					errCh <- fmt.Errorf("port = %d source %v", r.Int("port"), r.Source("port"))
					return
				}
				if got := r.Strings("tag"); !reflect.DeepEqual(got, []string{"cli"}) {
					errCh <- fmt.Errorf("tags = %v", got)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	// Shared input survived the storm.
	if got := config["tags"]; !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("shared config mutated: %v", got)
	}
}

func TestHelpIgnoresBindings(t *testing.T) {
	plain, err := NewParser([]Option{
		{Long: "host", Short: "h", Type: TypeString, Default: "localhost", Required: true},
		{Long: "tag", Repeatable: true},
	}, []Positional{{Name: "path"}})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := NewParser([]Option{
		{Long: "host", Short: "h", Type: TypeString, Default: "localhost", Required: true, ConfigKey: "host", EnvVar: "HOST"},
		{Long: "tag", Repeatable: true, ConfigKey: "tags", EnvVar: "TAG"},
	}, []Positional{{Name: "path"}})
	if err != nil {
		t.Fatal(err)
	}
	root := New("tool", "a tool", nil, nil)
	var hb, hb2 strings.Builder
	if err := root.WriteHelp(nil, plain, &hb); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteHelp(nil, bound, &hb2); err != nil {
		t.Fatal(err)
	}
	if hb.String() != hb2.String() {
		t.Fatalf("help changed by bindings:\n%s\n--- want ---\n%s", hb2.String(), hb.String())
	}
}

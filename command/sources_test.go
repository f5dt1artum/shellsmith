package command

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// boundParser builds a parser where every option is bound to both a
// configuration key (long name under "cfg.") and an environment variable
// ("ENV_" + upper long name), unless overridden.
func boundParser(t *testing.T, opts []Option, args []Positional) *Parser {
	t.Helper()
	p, err := NewParser(opts, args)
	if err != nil {
		t.Fatalf("NewParser: %v", err)
	}
	return p
}

func layeredOptions() []Option {
	return []Option{
		{Long: "host", Type: TypeString, Default: "default-host", ConfigKey: "host", EnvVar: "ENV_HOST"},
		{Long: "port", Type: TypeInt, Default: "8080", ConfigKey: "port", EnvVar: "ENV_PORT"},
		{Long: "timeout", Type: TypeDuration, Default: "1s", ConfigKey: "timeout", EnvVar: "ENV_TIMEOUT"},
		{Long: "verbose", Type: TypeBool, Default: "false", ConfigKey: "verbose", EnvVar: "ENV_VERBOSE"},
		{Long: "tag", Type: TypeString, Repeatable: true, ConfigKey: "tag", EnvVar: "ENV_TAG"},
		{Long: "need", Type: TypeString, Required: true, ConfigKey: "need", EnvVar: "ENV_NEED"},
	}
}

func TestNewParserRejectsDuplicateBindings(t *testing.T) {
	cases := []struct {
		label string
		opts  []Option
		want  string // SpecError.Name
	}{
		{
			"duplicate config key",
			[]Option{
				{Long: "a", ConfigKey: "shared"},
				{Long: "b", ConfigKey: "shared"},
			},
			"b",
		},
		{
			"duplicate env var",
			[]Option{
				{Long: "a", EnvVar: "SHARED"},
				{Long: "b", EnvVar: "SHARED"},
			},
			"b",
		},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			p, err := NewParser(c.opts, nil)
			if p != nil {
				t.Fatalf("returned parser %v on invalid spec, want nil", p)
			}
			if !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("error = %v, want errors.Is ErrInvalidSpec", err)
			}
			var se *SpecError
			if !errors.As(err, &se) {
				t.Fatalf("error %v is not *SpecError", err)
			}
			if se.Name != c.want {
				t.Fatalf("SpecError.Name = %q, want %q", se.Name, c.want)
			}
		})
	}
}

func TestNewParserAcceptsEmptyBindings(t *testing.T) {
	// Empty bindings never collide even when repeated, and a non-empty
	// binding does not collide with an empty one.
	opts := []Option{
		{Long: "a", ConfigKey: "", EnvVar: ""},
		{Long: "b", ConfigKey: "", EnvVar: ""},
		{Long: "c", ConfigKey: "k", EnvVar: ""},
		{Long: "d", ConfigKey: "", EnvVar: "V"},
	}
	if _, err := NewParser(opts, nil); err != nil {
		t.Fatalf("empty bindings must be allowed: %v", err)
	}
}

func TestParseWithSourcesDefaultsWhenLowerLayersEmpty(t *testing.T) {
	p := boundParser(t, layeredOptions(), nil)
	r, err := p.ParseWithSources([]string{"--need", "x"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.String("host") != "default-host" || r.Int("port") != 8080 ||
		r.Duration("timeout") != time.Second || r.Bool("verbose") {
		t.Fatalf("defaults not applied: host=%q port=%d timeout=%v verbose=%v",
			r.String("host"), r.Int("port"), r.Duration("timeout"), r.Bool("verbose"))
	}
	if r.Source("host") != SourceDefault || r.Source("port") != SourceDefault ||
		r.Source("need") != SourceCommandLine {
		t.Fatalf("unexpected sources: host=%s port=%s need=%s",
			r.Source("host"), r.Source("port"), r.Source("need"))
	}
	if r.Source("unknown") != SourceNone {
		t.Fatalf("unknown source = %v, want SourceNone", r.Source("unknown"))
	}
}

func TestSourcePrecedenceCommandLineWins(t *testing.T) {
	p := boundParser(t, layeredOptions(), nil)
	cfg := map[string][]string{"host": {"cfg-host"}, "port": {"9000"}}
	env := map[string]string{"ENV_HOST": "env-host", "ENV_PORT": "7000"}
	r, err := p.ParseWithSources([]string{"--host", "cli-host", "--port", "6000", "--need", "x"}, cfg, env)
	if err != nil {
		t.Fatal(err)
	}
	if r.String("host") != "cli-host" || r.Source("host") != SourceCommandLine {
		t.Fatalf("host = %q src = %s", r.String("host"), r.Source("host"))
	}
	if r.Int("port") != 6000 || r.Source("port") != SourceCommandLine {
		t.Fatalf("port = %d src = %s", r.Int("port"), r.Source("port"))
	}
	// Lower layers for other options still apply independently.
	if r.Source("need") != SourceCommandLine {
		t.Fatalf("need src = %s", r.Source("need"))
	}
}

func TestSourcePrecedenceEnvironmentBeatsConfig(t *testing.T) {
	p := boundParser(t, layeredOptions(), nil)
	cfg := map[string][]string{
		"host":    {"cfg-host"},
		"port":    {"9000"},
		"timeout": {"500ms"},
		"verbose": {"true"},
	}
	env := map[string]string{
		"ENV_HOST": "env-host",
		"ENV_PORT": "7000",
	}
	r, err := p.ParseWithSources([]string{"--need", "x"}, cfg, env)
	if err != nil {
		t.Fatal(err)
	}
	if r.String("host") != "env-host" || r.Source("host") != SourceEnvironment {
		t.Fatalf("host = %q src = %s", r.String("host"), r.Source("host"))
	}
	if r.Int("port") != 7000 || r.Source("port") != SourceEnvironment {
		t.Fatalf("port = %d src = %s", r.Int("port"), r.Source("port"))
	}
	// Config still wins where environment is absent.
	if r.Duration("timeout") != 500*time.Millisecond || r.Source("timeout") != SourceConfig {
		t.Fatalf("timeout = %v src = %s", r.Duration("timeout"), r.Source("timeout"))
	}
	if !r.Bool("verbose") || r.Source("verbose") != SourceConfig {
		t.Fatalf("verbose = %v src = %s", r.Bool("verbose"), r.Source("verbose"))
	}
}

func TestSourcePrecedenceConfigBeatsDefault(t *testing.T) {
	p := boundParser(t, layeredOptions(), nil)
	cfg := map[string][]string{"host": {"cfg-host"}, "port": {"9000"}}
	r, err := p.ParseWithSources([]string{"--need", "x"}, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.String("host") != "cfg-host" || r.Source("host") != SourceConfig {
		t.Fatalf("host = %q src = %s", r.String("host"), r.Source("host"))
	}
	if r.Int("port") != 9000 || r.Source("port") != SourceConfig {
		t.Fatalf("port = %d src = %s", r.Int("port"), r.Source("port"))
	}
}

func TestLayersDoNotMixForRepeatable(t *testing.T) {
	p := boundParser(t, layeredOptions(), nil)
	cfg := map[string][]string{"tag": {"cfg-a", "cfg-b"}}
	env := map[string]string{"ENV_TAG": "env-only"}

	// Environment present: its single value wholly replaces the config
	// list; no concatenation.
	r, err := p.ParseWithSources([]string{"--need", "x"}, cfg, env)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Strings("tag"); !reflect.DeepEqual(got, []string{"env-only"}) {
		t.Fatalf("env tags = %v, want [env-only]", got)
	}
	if r.Source("tag") != SourceEnvironment {
		t.Fatalf("tag src = %s", r.Source("tag"))
	}

	// Config present without environment: every element retained.
	r, err = p.ParseWithSources([]string{"--need", "x"}, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Strings("tag"); !reflect.DeepEqual(got, []string{"cfg-a", "cfg-b"}) {
		t.Fatalf("cfg tags = %v", got)
	}
	if r.Source("tag") != SourceConfig {
		t.Fatalf("tag src = %s", r.Source("tag"))
	}

	// Command line present: only its occurrences, lower layers ignored.
	r, err = p.ParseWithSources([]string{"--tag", "cli", "--need", "x"}, cfg, env)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Strings("tag"); !reflect.DeepEqual(got, []string{"cli"}) {
		t.Fatalf("cli tags = %v, want [cli]", got)
	}
	if r.Source("tag") != SourceCommandLine {
		t.Fatalf("tag src = %s", r.Source("tag"))
	}
}

func TestConfigNonRepeatableTakesLast(t *testing.T) {
	opts := []Option{{Long: "host", ConfigKey: "host"}, {Long: "port", Type: TypeInt, ConfigKey: "port"}}
	p := boundParser(t, opts, nil)
	cfg := map[string][]string{
		"host": {"first", "middle", "last"},
		// Earlier non-integer elements must not error: only the last is
		// selected for a non-repeatable option.
		"port": {"not-a-number", "42"},
	}
	r, err := p.ParseWithSources(nil, cfg, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.String("host") != "last" || r.Source("host") != SourceConfig {
		t.Fatalf("host = %q src = %s", r.String("host"), r.Source("host"))
	}
	if r.Int("port") != 42 || r.Source("port") != SourceConfig {
		t.Fatalf("port = %d src = %s", r.Int("port"), r.Source("port"))
	}
	if got := r.Strings("host"); !reflect.DeepEqual(got, []string{"last"}) {
		t.Fatalf("host slice = %v, want [last]", got)
	}
}

func TestEmptyConfigSliceMeansAbsent(t *testing.T) {
	opts := []Option{
		{Long: "host", Default: "dflt", ConfigKey: "host", EnvVar: "ENV_HOST"},
		{Long: "other", ConfigKey: "other"},
	}
	p := boundParser(t, opts, nil)

	for _, empty := range [][]string{nil, {}} {
		cfg := map[string][]string{"host": empty, "other": empty}
		env := map[string]string{"ENV_HOST": "env-host"}
		r, err := p.ParseWithSources(nil, cfg, env)
		if err != nil {
			t.Fatalf("empty slice (%v): %v", empty, err)
		}
		// Empty config slice falls through to environment.
		if r.String("host") != "env-host" || r.Source("host") != SourceEnvironment {
			t.Fatalf("host = %q src = %s", r.String("host"), r.Source("host"))
		}
		if r.Source("other") != SourceDefault {
			t.Fatalf("other src = %s, want default", r.Source("other"))
		}
	}

	// Empty slice with no environment falls through to the default.
	r, err := p.ParseWithSources(nil, map[string][]string{"host": {}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.String("host") != "dflt" || r.Source("host") != SourceDefault {
		t.Fatalf("host = %q src = %s", r.String("host"), r.Source("host"))
	}
}

func TestEmptyEnvironmentValueIsExplicit(t *testing.T) {
	opts := []Option{
		{Long: "s", Default: "dflt", EnvVar: "ENV_S"},
		{Long: "n", Type: TypeInt, Default: "5", EnvVar: "ENV_N"},
	}
	p := boundParser(t, opts, nil)

	r, err := p.ParseWithSources(nil, nil, map[string]string{"ENV_S": ""})
	if err != nil {
		t.Fatalf("empty string env for string option: %v", err)
	}
	if r.String("s") != "" || r.Source("s") != SourceEnvironment {
		t.Fatalf("s = %q src = %s, want empty/environment", r.String("s"), r.Source("s"))
	}
	if r.Provided("s") {
		t.Fatal("environment value must not count as command-line provided")
	}
	if r.Count("s") != 0 {
		t.Fatalf("count = %d, want 0", r.Count("s"))
	}

	// Empty environment value fails int conversion, beats the default.
	_, err = p.ParseWithSources(nil, nil, map[string]string{"ENV_N": ""})
	var pe *ParseError
	if !errors.As(err, &pe) || !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("empty env int: err = %v, want ErrInvalidValue", err)
	}
	if pe.Name != "n" || pe.Value != "" || pe.Source != SourceEnvironment || pe.Token != "" {
		t.Fatalf("ParseError = name=%q value=%q source=%s token=%q", pe.Name, pe.Value, pe.Source, pe.Token)
	}
}

func TestEmptyConfigElementIsExplicit(t *testing.T) {
	opts := []Option{
		{Long: "s", Default: "dflt", ConfigKey: "s"},
		{Long: "n", Type: TypeInt, Default: "5", ConfigKey: "n"},
	}
	p := boundParser(t, opts, nil)

	r, err := p.ParseWithSources(nil, map[string][]string{"s": {""}}, nil)
	if err != nil {
		t.Fatalf("empty config element for string option: %v", err)
	}
	if r.String("s") != "" || r.Source("s") != SourceConfig {
		t.Fatalf("s = %q src = %s", r.String("s"), r.Source("s"))
	}

	// Repeatable string retains the explicit empty element.
	pr := boundParser(t, []Option{{Long: "t", Repeatable: true, ConfigKey: "t"}}, nil)
	r, err = pr.ParseWithSources(nil, map[string][]string{"t": {"a", "", "b"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Strings("t"); !reflect.DeepEqual(got, []string{"a", "", "b"}) {
		t.Fatalf("tags = %v", got)
	}

	// Empty config element fails int conversion.
	_, err = p.ParseWithSources(nil, map[string][]string{"n": {"", "7"}}, nil)
	// Non-repeatable: the last element "7" is selected, the empty one is
	// not converted.
	if err != nil {
		t.Fatalf("non-repeatable should ignore earlier empty element: %v", err)
	}
	_, err = p.ParseWithSources(nil, map[string][]string{"n": {"7", ""}}, nil)
	var pe *ParseError
	if !errors.As(err, &pe) || !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("last empty element int: err = %v, want ErrInvalidValue", err)
	}
	if pe.Name != "n" || pe.Value != "" || pe.Source != SourceConfig || pe.Token != "" {
		t.Fatalf("ParseError = name=%q value=%q source=%s token=%q", pe.Name, pe.Value, pe.Source, pe.Token)
	}
}

func TestUnknownBindingsAreIgnored(t *testing.T) {
	p := boundParser(t, []Option{{Long: "host", Default: "dflt", ConfigKey: "host", EnvVar: "ENV_HOST"}}, nil)
	cfg := map[string][]string{"host": {"cfg"}, "unrelated": {"x", "y"}}
	env := map[string]string{"ENV_HOST": "env", "UNRELATED": "z"}
	r, err := p.ParseWithSources(nil, cfg, env)
	if err != nil {
		t.Fatal(err)
	}
	if r.String("host") != "env" {
		t.Fatalf("host = %q", r.String("host"))
	}
}

func TestProcessEnvironmentIsNotRead(t *testing.T) {
	opts := []Option{{Long: "host", Default: "dflt", EnvVar: "SHELLSMITH_TEST_HOST_42"}}
	p := boundParser(t, opts, nil)
	t.Setenv("SHELLSMITH_TEST_HOST_42", "from-process-env")
	r, err := p.ParseWithSources(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.String("host") != "dflt" || r.Source("host") != SourceDefault {
		t.Fatalf("process environment leaked: host=%q src=%s", r.String("host"), r.Source("host"))
	}
}

func TestRequiredSatisfiedByLowerLayers(t *testing.T) {
	opts := []Option{{Long: "need", Required: true, ConfigKey: "need", EnvVar: "ENV_NEED"}}
	p := boundParser(t, opts, nil)

	if _, err := p.ParseWithSources(nil, nil, nil); !errors.Is(err, ErrRequired) {
		t.Fatalf("no layer: err = %v, want ErrRequired", err)
	}
	if r, err := p.ParseWithSources(nil, map[string][]string{"need": {"from-cfg"}}, nil); err != nil {
		t.Fatalf("config satisfies required: %v", err)
	} else if r.String("need") != "from-cfg" || r.Source("need") != SourceConfig {
		t.Fatalf("need = %q src = %s", r.String("need"), r.Source("need"))
	}
	if r, err := p.ParseWithSources(nil, nil, map[string]string{"ENV_NEED": "from-env"}); err != nil {
		t.Fatalf("env satisfies required: %v", err)
	} else if r.String("need") != "from-env" || r.Source("need") != SourceEnvironment {
		t.Fatalf("need = %q src = %s", r.String("need"), r.Source("need"))
	}

	// A default still cannot satisfy required.
	pd, err := NewParser([]Option{{Long: "need", Required: true, Default: "d", ConfigKey: "need", EnvVar: "ENV_NEED"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pd.ParseWithSources(nil, nil, nil); !errors.Is(err, ErrRequired) {
		t.Fatalf("default satisfied required: err = %v", err)
	}
}

func TestLowerLayerConversionFailures(t *testing.T) {
	opts := []Option{
		{Long: "port", Type: TypeInt, ConfigKey: "port", EnvVar: "ENV_PORT"},
		{Long: "dur", Type: TypeDuration, ConfigKey: "dur", EnvVar: "ENV_DUR"},
	}
	p := boundParser(t, opts, nil)

	check := func(t *testing.T, err error, name, value string, src Source) {
		t.Helper()
		var pe *ParseError
		if !errors.As(err, &pe) {
			t.Fatalf("err = %v, want *ParseError", err)
		}
		if !errors.Is(err, ErrInvalidValue) {
			t.Fatalf("err = %v, want ErrInvalidValue", err)
		}
		if pe.Name != name || pe.Value != value || pe.Source != src || pe.Token != "" {
			t.Fatalf("ParseError = name=%q value=%q source=%s token=%q; want name=%q value=%q source=%s",
				pe.Name, pe.Value, pe.Source, pe.Token, name, value, src)
		}
	}

	_, err := p.ParseWithSources(nil, nil, map[string]string{"ENV_PORT": "abc"})
	check(t, err, "port", "abc", SourceEnvironment)

	_, err = p.ParseWithSources(nil, map[string][]string{"port": {"abc"}}, nil)
	check(t, err, "port", "abc", SourceConfig)

	_, err = p.ParseWithSources(nil, map[string][]string{"dur": {"soon"}}, nil)
	check(t, err, "dur", "soon", SourceConfig)

	// A higher winning layer suppresses a failing lower layer entirely.
	r, err := p.ParseWithSources(
		[]string{"--port", "80"},
		map[string][]string{"port": {"bad-cfg"}},
		map[string]string{"ENV_PORT": "bad-env"},
	)
	if err != nil {
		t.Fatalf("command line should suppress failing lower layers: %v", err)
	}
	if r.Int("port") != 80 || r.Source("port") != SourceCommandLine {
		t.Fatalf("port = %d src = %s", r.Int("port"), r.Source("port"))
	}

	// Environment present suppresses a failing config layer.
	r, err = p.ParseWithSources(nil, map[string][]string{"port": {"bad-cfg"}}, map[string]string{"ENV_PORT": "80"})
	if err != nil {
		t.Fatalf("environment should suppress failing config: %v", err)
	}
	if r.Int("port") != 80 || r.Source("port") != SourceEnvironment {
		t.Fatalf("port = %d src = %s", r.Int("port"), r.Source("port"))
	}
}

func TestLowerLayerFailureReturnsNoPartialResult(t *testing.T) {
	opts := []Option{
		{Long: "ok", ConfigKey: "ok"},
		{Long: "bad", Type: TypeInt, ConfigKey: "bad"},
	}
	p := boundParser(t, opts, nil)
	r, err := p.ParseWithSources(nil, map[string][]string{"ok": {"fine"}, "bad": {"nope"}}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if r != nil {
		t.Fatalf("returned partial result %v", r)
	}
}

func TestProvidedAndCountOnlyCountCommandLine(t *testing.T) {
	opts := []Option{
		{Long: "host", ConfigKey: "host", EnvVar: "ENV_HOST"},
		{Long: "tag", Repeatable: true, ConfigKey: "tag", EnvVar: "ENV_TAG"},
	}
	p := boundParser(t, opts, nil)
	cfg := map[string][]string{"host": {"cfg"}, "tag": {"a", "b"}}
	env := map[string]string{"ENV_HOST": "env"}

	r, err := p.ParseWithSources(nil, cfg, env)
	if err != nil {
		t.Fatal(err)
	}
	if r.Provided("host") || r.Count("host") != 0 {
		t.Fatalf("env-only host: provided=%v count=%d", r.Provided("host"), r.Count("host"))
	}
	if r.Provided("tag") || r.Count("tag") != 0 {
		t.Fatalf("config-only tag: provided=%v count=%d", r.Provided("tag"), r.Count("tag"))
	}
	if got := r.Strings("tag"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("tag values = %v", got)
	}

	r, err = p.ParseWithSources([]string{"--host", "cli", "--tag", "x", "--tag", "y"}, cfg, env)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Provided("host") || r.Count("host") != 1 {
		t.Fatalf("cli host: provided=%v count=%d", r.Provided("host"), r.Count("host"))
	}
	if !r.Provided("tag") || r.Count("tag") != 2 {
		t.Fatalf("cli tag: provided=%v count=%d", r.Provided("tag"), r.Count("tag"))
	}
	if got := r.Strings("tag"); !reflect.DeepEqual(got, []string{"x", "y"}) {
		t.Fatalf("tag values = %v, lower layers must not mix in", got)
	}
}

func TestDefaultsDoNotLeakIntoSlices(t *testing.T) {
	opts := []Option{
		{Long: "s", Default: "dflt", ConfigKey: "s", EnvVar: "ENV_S"},
		{Long: "n", Type: TypeInt, Default: "42", ConfigKey: "n"},
	}
	p := boundParser(t, opts, nil)
	r, err := p.ParseWithSources(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Strings("s"); len(got) != 0 {
		t.Fatalf("default leaked into Strings: %v", got)
	}
	if got := r.Ints("n"); len(got) != 0 {
		t.Fatalf("default leaked into Ints: %v", got)
	}

	// Environment/config values do appear in the slices.
	r, err = p.ParseWithSources(nil, map[string][]string{"n": {"7"}}, map[string]string{"ENV_S": "e"})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Strings("s"); !reflect.DeepEqual(got, []string{"e"}) {
		t.Fatalf("env Strings = %v", got)
	}
	if got := r.Ints("n"); !reflect.DeepEqual(got, []int{7}) {
		t.Fatalf("cfg Ints = %v", got)
	}
}

func TestParseWithSourcesPositionalsUnchanged(t *testing.T) {
	opts := []Option{{Long: "v", Short: "v", Type: TypeBool, ConfigKey: "v", EnvVar: "ENV_V"}}
	p := boundParser(t, opts, []Positional{{Name: "cmd"}, {Name: "rest", Variadic: true}})
	r, err := p.ParseWithSources(
		[]string{"-v", "run", "x", "--", "-z"},
		map[string][]string{"v": {"true"}},
		map[string]string{"ENV_V": "true"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if r.Arg("cmd") != "run" {
		t.Fatalf("cmd = %q", r.Arg("cmd"))
	}
	if got := r.ArgValues("rest"); !reflect.DeepEqual(got, []string{"x", "-z"}) {
		t.Fatalf("rest = %v", got)
	}
	if !r.Bool("v") || r.Source("v") != SourceCommandLine {
		t.Fatalf("v = %v src = %s", r.Bool("v"), r.Source("v"))
	}
}

func TestParseWithSourcesDoesNotMutateInputs(t *testing.T) {
	opts := layeredOptions()
	p := boundParser(t, opts, []Positional{{Name: "rest", Variadic: true}})
	argv := []string{"--host", "cli", "--need", "x", "pos", "--", "--rest"}
	argvCopy := append([]string(nil), argv...)
	cfg := map[string][]string{
		"host":    {"a", "b"},
		"port":    {"1", "2"},
		"tag":     {"t1", "t2"},
		"ignored": {"z"},
	}
	cfgCopy := map[string][]string{}
	for k, v := range cfg {
		cfgCopy[k] = append([]string(nil), v...)
	}
	env := map[string]string{"ENV_HOST": "e", "ENV_PORT": "3", "ENV_NEED": "n", "OTHER": "o"}
	envCopy := map[string]string{}
	for k, v := range env {
		envCopy[k] = v
	}

	if _, err := p.ParseWithSources(argv, cfg, env); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(argv, argvCopy) {
		t.Fatalf("argv mutated: %v", argv)
	}
	if !reflect.DeepEqual(cfg, cfgCopy) {
		t.Fatalf("config map mutated: %v vs %v", cfg, cfgCopy)
	}
	for k, v := range cfg {
		if !reflect.DeepEqual(v, cfgCopy[k]) {
			t.Fatalf("config slice %q mutated: %v", k, v)
		}
	}
	if !reflect.DeepEqual(env, envCopy) {
		t.Fatalf("environ map mutated: %v vs %v", env, envCopy)
	}
}

func TestParseWithSourcesDeterministic(t *testing.T) {
	// Repeatable config conversion reports the first failing element in
	// slice order regardless of map iteration order.
	opts := []Option{
		{Long: "a", Type: TypeInt, Repeatable: true, ConfigKey: "a"},
		{Long: "b", Type: TypeInt, Repeatable: true, ConfigKey: "b"},
	}
	p := boundParser(t, opts, nil)
	cfg := map[string][]string{"a": {"bad-a"}, "b": {"bad-b"}}
	seen := map[string]int{}
	for i := 0; i < 50; i++ {
		_, err := p.ParseWithSources(nil, cfg, nil)
		var pe *ParseError
		if !errors.As(err, &pe) {
			t.Fatalf("iteration %d: err = %v", i, err)
		}
		seen[pe.Name]++
	}
	if len(seen) != 1 {
		t.Fatalf("failure option nondeterministic: %v", seen)
	}
}

func TestParseMatchesParseWithSourcesEmptyLayers(t *testing.T) {
	p := boundParser(t, layeredOptions(), []Positional{{Name: "target", Variadic: true}})
	argv := []string{"--host", "cli", "--port", "9", "--tag", "x", "--need", "n", "thing", "--", "-y"}
	r1, err := p.Parse(append([]string(nil), argv...))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := p.ParseWithSources(append([]string(nil), argv...), map[string][]string{}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"host", "port", "timeout", "verbose", "tag", "need"} {
		if r1.Source(name) != r2.Source(name) {
			t.Fatalf("%s source: %s vs %s", name, r1.Source(name), r2.Source(name))
		}
		if !reflect.DeepEqual(r1.Strings(name), r2.Strings(name)) ||
			!reflect.DeepEqual(r1.Ints(name), r2.Ints(name)) {
			t.Fatalf("%s slices differ", name)
		}
		if r1.Provided(name) != r2.Provided(name) || r1.Count(name) != r2.Count(name) {
			t.Fatalf("%s provided/count differ", name)
		}
	}
	if !reflect.DeepEqual(r1.Args(), r2.Args()) {
		t.Fatalf("args differ: %v vs %v", r1.Args(), r2.Args())
	}
}

func TestParseWithSourcesConcurrentUse(t *testing.T) {
	p := boundParser(t, layeredOptions(), []Positional{{Name: "rest", Variadic: true}})

	const goroutines = 32
	const iterations = 80
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				argv := []string{"--need", fmt.Sprintf("cli-%d", g), "pos"}
				cfg := map[string][]string{
					"host": {fmt.Sprintf("cfg-%d", g)},
					"port": {fmt.Sprintf("%d", 9000+g)},
					"tag":  {"a", "b"},
				}
				env := map[string]string{"ENV_HOST": fmt.Sprintf("env-%d", g)}
				r, err := p.ParseWithSources(argv, cfg, env)
				if err != nil {
					errCh <- err
					return
				}
				if r.String("host") != fmt.Sprintf("env-%d", g) || r.Source("host") != SourceEnvironment {
					errCh <- fmt.Errorf("host = %q src = %s", r.String("host"), r.Source("host"))
					return
				}
				if r.Int("port") != 9000+g || r.Source("port") != SourceConfig {
					errCh <- fmt.Errorf("port = %d src = %s", r.Int("port"), r.Source("port"))
					return
				}
				if got := r.Strings("tag"); !reflect.DeepEqual(got, []string{"a", "b"}) {
					errCh <- fmt.Errorf("tag = %v", got)
					return
				}
				if got := r.ArgValues("rest"); !reflect.DeepEqual(got, []string{"pos"}) {
					errCh <- fmt.Errorf("rest = %v", got)
					return
				}
				// Caller maps must survive intact.
				if cfg["host"][0] != fmt.Sprintf("cfg-%d", g) || env["ENV_HOST"] != fmt.Sprintf("env-%d", g) {
					errCh <- fmt.Errorf("input maps corrupted")
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
}

func TestBoundParserHelpIsByteIdentical(t *testing.T) {
	plain, err := NewParser(
		[]Option{{Long: "host", Type: TypeString, Required: true}, {Long: "n", Type: TypeInt, Default: "3"}},
		[]Positional{{Name: "dest"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := NewParser(
		[]Option{
			{Long: "host", Type: TypeString, Required: true, ConfigKey: "host", EnvVar: "HOST"},
			{Long: "n", Type: TypeInt, Default: "3", ConfigKey: "n", EnvVar: "N"},
		},
		[]Positional{{Name: "dest"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	leaf := New("svc", "", nil, noop)
	var b1, b2 strings.Builder
	if err := leaf.WriteHelp(nil, plain, &b1); err != nil {
		t.Fatal(err)
	}
	if err := leaf.WriteHelp(nil, bound, &b2); err != nil {
		t.Fatal(err)
	}
	if b1.String() != b2.String() {
		t.Fatalf("help differs with bindings:\n%q\nvs\n%q", b1.String(), b2.String())
	}
}

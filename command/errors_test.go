package command

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
)

func TestClassifyErrorNil(t *testing.T) {
	if got := ClassifyError(nil); got != ClassNone || got.String() != "none" {
		t.Fatalf("ClassifyError(nil) = %v (%q), want ClassNone (none)", got, got.String())
	}
	if got := ExitCode(nil); got != 0 {
		t.Fatalf("ExitCode(nil) = %d, want 0", got)
	}
}

func TestClassifyErrorUnknown(t *testing.T) {
	err := errors.New("boom")
	if got := ClassifyError(err); got != ClassFailure || got.String() != "failure" {
		t.Fatalf("ClassifyError(unknown) = %v, want ClassFailure", got)
	}
	if got := ExitCode(err); got != 1 {
		t.Fatalf("ExitCode(unknown) = %d, want 1", got)
	}
}

// TestClassificationRules checks each documented direct mapping together
// with its exit code and stable text.
func TestClassificationRules(t *testing.T) {
	root := New("tool", "", nil, nil)
	if err := root.Add(New("child", "", nil, noop)); err != nil {
		t.Fatal(err)
	}
	lookupErr := executeErr(t, root, context.Background(), []string{"nope"})
	requiredErr := executeErr(t, root, context.Background(), nil)

	// Definition errors produced by the package.
	invalidCmdErr := root.Add(nil)
	specErr := func() error { _, e := NewParser([]Option{{Long: "-bad"}}, nil); return e }()
	conflictErr := func() error {
		r := New("p", "", nil, nil)
		_ = r.Add(New("ok", "", nil, noop))
		return r.Add(New("ok", "", nil, noop))
	}()
	attachedErr := func() error {
		r := New("p", "", nil, nil)
		c := New("ok", "", nil, noop)
		_ = r.Add(c)
		return r.Add(c)
	}()
	var wantSpec *SpecError
	if !errors.As(specErr, &wantSpec) {
		t.Fatalf("expected *SpecError from NewParser, got %T", specErr)
	}
	badNameErr := func() error {
		r := New("p", "", nil, nil)
		return r.Add(New("-bad", "", nil, noop))
	}()

	p := parserForClassify(t)
	shellErr := root.WriteCompletion("tcsh", nil, io_discard{})
	configErr := &ConfigError{Path: "x.json", kind: ErrInvalidConfig, msg: "bad"}

	envParseErr := parseErr(t, p, []string{"--need", "x"}, nil, map[string]string{"APP_PORT": "abc"})
	cfgParseErr := parseErrFromConfig(t, p)
	cliUnknownOpt := parseErr(t, p, []string{"--nope"}, nil, nil)
	cliInvalidValue := parseErr(t, p, []string{"--port", "abc"}, nil, nil)
	cliMissingValue := parseErr(t, p, []string{"--port"}, nil, nil)
	cliRequired := parseErr(t, p, nil, nil, nil)
	cliUnexpected := parseErr(t, p, []string{"--need", "x", "extra", "positional"}, nil, nil)

	cases := []struct {
		name string
		err  error
		want ErrorClass
		code int
		text string
	}{
		{"lookup", lookupErr, ClassUsage, 2, "usage"},
		{"command required", requiredErr, ClassUsage, 2, "usage"},
		{"shell", shellErr, ClassUsage, 2, "usage"},
		{"cli unknown option", cliUnknownOpt, ClassUsage, 2, "usage"},
		{"cli invalid value", cliInvalidValue, ClassUsage, 2, "usage"},
		{"cli missing value", cliMissingValue, ClassUsage, 2, "usage"},
		{"cli required", cliRequired, ClassUsage, 2, "usage"},
		{"cli unexpected argument", cliUnexpected, ClassUsage, 2, "usage"},

		{"config", configErr, ClassConfiguration, 78, "configuration"},
		{"env invalid value", envParseErr, ClassConfiguration, 78, "configuration"},
		{"config-file invalid value", cfgParseErr, ClassConfiguration, 78, "configuration"},

		{"invalid command nil node", invalidCmdErr, ClassDefinition, 70, "definition"},
		{"name", badNameErr, ClassDefinition, 70, "definition"},
		{"conflict", conflictErr, ClassDefinition, 70, "definition"},
		{"attached", attachedErr, ClassDefinition, 70, "definition"},
		{"spec", specErr, ClassDefinition, 70, "definition"},
		{"invalid command cycle", fmt.Errorf("wrap: %w", ErrInvalidCommand), ClassDefinition, 70, "definition"},

		{"canceled", context.Canceled, ClassCanceled, 130, "canceled"},
		{"deadline", context.DeadlineExceeded, ClassTimeout, 124, "timeout"},
		{"execute canceled", executeErr(t, root, canceledContext(), []string{"child"}), ClassCanceled, 130, "canceled"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.err == nil {
				t.Fatalf("test case %q produced no error", c.name)
			}
			if got := ClassifyError(c.err); got != c.want {
				t.Errorf("ClassifyError = %v, want %v", got, c.want)
			}
			if got := c.want.String(); got != c.text {
				t.Errorf("String = %q, want %q", got, c.text)
			}
			if got := ExitCode(c.err); got != c.code {
				t.Errorf("ExitCode = %d, want %d", got, c.code)
			}
		})
	}
}

// TestClassifySeesThroughWrapping wraps each class of error one and two
// layers deep and through a custom wrapper, and expects the same result.
func TestClassifySeesThroughWrapping(t *testing.T) {
	base := []struct {
		err  error
		want ErrorClass
	}{
		{&LookupError{Arg: "x"}, ClassUsage},
		{ErrCommandRequired, ClassUsage},
		{&ShellError{Shell: "x"}, ClassUsage},
		{&ConfigError{Path: "p", kind: ErrConfigIO}, ClassConfiguration},
		{&NameError{Name: "x"}, ClassDefinition},
		{&ConflictError{Name: "x"}, ClassDefinition},
		{&AttachedError{Name: "x"}, ClassDefinition},
		{&SpecError{Name: "x"}, ClassDefinition},
		{ErrInvalidCommand, ClassDefinition},
		{context.Canceled, ClassCanceled},
		{context.DeadlineExceeded, ClassTimeout},
		{errors.New("x"), ClassFailure},
	}
	for i, b := range base {
		wrapped := fmt.Errorf("layer1: %w", fmt.Errorf("layer2: %w", b.err))
		if got := ClassifyError(wrapped); got != b.want {
			t.Errorf("case %d wrapped: got %v, want %v", i, got, b.want)
		}
		if got := ExitCode(wrapped); got != b.want.ExitCode() {
			t.Errorf("case %d wrapped code: got %d, want %d", i, got, b.want.ExitCode())
		}
	}
}

// customJoin is a multi-cause error independent of errors.Join.
type customJoin struct{ errs []error }

func (j *customJoin) Error() string   { return "join" }
func (j *customJoin) Unwrap() []error { return j.errs }

// TestClassifyJoinPriority covers errors.Join and a custom
// Unwrap() []error type, asserting the documented precedence.
func TestClassifyJoinPriority(t *testing.T) {
	usage := &LookupError{Arg: "x"}
	config := &ConfigError{Path: "p", kind: ErrConfigIO}
	definition := &NameError{Name: "x"}
	timeout := context.DeadlineExceeded
	canceled := context.Canceled
	failure := errors.New("boom")

	cases := []struct {
		name string
		err  error
		want ErrorClass
	}{
		{"failure+definition", errors.Join(failure, definition), ClassDefinition},
		{"definition+usage", errors.Join(definition, usage), ClassUsage},
		{"usage+configuration", errors.Join(usage, config), ClassConfiguration},
		{"configuration+timeout", errors.Join(config, timeout), ClassTimeout},
		{"timeout+canceled", errors.Join(timeout, canceled), ClassCanceled},
		{"all", errors.Join(failure, definition, usage, config, timeout, canceled), ClassCanceled},
		{"all reversed", errors.Join(canceled, timeout, config, usage, definition, failure), ClassCanceled},
		{"custom slice", &customJoin{errs: []error{failure, definition, usage}}, ClassUsage},
		{"nested joins", errors.Join(errors.Join(failure, definition), errors.Join(usage, config)), ClassConfiguration},
		{"nil members ignored", errors.Join(nil, definition, nil), ClassDefinition},
		{"wrapped member", fmt.Errorf("outer: %w", errors.Join(failure, canceled)), ClassCanceled},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyError(c.err); got != c.want {
				t.Errorf("ClassifyError = %v (%d), want %v (%d)", got, got.ExitCode(), c.want, c.want.ExitCode())
			}
		})
	}
}

// TestParseErrorSourceClassification pins the one field-dependent rule:
// the same ErrInvalidValue sentinel is configuration from environment or
// config layers but usage from the command line (SourceNone).
func TestParseErrorSourceClassification(t *testing.T) {
	p := parserForClassify(t)

	// Command-line conversion failure: SourceNone -> usage.
	cli := parseErr(t, p, []string{"--port=xx"}, nil, nil)
	var pe *ParseError
	if !errors.As(cli, &pe) || pe.Source != SourceNone {
		t.Fatalf("expected command-line ParseError, got %#v", cli)
	}
	if ClassifyError(cli) != ClassUsage {
		t.Errorf("cli invalid value = %v, want usage", ClassifyError(cli))
	}

	// Environment conversion failure -> configuration, even when the
	// command line is otherwise fine.
	env := parseErr(t, p, []string{"--need", "x"}, nil, map[string]string{"APP_PORT": "xx"})
	if ClassifyError(env) != ClassConfiguration {
		t.Errorf("env invalid value = %v, want configuration", ClassifyError(env))
	}

	// Other env-layer parse errors do not exist (only conversion fails
	// there); a SourceEnvironment ParseError wrapping a different
	// sentinel must remain usage because only ErrInvalidValue is
	// configuration.
	odd := &ParseError{Source: SourceEnvironment, kind: ErrRequired}
	if ClassifyError(odd) != ClassUsage {
		t.Errorf("env required = %v, want usage", ClassifyError(odd))
	}
	// SourceDefault invalid value is likewise not env/config -> usage.
	def := &ParseError{Source: SourceDefault, kind: ErrInvalidValue}
	if ClassifyError(def) != ClassUsage {
		t.Errorf("default invalid value = %v, want usage", ClassifyError(def))
	}
	// Wrapped env conversion failure keeps its class.
	wrapped := fmt.Errorf("running: %w", env)
	if ClassifyError(wrapped) != ClassConfiguration {
		t.Errorf("wrapped env invalid value = %v, want configuration", ClassifyError(wrapped))
	}
}

// TestClassifyDoesNotMutate records the error fields before and after
// classification, including calls through joins.
func TestClassifyDoesNotMutate(t *testing.T) {
	raw := []*ParseError{
		{Name: "port", Token: "--port", Value: "abc", Source: SourceNone, kind: ErrInvalidValue, msg: "m"},
		{Name: "port", Value: "abc", Source: SourceEnvironment, kind: ErrInvalidValue, msg: "m"},
	}
	ce := &ConfigError{Path: "f", kind: ErrInvalidConfig, msg: "m"}
	le := &LookupError{Path: []string{"tool"}, Arg: "x"}
	joined := errors.Join(raw[0], ce)

	snap := func() []any {
		return []any{*raw[0], *raw[1], *ce, *le, joined.Error()}
	}
	before := snap()
	for range 10 {
		_ = ClassifyError(raw[0])
		_ = ExitCode(raw[1])
		_ = ClassifyError(ce)
		_ = ClassifyError(le)
		_ = ClassifyError(joined)
	}
	after := snap()
	for i := range before {
		if fmt.Sprint(before[i]) != fmt.Sprint(after[i]) {
			t.Errorf("error state changed at %d:\nbefore %#v\nafter  %#v", i, before[i], after[i])
		}
	}
}

// TestClassifyConcurrent hammers the classifiers from many goroutines;
// the race detector does the real work here.
func TestClassifyConcurrent(t *testing.T) {
	errs := []error{
		nil,
		errors.New("x"),
		&LookupError{Arg: "x"},
		&ConfigError{Path: "p", kind: ErrConfigIO},
		&NameError{Name: "x"},
		fmt.Errorf("w: %w", context.Canceled),
		errors.Join(&ShellError{Shell: "x"}, context.DeadlineExceeded),
	}
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				err := errs[i%len(errs)]
				_ = ClassifyError(err)
				_ = ExitCode(err)
			}
		}()
	}
	wg.Wait()
}

// TestErrorClassExitCodeConsistency checks ExitCode(err) always equals
// ClassifyError(err).ExitCode(), and the full class table.
func TestErrorClassExitCodeConsistency(t *testing.T) {
	table := map[ErrorClass]int{
		ClassNone: 0, ClassUsage: 2, ClassConfiguration: 78,
		ClassDefinition: 70, ClassCanceled: 130, ClassTimeout: 124, ClassFailure: 1,
	}
	for c, code := range table {
		if c.ExitCode() != code {
			t.Errorf("%v.ExitCode() = %d, want %d", c, c.ExitCode(), code)
		}
	}
	for _, err := range []error{nil, errors.New("x"), &LookupError{}, context.Canceled} {
		if ExitCode(err) != ClassifyError(err).ExitCode() {
			t.Errorf("ExitCode/ClassifyError disagree for %v", err)
		}
	}
	// Out-of-range class falls back to the failure code.
	if (ErrorClass(99)).ExitCode() != 1 {
		t.Errorf("out-of-range class code = %d, want 1", ErrorClass(99).ExitCode())
	}
}

// TestErrorTextDoesNotAffectClass makes errors whose text mentions other
// classes and confirms text never changes the result.
func TestErrorTextDoesNotAffectClass(t *testing.T) {
	err := errors.New("context canceled: configuration invalid usage error")
	if ClassifyError(err) != ClassFailure {
		t.Fatalf("text matching produced %v, want failure", ClassifyError(err))
	}
	// A real canceled error with misleading text stays canceled.
	cancel := fmt.Errorf("configuration usage failure: %w", context.Canceled)
	if ClassifyError(cancel) != ClassCanceled {
		t.Fatalf("wrapped canceled = %v, want canceled", ClassifyError(cancel))
	}
}

// --- helpers ---

type io_discard struct{}

func (io_discard) Write(p []byte) (int, error) { return len(p), nil }

func executeErr(t *testing.T, n *Node, ctx context.Context, args []string) error {
	t.Helper()
	err := n.Execute(ctx, args, io_discard{}, io_discard{})
	if err == nil {
		t.Fatalf("Execute(%v) returned nil, want error", args)
	}
	return err
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func parserForClassify(t *testing.T) *Parser {
	t.Helper()
	p, err := NewParser([]Option{
		{Long: "host", Type: TypeString, ConfigKey: "host", EnvVar: "APP_HOST"},
		{Long: "port", Short: "p", Type: TypeInt, ConfigKey: "port", EnvVar: "APP_PORT"},
		{Long: "need", Type: TypeString, Required: true, ConfigKey: "need", EnvVar: "APP_NEED"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func parseErr(t *testing.T, p *Parser, args []string, config map[string][]string, environ map[string]string) error {
	t.Helper()
	_, err := p.ParseWithSources(args, config, environ)
	if err == nil {
		t.Fatalf("ParseWithSources(%v) returned nil, want error", args)
	}
	return err
}

func parseErrFromConfig(t *testing.T, p *Parser) error {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/c.json"
	if err := os.WriteFile(path, []byte(`{"port":"abc","need":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := p.ParseConfigFiles(nil, []string{path}, nil)
	if err == nil {
		t.Fatal("ParseConfigFiles returned nil, want error")
	}
	return err
}

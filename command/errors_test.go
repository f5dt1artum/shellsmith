package command

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestClassifyErrorNilAndUnknown(t *testing.T) {
	if got := ClassifyError(nil); got != ErrorNone {
		t.Errorf("ClassifyError(nil) = %q, want none", got)
	}
	if got := ExitCode(nil); got != 0 {
		t.Errorf("ExitCode(nil) = %d, want 0", got)
	}

	plain := errors.New("boom")
	if got := ClassifyError(plain); got != ErrorFailure {
		t.Errorf("ClassifyError(plain) = %q, want failure", got)
	}
	if got := ExitCode(plain); got != 1 {
		t.Errorf("ExitCode(plain) = %d, want 1", got)
	}
}

func TestClassifyErrorClassesAndCodes(t *testing.T) {
	lookup := &LookupError{Path: []string{"tool"}, Arg: "nope"}
	configIO := &ConfigError{Path: "x.yaml", kind: ErrConfigIO, msg: "io"}
	configFmt := &ConfigError{Path: "x.yaml", kind: ErrUnsupportedConfigFormat, msg: "fmt"}
	configBad := &ConfigError{Path: "x.yaml", kind: ErrInvalidConfig, msg: "bad"}
	shell := &ShellError{Shell: "tcsh"}
	cliInvalid := &ParseError{Name: "n", Token: "--n", Value: "x", kind: ErrInvalidValue,
		Source: SourceNone, msg: "bad cli value"}
	envInvalid := &ParseError{Name: "n", Value: "x", kind: ErrInvalidValue,
		Source: SourceEnvironment, msg: "bad env value"}
	cfgInvalid := &ParseError{Name: "n", Value: "x", kind: ErrInvalidValue,
		Source: SourceConfig, msg: "bad cfg value"}
	required := &ParseError{Name: "n", kind: ErrRequired, Source: SourceNone, msg: "required"}
	unknownOpt := &ParseError{Token: "--x", kind: ErrUnknownOption, msg: "unknown"}
	nameErr := &NameError{Name: "bad!"}
	conflictErr := &ConflictError{Name: "dup"}
	attachedErr := &AttachedError{Name: "child"}
	specErr := &SpecError{Name: "n", msg: "bad spec"}

	cases := []struct {
		name string
		err  error
		want ErrorClass
		code int
	}{
		{"lookup", lookup, ErrorUsage, 2},
		{"unknown command sentinel", ErrUnknownCommand, ErrorUsage, 2},
		{"command required", ErrCommandRequired, ErrorUsage, 2},
		{"shell", shell, ErrorUsage, 2},
		{"unsupported shell sentinel", ErrUnsupportedShell, ErrorUsage, 2},
		{"cli parse error", cliInvalid, ErrorUsage, 2},
		{"required parse error", required, ErrorUsage, 2},
		{"unknown option parse error", unknownOpt, ErrorUsage, 2},
		{"config io", configIO, ErrorConfiguration, 78},
		{"config format", configFmt, ErrorConfiguration, 78},
		{"config invalid", configBad, ErrorConfiguration, 78},
		{"env invalid value", envInvalid, ErrorConfiguration, 78},
		{"config invalid value", cfgInvalid, ErrorConfiguration, 78},
		{"name error", nameErr, ErrorDefinition, 70},
		{"invalid name sentinel", ErrInvalidName, ErrorDefinition, 70},
		{"conflict error", conflictErr, ErrorDefinition, 70},
		{"name conflict sentinel", ErrNameConflict, ErrorDefinition, 70},
		{"attached error", attachedErr, ErrorDefinition, 70},
		{"already attached sentinel", ErrAlreadyAttached, ErrorDefinition, 70},
		{"spec error", specErr, ErrorDefinition, 70},
		{"invalid spec sentinel", ErrInvalidSpec, ErrorDefinition, 70},
		{"invalid command", ErrInvalidCommand, ErrorDefinition, 70},
		{"canceled", context.Canceled, ErrorCanceled, 130},
		{"deadline", context.DeadlineExceeded, ErrorTimeout, 124},
		{"plain", errors.New("x"), ErrorFailure, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyError(tc.err); got != tc.want {
				t.Errorf("ClassifyError = %q, want %q", got, tc.want)
			}
			if got := ExitCode(tc.err); got != tc.code {
				t.Errorf("ExitCode = %d, want %d", got, tc.code)
			}
		})
	}
}

func TestClassifyErrorProducedByAPIParsing(t *testing.T) {
	p, err := NewParser([]Option{{Long: "n", Type: TypeInt, EnvVar: "N", ConfigKey: "n"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Parse([]string{"--n", "x"}); ClassifyError(err) != ErrorUsage || ExitCode(err) != 2 {
		t.Errorf("command-line invalid value: class=%q code=%d", ClassifyError(err), ExitCode(err))
	}
	if _, err := p.ParseWithSources(nil, nil, map[string]string{"N": "x"}); ClassifyError(err) != ErrorConfiguration || ExitCode(err) != 78 {
		t.Errorf("environment invalid value: class=%q code=%d", ClassifyError(err), ExitCode(err))
	}
	if _, err := p.ParseWithSources(nil, map[string][]string{"n": {"x"}}, nil); ClassifyError(err) != ErrorConfiguration || ExitCode(err) != 78 {
		t.Errorf("config invalid value: class=%q code=%d", ClassifyError(err), ExitCode(err))
	}
}

func TestClassifyErrorWrapped(t *testing.T) {
	// Several layers of %w wrapping must not hide the cause.
	deep := fmt.Errorf("layer1: %w", fmt.Errorf("layer2: %w", context.Canceled))
	if got := ClassifyError(deep); got != ErrorCanceled {
		t.Errorf("wrapped canceled = %q, want canceled", got)
	}

	wrappedLookup := fmt.Errorf("doing work: %w", &LookupError{Arg: "x"})
	if got := ClassifyError(wrappedLookup); got != ErrorUsage {
		t.Errorf("wrapped lookup = %q, want usage", got)
	}

	wrappedConfig := fmt.Errorf("setup: %w", &ConfigError{Path: "c.json", kind: ErrConfigIO, msg: "io"})
	if got := ClassifyError(wrappedConfig); got != ErrorConfiguration {
		t.Errorf("wrapped config = %q, want configuration", got)
	}

	// An env-layer ParseError wrapped in arbitrary layers stays configuration.
	wrappedEnv := fmt.Errorf("outer: %w",
		fmt.Errorf("inner: %w", &ParseError{Name: "n", Value: "x", Source: SourceEnvironment, kind: ErrInvalidValue}))
	if got := ClassifyError(wrappedEnv); got != ErrorConfiguration {
		t.Errorf("wrapped env parse error = %q, want configuration", got)
	}
}

func TestClassifyErrorJoinPrecedence(t *testing.T) {
	canceled := context.Canceled
	deadline := context.DeadlineExceeded
	usage := ErrCommandRequired
	config := &ConfigError{Path: "c", kind: ErrConfigIO, msg: "io"}
	definition := ErrInvalidCommand
	failure := errors.New("boom")

	cases := []struct {
		name string
		err  error
		want ErrorClass
		code int
	}{
		{"canceled beats all", errors.Join(failure, definition, usage, config, deadline, canceled), ErrorCanceled, 130},
		{"timeout beats config", errors.Join(failure, usage, config, deadline), ErrorTimeout, 124},
		{"config beats usage", errors.Join(failure, usage, config), ErrorConfiguration, 78},
		{"usage beats definition", errors.Join(failure, definition, usage), ErrorUsage, 2},
		{"definition beats failure", errors.Join(failure, definition), ErrorDefinition, 70},
		{"only failure", errors.Join(failure, errors.New("other")), ErrorFailure, 1},
		{"deeply nested joins", errors.Join(errors.Join(usage, definition), errors.Join(failure, config)), ErrorConfiguration, 78},
		{"join wrapped in fmt", fmt.Errorf("wrapped: %w", errors.Join(usage, canceled)), ErrorCanceled, 130},
		{"multi-wrap fmt", fmt.Errorf("%w: %w", usage, config), ErrorConfiguration, 78},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyError(tc.err); got != tc.want {
				t.Errorf("ClassifyError = %q, want %q", got, tc.want)
			}
			if got := ExitCode(tc.err); got != tc.code {
				t.Errorf("ExitCode = %d, want %d", got, tc.code)
			}
		})
	}
}

func TestClassifyErrorTextDoesNotMatter(t *testing.T) {
	// Errors whose text mentions known categories must be classified by
	// their type identity only.
	fake := errors.New("context canceled: command required, invalid config file")
	if got := ClassifyError(fake); got != ErrorFailure {
		t.Errorf("text-matching error classified as %q, want failure", got)
	}
}

func TestClassifyErrorDoesNotMutate(t *testing.T) {
	original := &ParseError{Name: "n", Short: "s", Token: "--n=x", Value: "x",
		Source: SourceEnvironment, kind: ErrInvalidValue, msg: "msg"}
	snapshot := *original
	_ = ClassifyError(original)
	_ = ExitCode(original)
	if *original != snapshot {
		t.Errorf("ParseError mutated: got %+v want %+v", *original, snapshot)
	}

	ce := &ConfigError{Path: "p", kind: ErrConfigIO, msg: "m"}
	ceSnap := *ce
	_ = ClassifyError(fmt.Errorf("w: %w", ce))
	if *ce != ceSnap {
		t.Errorf("ConfigError mutated: got %+v want %+v", *ce, ceSnap)
	}
}

func TestClassifyErrorStableText(t *testing.T) {
	for class, text := range map[ErrorClass]string{
		ErrorNone:          "none",
		ErrorUsage:         "usage",
		ErrorConfiguration: "configuration",
		ErrorDefinition:    "definition",
		ErrorCanceled:      "canceled",
		ErrorTimeout:       "timeout",
		ErrorFailure:       "failure",
	} {
		if string(class) != text {
			t.Errorf("ErrorClass %q has unstable text %q", class, text)
		}
	}
}

func TestClassifyErrorConcurrent(t *testing.T) {
	errs := []error{
		nil,
		errors.New("boom"),
		ErrCommandRequired,
		&ConfigError{Path: "c", kind: ErrConfigIO, msg: "io"},
		ErrInvalidCommand,
		fmt.Errorf("w: %w", errors.Join(context.Canceled, ErrCommandRequired)),
	}
	const goroutines = 32
	const iterations = 200

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				err := errs[(seed+i)%len(errs)]
				_ = ClassifyError(err)
				_ = ExitCode(err)
			}
		}(g)
	}
	wg.Wait()
}

func TestClassifyErrorRealExecuteAndParser(t *testing.T) {
	root := New("tool", "root", nil, nil)

	// Unknown command through Execute.
	if err := root.Execute(context.Background(), []string{"missing"}, nil, nil); ClassifyError(err) != ErrorUsage {
		t.Errorf("Execute unknown command class = %q, want usage", ClassifyError(err))
	}
	// No handler and no args.
	leaf := New("leaf", "", nil, nil)
	if err := root.Add(leaf); err != nil {
		t.Fatal(err)
	}
	if err := root.Execute(context.Background(), []string{"leaf"}, nil, nil); ClassifyError(err) != ErrorUsage {
		t.Errorf("Execute command required class = %q, want usage", ClassifyError(err))
	}
	// Canceled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := root.Execute(ctx, nil, nil, nil); ClassifyError(err) != ErrorCanceled {
		t.Errorf("Execute canceled class = %q, want canceled", ClassifyError(err))
	}
	// Definition error from Add.
	if err := root.Add(nil); ClassifyError(err) != ErrorDefinition {
		t.Errorf("Add(nil) class = %q, want definition", ClassifyError(err))
	}
}

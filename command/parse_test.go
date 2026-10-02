package command

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestNewParserRejectsInvalidSpecs(t *testing.T) {
	cases := []struct {
		label string
		opts  []Option
		args  []Positional
		want  string // expected SpecError.Name, empty means name not asserted
	}{
		{"empty long", []Option{{Long: ""}}, nil, ""},
		{"long with leading hyphen", []Option{{Long: "-x"}}, nil, "-x"},
		{"long with space", []Option{{Long: "a b"}}, nil, "a b"},
		{"long non-ascii", []Option{{Long: "名"}}, nil, "名"},
		{"duplicate long", []Option{{Long: "p"}, {Long: "p"}}, nil, "p"},
		{"short too long", []Option{{Long: "p", Short: "ab"}}, nil, "p"},
		{"short hyphen", []Option{{Long: "p", Short: "-"}}, nil, "p"},
		{"duplicate short", []Option{{Long: "a", Short: "x"}, {Long: "b", Short: "x"}}, nil, "b"},
		{"bad int default", []Option{{Long: "n", Type: TypeInt, Default: "abc"}}, nil, "n"},
		{"bad bool default", []Option{{Long: "f", Type: TypeBool, Default: "maybe"}}, nil, "f"},
		{"bad duration default", []Option{{Long: "d", Type: TypeDuration, Default: "soon"}}, nil, "d"},
		{"empty positional name", nil, []Positional{{Name: ""}}, ""},
		{"duplicate positional", nil, []Positional{{Name: "a"}, {Name: "a"}}, "a"},
		{"two variadics", nil, []Positional{{Name: "a", Variadic: true}, {Name: "b", Variadic: true}}, "b"},
		{"variadic not last", nil, []Positional{{Name: "a", Variadic: true}, {Name: "b"}}, "a"},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			p, err := NewParser(c.opts, c.args)
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
			if c.want != "" && se.Name != c.want {
				t.Fatalf("SpecError.Name = %q, want %q", se.Name, c.want)
			}
		})
	}
}

func TestNewParserAcceptsValidSpecs(t *testing.T) {
	cases := []struct {
		opts []Option
		args []Positional
	}{
		{nil, nil},
		{[]Option{{Long: "a"}, {Long: "b_1-x", Short: "Z"}}, nil},
		{[]Option{{Long: "n", Type: TypeInt, Default: "7"},
			{Long: "d", Type: TypeDuration, Default: "500ms"},
			{Long: "f", Type: TypeBool, Default: "true"},
			{Long: "s", Type: TypeString, Default: "x"}}, nil},
		{nil, []Positional{{Name: "a"}, {Name: "rest", Variadic: true}}},
		{[]Option{{Long: "x", Type: TypeBool}}, []Positional{{Name: "items", Variadic: true}}},
	}
	for i, c := range cases {
		if _, err := NewParser(c.opts, c.args); err != nil {
			t.Fatalf("case %d: unexpected error %v", i, err)
		}
	}
}

func TestNewParserDetachesFromSpecSlices(t *testing.T) {
	opts := []Option{{Long: "host", Short: "h", Default: "localhost"}}
	args := []Positional{{Name: "target"}}
	p, err := NewParser(opts, args)
	if err != nil {
		t.Fatal(err)
	}
	opts[0].Long = "mutated"
	opts[0].Short = "z"
	opts[0].Default = "changed"
	args[0].Name = "mutated"

	r, err := p.Parse([]string{"target-value"})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.String("host"); got != "localhost" {
		t.Fatalf("parser shares option spec with caller, default = %q", got)
	}
	if got := r.Arg("target"); got != "target-value" {
		t.Fatalf("parser shares positional spec with caller, got %q", got)
	}
	if got := r.Arg("mutated"); got != "" {
		t.Fatalf("caller renamed positional after construction, got %q", got)
	}
}

func TestLongOptionForms(t *testing.T) {
	p, err := NewParser([]Option{
		{Long: "host", Type: TypeString},
		{Long: "port", Type: TypeInt},
		{Long: "timeout", Type: TypeDuration},
		{Long: "verbose", Type: TypeBool},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	r, err := p.Parse([]string{"--host", "example.com", "--port=8080", "--timeout", "250ms", "--verbose"})
	if err != nil {
		t.Fatal(err)
	}
	if r.String("host") != "example.com" {
		t.Fatalf("host = %q", r.String("host"))
	}
	if r.Int("port") != 8080 {
		t.Fatalf("port = %d", r.Int("port"))
	}
	if r.Duration("timeout") != 250*time.Millisecond {
		t.Fatalf("timeout = %v", r.Duration("timeout"))
	}
	if !r.Bool("verbose") || !r.Provided("verbose") || r.Count("verbose") != 1 {
		t.Fatalf("verbose = %v provided=%v count=%d", r.Bool("verbose"), r.Provided("verbose"), r.Count("verbose"))
	}
}

func TestBoolExplicitForms(t *testing.T) {
	p, _ := NewParser([]Option{{Long: "flag", Short: "f", Type: TypeBool}}, nil)
	for _, argv := range [][]string{
		{"--flag"},
		{"--flag=true"},
		{"--flag=1"},
		{"-f"},
	} {
		r, err := p.Parse(argv)
		if err != nil {
			t.Fatalf("%v: %v", argv, err)
		}
		if !r.Bool("flag") {
			t.Fatalf("%v: flag = false, want true", argv)
		}
	}
	r, err := p.Parse([]string{"--flag=false"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Bool("flag") || !r.Provided("flag") {
		t.Fatalf("flag = %v provided=%v, want false/true", r.Bool("flag"), r.Provided("flag"))
	}

	// Bare bool must not consume the following token.
	pPos, _ := NewParser([]Option{{Long: "flag", Short: "f", Type: TypeBool}},
		[]Positional{{Name: "rest", Variadic: true}})
	r, err = pPos.Parse([]string{"--flag", "positional"})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Args(); !reflect.DeepEqual(got, []string{"positional"}) {
		t.Fatalf("bool ate next token: args = %v", got)
	}
}

func TestShortClusters(t *testing.T) {
	p, _ := NewParser([]Option{
		{Long: "verbose", Short: "v", Type: TypeBool},
		{Long: "quiet", Short: "q", Type: TypeBool},
		{Long: "port", Short: "p", Type: TypeInt},
	}, nil)

	r, err := p.Parse([]string{"-vq"})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Bool("verbose") || !r.Bool("quiet") {
		t.Fatalf("cluster -vq: verbose=%v quiet=%v", r.Bool("verbose"), r.Bool("quiet"))
	}

	// Non-bool inside a cluster consumes the remainder, even "=value".
	for _, argv := range [][]string{{"-vp9000"}, {"-vp=9000"}, {"-p", "9000"}, {"-p9000"}} {
		r, err := p.Parse(argv)
		if err != nil {
			t.Fatalf("%v: %v", argv, err)
		}
		if r.Int("port") != 9000 {
			t.Fatalf("%v: port = %d", argv, r.Int("port"))
		}
	}
}

func TestWaitingOptionConsumesHyphenToken(t *testing.T) {
	p, _ := NewParser([]Option{
		{Long: "n", Short: "n", Type: TypeInt},
		{Long: "d", Short: "d", Type: TypeDuration},
		{Long: "s", Short: "s", Type: TypeString},
	}, nil)
	r, err := p.Parse([]string{"-n", "-5", "--d", "-1m30s", "-s", "--weird"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Int("n") != -5 {
		t.Fatalf("n = %d, want -5", r.Int("n"))
	}
	if r.Duration("d") != -90*time.Second {
		t.Fatalf("d = %v, want -1m30s", r.Duration("d"))
	}
	if r.String("s") != "--weird" {
		t.Fatalf("s = %q", r.String("s"))
	}

	// A hyphen token when no option awaits is an unknown option, never a
	// positional argument.
	_, err = p.Parse([]string{"-5"})
	if !errors.Is(err, ErrUnknownOption) {
		t.Fatalf("standalone -5: err = %v, want ErrUnknownOption", err)
	}
	var pe *ParseError
	if !errors.As(err, &pe) || pe.Token != "-5" {
		t.Fatalf("error must carry token %q, got %+v", "-5", err)
	}
}

func TestDashDashSeparator(t *testing.T) {
	p, _ := NewParser([]Option{{Long: "v", Short: "v", Type: TypeBool}},
		[]Positional{{Name: "rest", Variadic: true}})
	r, err := p.Parse([]string{"-v", "--", "--not-an-option", "-x", "plain"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--not-an-option", "-x", "plain"}
	if !reflect.DeepEqual(r.Args(), want) {
		t.Fatalf("args after -- = %v, want %v", r.Args(), want)
	}
	if got := r.ArgValues("rest"); !reflect.DeepEqual(got, want) {
		t.Fatalf("variadic = %v, want %v", got, want)
	}

	// Without the separator the same token is an unknown option.
	if _, err := p.Parse([]string{"--not-an-option"}); !errors.Is(err, ErrUnknownOption) {
		t.Fatalf("err = %v, want ErrUnknownOption", err)
	}

	// A bare "-" is positional even without a separator.
	r, err = p.Parse([]string{"-"})
	if err != nil {
		t.Fatal(err)
	}
	if r.NArg() != 1 || r.Args()[0] != "-" {
		t.Fatalf("bare dash: args = %v", r.Args())
	}
}

func TestRepeatableOptions(t *testing.T) {
	p, _ := NewParser([]Option{
		{Long: "tag", Short: "t", Type: TypeString, Repeatable: true},
		{Long: "num", Type: TypeInt, Repeatable: true},
	}, nil)
	r, err := p.Parse([]string{"-t", "a", "--tag=b", "--num", "1", "--num", "2"})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Strings("tag"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("tags = %v", got)
	}
	if got := r.Ints("num"); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("nums = %v", got)
	}
	if r.Count("tag") != 2 || !r.Provided("tag") {
		t.Fatalf("count = %d provided = %v", r.Count("tag"), r.Provided("tag"))
	}
	// Scalar accessor reports the last occurrence.
	if r.String("tag") != "b" || r.Int("num") != 2 {
		t.Fatalf("scalar = %q/%d", r.String("tag"), r.Int("num"))
	}

	// Non-repeatable: last occurrence wins and count stays 1.
	p2, _ := NewParser([]Option{{Long: "mode", Type: TypeString}}, nil)
	r2, err := p2.Parse([]string{"--mode", "a", "--mode", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if r2.String("mode") != "b" || r2.Count("mode") != 1 {
		t.Fatalf("mode = %q count = %d", r2.String("mode"), r2.Count("mode"))
	}
}

func TestDefaults(t *testing.T) {
	p, _ := NewParser([]Option{
		{Long: "s", Type: TypeString, Default: "dflt"},
		{Long: "n", Type: TypeInt, Default: "42"},
		{Long: "d", Type: TypeDuration, Default: "3s"},
		{Long: "f", Type: TypeBool, Default: "true"},
		{Long: "e", Type: TypeString},
	}, nil)
	r, err := p.Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.String("s") != "dflt" || r.Int("n") != 42 || r.Duration("d") != 3*time.Second || !r.Bool("f") {
		t.Fatalf("defaults: s=%q n=%d d=%v f=%v", r.String("s"), r.Int("n"), r.Duration("d"), r.Bool("f"))
	}
	if r.Provided("s") || r.Count("s") != 0 || r.Provided("e") {
		t.Fatal("defaults must not count as explicit occurrences")
	}
	if got := r.Strings("s"); len(got) != 0 {
		t.Fatalf("default leaked into repeated-values slice: %v", got)
	}
	if r.String("e") != "" {
		t.Fatalf("zero value default = %q", r.String("e"))
	}

	// Explicit value overrides the default and marks the option provided.
	r2, err := p.Parse([]string{"--s", "explicit", "--f=false"})
	if err != nil {
		t.Fatal(err)
	}
	if r2.String("s") != "explicit" || !r2.Provided("s") {
		t.Fatalf("override: s=%q provided=%v", r2.String("s"), r2.Provided("s"))
	}
	if r2.Bool("f") {
		t.Fatal("explicit --f=false should override true default")
	}
}

func TestRequired(t *testing.T) {
	p, _ := NewParser([]Option{
		{Long: "need", Required: true},
		{Long: "with-default", Required: true, Default: "d"},
	}, nil)

	// Empty args still enforce required options.
	_, err := p.Parse(nil)
	if !errors.Is(err, ErrRequired) {
		t.Fatalf("empty args: err = %v, want ErrRequired", err)
	}
	var pe *ParseError
	if !errors.As(err, &pe) || pe.Name != "need" {
		t.Fatalf("error must carry spec name %q, got %+v", "need", err)
	}

	// A default does not satisfy the required constraint.
	_, err = p.Parse([]string{"--need", "x"})
	if !errors.Is(err, ErrRequired) {
		t.Fatalf("default satisfied required: err = %v", err)
	}

	r, err := p.Parse([]string{"--need", "x", "--with-default", "y"})
	if err != nil {
		t.Fatal(err)
	}
	if r.String("need") != "x" {
		t.Fatalf("need = %q", r.String("need"))
	}
}

func TestParseErrors(t *testing.T) {
	p, _ := NewParser([]Option{
		{Long: "port", Short: "p", Type: TypeInt},
		{Long: "dur", Type: TypeDuration},
		{Long: "flag", Type: TypeBool},
	}, []Positional{{Name: "first"}, {Name: "second"}})

	// Unknown long and short.
	for _, argv := range [][]string{{"--nope"}, {"-z"}, {"-vz"}} {
		_, err := p.Parse(argv)
		if !errors.Is(err, ErrUnknownOption) {
			t.Fatalf("%v: err = %v, want ErrUnknownOption", argv, err)
		}
	}

	// Missing value.
	for _, argv := range [][]string{{"--port"}, {"-p"}} {
		_, err := p.Parse(argv)
		if !errors.Is(err, ErrMissingValue) {
			t.Fatalf("%v: err = %v, want ErrMissingValue", argv, err)
		}
		var pe *ParseError
		if !errors.As(err, &pe) || pe.Name != "port" {
			t.Fatalf("%v: missing-value error must carry name %q, got %+v", argv, "port", err)
		}
	}

	// Type conversion failures.
	for _, argv := range [][]string{{"--port", "abc"}, {"--port=abc"}, {"-pabc"}, {"--dur", "soon"}, {"--flag=maybe"}} {
		_, err := p.Parse(argv)
		if !errors.Is(err, ErrInvalidValue) {
			t.Fatalf("%v: err = %v, want ErrInvalidValue", argv, err)
		}
		var pe *ParseError
		if !errors.As(err, &pe) || pe.Name == "" || pe.Value == "" {
			t.Fatalf("%v: invalid-value error must carry name and value, got %+v", argv, err)
		}
	}

	// Missing positional.
	_, err := p.Parse([]string{"only-one"})
	if !errors.Is(err, ErrRequired) {
		t.Fatalf("err = %v, want ErrRequired", err)
	}
	var pe *ParseError
	if !errors.As(err, &pe) || pe.Name != "second" {
		t.Fatalf("missing positional error must carry %q, got %+v", "second", err)
	}

	// Surplus positional.
	_, err = p.Parse([]string{"a", "b", "c"})
	if !errors.Is(err, ErrUnexpectedArgument) {
		t.Fatalf("err = %v, want ErrUnexpectedArgument", err)
	}
	if !errors.As(err, &pe) || pe.Token != "c" {
		t.Fatalf("surplus error must carry token %q, got %+v", "c", err)
	}

	// Surplus positional after --.
	_, err = p.Parse([]string{"a", "b", "--", "c"})
	if !errors.Is(err, ErrUnexpectedArgument) {
		t.Fatalf("err = %v, want ErrUnexpectedArgument", err)
	}
}

func TestNamedPositionals(t *testing.T) {
	p, _ := NewParser(nil, []Positional{{Name: "src"}, {Name: "dst"}})
	r, err := p.Parse([]string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Arg("src") != "a" || r.Arg("dst") != "b" {
		t.Fatalf("src=%q dst=%q", r.Arg("src"), r.Arg("dst"))
	}
}

func TestVariadicPositional(t *testing.T) {
	p, _ := NewParser([]Option{{Long: "f", Short: "f", Type: TypeBool}},
		[]Positional{{Name: "cmd"}, {Name: "args", Variadic: true}})

	r, err := p.Parse([]string{"only"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Arg("cmd") != "only" {
		t.Fatalf("cmd = %q, want %q", r.Arg("cmd"), "only")
	}
	if got := r.ArgValues("args"); len(got) != 0 {
		t.Fatalf("zero variadic values: %v", got)
	}

	r, err = p.Parse([]string{"-f", "run", "x", "y", "--", "-z"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Arg("cmd") != "run" {
		t.Fatalf("cmd = %q", r.Arg("cmd"))
	}
	want := []string{"x", "y", "-z"}
	if got := r.ArgValues("args"); !reflect.DeepEqual(got, want) {
		t.Fatalf("variadic values = %v, want %v", got, want)
	}

	// Variadic absorbs everything, so no surplus-argument error.
	r, err = p.Parse([]string{"c", "1", "2", "3", "4", "5"})
	if err != nil {
		t.Fatal(err)
	}
	if r.NArg() != 6 {
		t.Fatalf("NArg = %d, want 6", r.NArg())
	}
}

func TestFailureReturnsNoPartialResult(t *testing.T) {
	p, _ := NewParser([]Option{{Long: "a"}, {Long: "b", Required: true}}, nil)
	r, err := p.Parse([]string{"--a", "x", "--unknown"})
	if err == nil {
		t.Fatal("expected error")
	}
	if r != nil {
		t.Fatalf("returned partial result %v on failure", r)
	}
	r, err = p.Parse([]string{"--a", "x"})
	if err == nil || r != nil {
		t.Fatalf("required failure must return nil result, got %v, %v", r, err)
	}
}

func TestParseDoesNotMutateInput(t *testing.T) {
	p, _ := NewParser([]Option{{Long: "n", Short: "n", Type: TypeInt}, {Long: "v", Short: "v", Type: TypeBool}},
		[]Positional{{Name: "rest", Variadic: true}})
	argv := []string{"-v", "--n", "-3", "a", "--", "-x"}
	original := append([]string(nil), argv...)
	if _, err := p.Parse(argv); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(argv, original) {
		t.Fatalf("input mutated: %v, want %v", argv, original)
	}
}

func TestResultsAreIndependent(t *testing.T) {
	p, _ := NewParser([]Option{{Long: "t", Repeatable: true}}, nil)
	r1, err := p.Parse([]string{"--t", "a"})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := p.Parse([]string{"--t", "b", "--t", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if got := r1.Strings("t"); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("r1 contaminated by r2: %v", got)
	}
	if got := r2.Strings("t"); !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Fatalf("r2 = %v", got)
	}
}

func TestParserConcurrentUse(t *testing.T) {
	p, _ := NewParser([]Option{
		{Long: "id", Type: TypeInt},
		{Long: "tag", Repeatable: true},
	}, []Positional{{Name: "rest", Variadic: true}})

	const goroutines = 32
	const iterations = 100
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				argv := []string{"--id", fmt.Sprintf("%d", g), "--tag", "a", "x", "--", "-y"}
				r, err := p.Parse(argv)
				if err != nil {
					errCh <- err
					return
				}
				if r.Int("id") != g {
					errCh <- fmt.Errorf("id = %d, want %d", r.Int("id"), g)
					return
				}
				if got := r.Strings("tag"); !reflect.DeepEqual(got, []string{"a"}) {
					errCh <- fmt.Errorf("tags = %v", got)
					return
				}
				if got := r.ArgValues("rest"); !reflect.DeepEqual(got, []string{"x", "-y"}) {
					errCh <- fmt.Errorf("rest = %v", got)
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

func TestEdgeCases(t *testing.T) {
	// Explicit empty value is distinct from a default.
	p, _ := NewParser([]Option{
		{Long: "s", Type: TypeString, Default: "dflt"},
		{Long: "n", Type: TypeInt},
	}, nil)
	r, err := p.Parse([]string{"--s=", "--n="})
	_ = r
	if !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("--n= should fail int conversion, got %v", err)
	}
	r, err = p.Parse([]string{"--s="})
	if err != nil {
		t.Fatal(err)
	}
	if r.String("s") != "" || !r.Provided("s") {
		t.Fatalf("--s= should override default with explicit empty string, got %q provided=%v", r.String("s"), r.Provided("s"))
	}

	// Repeated bools in one cluster accumulate on a repeatable bool.
	pb, _ := NewParser([]Option{{Long: "v", Short: "v", Type: TypeBool, Repeatable: true}}, nil)
	rb, err := pb.Parse([]string{"-vvv"})
	if err != nil {
		t.Fatal(err)
	}
	if rb.Count("v") != 3 || len(rb.Bools("v")) != 3 {
		t.Fatalf("count = %d bools = %v", rb.Count("v"), rb.Bools("v"))
	}

	// Standalone "--" with no positionals.
	pp, _ := NewParser(nil, nil)
	rp, err := pp.Parse([]string{"--"})
	if err != nil || rp.NArg() != 0 {
		t.Fatalf("bare --: err=%v narg=%d", err, rp.NArg())
	}

	// Unknown names read as zero values without panicking.
	re, err := NewParser(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rr, err := re.Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if rr.String("nope") != "" || rr.Int("nope") != 0 || rr.Bool("nope") || rr.Duration("nope") != 0 {
		t.Fatal("unknown option accessor must yield zero values")
	}
	if rr.Provided("nope") || rr.Count("nope") != 0 {
		t.Fatal("unknown option must not report as provided")
	}
}

func TestParserIntegratesWithCommandTree(t *testing.T) {
	// A handler that opts into parsing.
	p, err := NewParser([]Option{{Long: "port", Type: TypeInt, Default: "8080"}},
		[]Positional{{Name: "path"}})
	if err != nil {
		t.Fatal(err)
	}
	var parsed *Result
	parsingHandler := func(path []string, args []string, stdout, stderr io.Writer) error {
		r, err := p.Parse(args)
		if err != nil {
			return err
		}
		parsed = r
		return nil
	}

	root := New("tool", "", nil, nil)
	if err := root.Add(New("serve", "", []string{"s"}, parsingHandler)); err != nil {
		t.Fatal(err)
	}
	if err := root.Execute(context.Background(),
		[]string{"s", "--port", "9090", "/x"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if parsed.Int("port") != 9090 || parsed.Arg("path") != "/x" {
		t.Fatalf("port=%d path=%q", parsed.Int("port"), parsed.Arg("path"))
	}

	// Parse errors propagate through Execute unchanged.
	err = root.Execute(context.Background(), []string{"serve", "--bogus"}, io.Discard, io.Discard)
	if !errors.Is(err, ErrUnknownOption) {
		t.Fatalf("err = %v, want ErrUnknownOption", err)
	}

	// Defaults apply inside the handler when the option is omitted.
	parsed = nil
	if err := root.Execute(context.Background(), []string{"serve", "/y"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if parsed == nil || parsed.Int("port") != 8080 || parsed.Arg("path") != "/y" {
		t.Fatalf("default not applied: %v", parsed)
	}

	// A handler that does not opt in still receives untouched raw args,
	// including option-like tokens.
	var raw []string
	legacy := New("legacy", "", nil, func(path []string, as []string, stdout, stderr io.Writer) error {
		raw = append([]string(nil), as...)
		return nil
	})
	var stdout, stderr bytes.Buffer
	if err := legacy.Execute(context.Background(),
		[]string{"--anything", "-x", "val"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if want := []string{"--anything", "-x", "val"}; !reflect.DeepEqual(raw, want) {
		t.Fatalf("legacy handler args = %v, want %v", raw, want)
	}
}

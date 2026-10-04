package command

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func sensitiveParser(t *testing.T) *Parser {
	t.Helper()
	p, err := NewParser([]Option{
		{Long: "token", Short: "t", Type: TypeString, Sensitive: true},
		{Long: "pin", Short: "p", Type: TypeInt, Sensitive: true},
		{Long: "quiet", Short: "q", Type: TypeBool, Sensitive: true},
		{Long: "verbose", Short: "v", Type: TypeBool},
		{Long: "name", Short: "n", Type: TypeString},
		{Long: "tags", Type: TypeString, Repeatable: true, Sensitive: true},
	}, []Positional{{Name: "rest", Variadic: true}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSensitiveDoesNotChangeSuccessfulParse(t *testing.T) {
	p := sensitiveParser(t)
	r, err := p.Parse([]string{
		"-t", "s3cret", "--pin=42", "-q", "-v",
		"--tags", "a", "--tags=b",
		"--name", "ordinary", "pos",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.String("token"); got != "s3cret" {
		t.Fatalf("token = %q, want the original secret", got)
	}
	if r.Int("pin") != 42 {
		t.Fatalf("pin = %d, want 42", r.Int("pin"))
	}
	if !r.Bool("quiet") || !r.Provided("quiet") || r.Count("quiet") != 1 {
		t.Fatalf("sensitive bool: value=%v provided=%v count=%d", r.Bool("quiet"), r.Provided("quiet"), r.Count("quiet"))
	}
	if !r.Bool("verbose") {
		t.Fatal("non-sensitive bool must still parse inside a sensitive-aware vector")
	}
	if got := r.Strings("tags"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("tags = %v", got)
	}
	if r.Count("tags") != 2 || !r.Provided("tags") {
		t.Fatalf("tags provided=%v count=%d", r.Provided("tags"), r.Count("tags"))
	}
	if r.String("name") != "ordinary" {
		t.Fatalf("name = %q", r.String("name"))
	}
	if got := r.Args(); !reflect.DeepEqual(got, []string{"pos"}) {
		t.Fatalf("positionals = %v", got)
	}
}

func TestSensitiveSourcePrecedenceAndRequired(t *testing.T) {
	p, err := NewParser([]Option{
		{Long: "token", EnvVar: "T", ConfigKey: "token", Sensitive: true, Required: true},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Config satisfies required for a sensitive option.
	r, err := p.ParseWithSources(nil, map[string][]string{"token": {"cfg-secret"}}, nil)
	if err != nil {
		t.Fatalf("config must satisfy required sensitive option: %v", err)
	}
	if r.Source("token") != SourceConfig || r.String("token") != "cfg-secret" {
		t.Fatalf("token = %q source %v", r.String("token"), r.Source("token"))
	}

	// Environment beats config and still exposes the raw value.
	r, err = p.ParseWithSources(nil,
		map[string][]string{"token": {"cfg-secret"}},
		map[string]string{"T": "env-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Source("token") != SourceEnvironment || r.String("token") != "env-secret" {
		t.Fatalf("token = %q source %v", r.String("token"), r.Source("token"))
	}

	// Command line beats everything and counts as provided.
	r, err = p.ParseWithSources([]string{"--token", "cli-secret"},
		map[string][]string{"token": {"cfg-secret"}},
		map[string]string{"T": "env-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Source("token") != SourceCommandLine || r.String("token") != "cli-secret" ||
		!r.Provided("token") || r.Count("token") != 1 {
		t.Fatalf("token = %q source %v provided=%v count=%d",
			r.String("token"), r.Source("token"), r.Provided("token"), r.Count("token"))
	}

	// Required is still enforced.
	if _, err := p.ParseWithSources(nil, nil, nil); !errors.Is(err, ErrRequired) {
		t.Fatalf("sensitive required: err = %v, want ErrRequired", err)
	}
}

func TestSensitiveInvalidDefaultStillSpecError(t *testing.T) {
	p, err := NewParser([]Option{{Long: "token", Type: TypeInt, Default: "abc", Sensitive: true}}, nil)
	if p != nil {
		t.Fatalf("parser = %v, want nil", p)
	}
	var se *SpecError
	if !errors.As(err, &se) || !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("err = %v, want *SpecError wrapping ErrInvalidSpec", err)
	}
	if se.Name != "token" {
		t.Fatalf("SpecError.Name = %q", se.Name)
	}
}

func TestRedactArgsLongForms(t *testing.T) {
	p := sensitiveParser(t)
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, nil},
		{"empty", []string{}, nil},
		{"space value", []string{"--token", "s3cret"}, []string{"--token", "<redacted>"}},
		{"equals value", []string{"--token=s3cret"}, []string{"--token=<redacted>"}},
		{"empty equals value", []string{"--token="}, []string{"--token=<redacted>"}},
		{"hyphen value", []string{"--token", "-weird"}, []string{"--token", "<redacted>"}},
		{"repeated space", []string{"--token", "a", "--token", "b"},
			[]string{"--token", "<redacted>", "--token", "<redacted>"}},
		{"repeated mixed", []string{"--token=a", "--tags", "b", "--tags=c"},
			[]string{"--token=<redacted>", "--tags", "<redacted>", "--tags=<redacted>"}},
		{"sensitive int", []string{"--pin=7"}, []string{"--pin=<redacted>"}},
		{"non-sensitive keeps value", []string{"--name", "ok"}, []string{"--name", "ok"}},
		{"non-sensitive equals keeps value", []string{"--name=ok"}, []string{"--name=ok"}},
		{"positional kept", []string{"plain", "-"}, []string{"plain", "-"}},
		{"unknown long kept", []string{"--unknown", "s3cret"}, []string{"--unknown", "s3cret"}},
		{"incomplete long at end kept", []string{"--token"}, []string{"--token"}},
		{"incomplete short at end kept", []string{"-t"}, []string{"-t"}},
		{"unknown short kept", []string{"-z", "s3cret"}, []string{"-z", "s3cret"}},
		{"non-sensitive option eats sensitive-looking value",
			[]string{"--name", "--token"}, []string{"--name", "--token"}},
		{"non-sensitive short eats sensitive-looking value",
			[]string{"-n", "-t"}, []string{"-n", "-t"}},
		{"separator stops recognition",
			[]string{"--token", "a", "--", "--token", "b", "-tsecret", "plain"},
			[]string{"--token", "<redacted>", "--", "--token", "b", "-tsecret", "plain"}},
		{"separator keeps rest byte for byte",
			[]string{"--", "--tags=keep", "--name=keep"},
			[]string{"--", "--tags=keep", "--name=keep"}},
		{"token value that is the separator is still a value",
			[]string{"--token", "--", "x"}, []string{"--token", "<redacted>", "x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := p.RedactArgs(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("RedactArgs(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestRedactArgsShortForms(t *testing.T) {
	p := sensitiveParser(t)
	cases := []struct {
		in   []string
		want []string
	}{
		{[]string{"-t", "s3cret"}, []string{"-t", "<redacted>"}},
		{[]string{"-ts3cret"}, []string{"-t<redacted>"}},
		{[]string{"-t=s3cret"}, []string{"-t=<redacted>"}},
		{[]string{"-t="}, []string{"-t=<redacted>"}},
		{[]string{"-vts3cret"}, []string{"-vt<redacted>"}},
		{[]string{"-vt=s3cret"}, []string{"-vt=<redacted>"}},
		{[]string{"-t", "a", "-t", "b"}, []string{"-t", "<redacted>", "-t", "<redacted>"}},
		{[]string{"-ts3cret", "-t", "b"}, []string{"-t<redacted>", "-t", "<redacted>"}},
		// Sensitive boolean: bare forms untouched, explicit value masked.
		{[]string{"-q"}, []string{"-q"}},
		{[]string{"-qv"}, []string{"-qv"}},
		{[]string{"-vq"}, []string{"-vq"}},
		{[]string{"--quiet=true"}, []string{"--quiet=<redacted>"}},
		// Non-sensitive cluster and unknown characters stay verbatim.
		{[]string{"-v"}, []string{"-v"}},
		{[]string{"-vz"}, []string{"-vz"}},
		{[]string{"-ztsecret"}, []string{"-ztsecret"}},
		// Non-sensitive value-taking short consumes the next token.
		{[]string{"-n", "s3cret"}, []string{"-n", "s3cret"}},
		{[]string{"-n", "--token"}, []string{"-n", "--token"}},
		{[]string{"-ns3cret"}, []string{"-ns3cret"}},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.in, "|"), func(t *testing.T) {
			got := p.RedactArgs(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("RedactArgs(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestRedactArgsDoesNotMutateInputAndReturnsIndependentSlice(t *testing.T) {
	p := sensitiveParser(t)
	in := []string{"--token", "s3cret", "-ts3cret", "pos", "--", "--token", "kept"}
	snapshot := append([]string(nil), in...)

	out := p.RedactArgs(in)
	if !reflect.DeepEqual(in, snapshot) {
		t.Fatalf("input mutated: %v, want %v", in, snapshot)
	}
	if len(out) != len(in) {
		t.Fatalf("output length %d, want %d", len(out), len(in))
	}

	// Mutating the returned slice must not reach the caller's input.
	out[0] = "mutated"
	if in[0] != "--token" {
		t.Fatalf("output aliases input backing array: %v", in)
	}
}

func TestRedactArgsConcurrent(t *testing.T) {
	p := sensitiveParser(t)
	in := []string{"-v", "--token", "s3cret", "--tags=a", "-ts3cret", "pos", "--", "--token", "kept"}
	want := []string{"-v", "--token", "<redacted>", "--tags=<redacted>", "-t<redacted>", "pos", "--", "--token", "kept"}

	const goroutines = 32
	const iterations = 200
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				got := p.RedactArgs(in)
				if !reflect.DeepEqual(got, want) {
					errCh <- fmt.Errorf("RedactArgs = %v, want %v", got, want)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, []string{"-v", "--token", "s3cret", "--tags=a", "-ts3cret", "pos", "--", "--token", "kept"}) {
		t.Fatalf("shared input mutated under concurrent calls: %v", in)
	}
}

func TestWriteHelpSensitiveOptions(t *testing.T) {
	p, err := NewParser([]Option{
		{Long: "token", Short: "t", Type: TypeString, Default: "hunter2", Sensitive: true},
		{Long: "pin", Type: TypeInt, Required: true, Sensitive: true},
		{Long: "tags", Type: TypeString, Repeatable: true, Sensitive: true},
		{Long: "quiet", Short: "q", Type: TypeBool, Sensitive: true},
		{Long: "name", Type: TypeString, Default: "visible"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := New("svc", "", nil, nil).WriteHelp(nil, p, &buf); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, line := range []string{
		"  -t, --token <string>  (sensitive)\n",
		"  --pin <int>  (required, sensitive)\n",
		"  --tags <string>  (repeatable, sensitive)\n",
		"  -q, --quiet  (sensitive)\n",
		"  --name <string>  (default: visible)\n",
	} {
		if !strings.Contains(got, line) {
			t.Errorf("help missing line %q\nfull help:\n%s", line, got)
		}
	}
	if strings.Contains(got, "hunter2") {
		t.Errorf("sensitive default leaked into help:\n%s", got)
	}
	if !strings.Contains(got, "default: visible") {
		t.Errorf("non-sensitive default rendering changed:\n%s", got)
	}
	if strings.Count(got, "default:") != 1 {
		t.Errorf("exactly one default marker expected (the visible one):\n%s", got)
	}
}

func TestSensitiveCommandLineConversionErrorsAreMasked(t *testing.T) {
	p, err := NewParser([]Option{
		{Long: "token", Short: "t", Type: TypeInt, Sensitive: true},
		{Long: "quiet", Short: "q", Type: TypeBool, Sensitive: true},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "abc"

	cases := []struct {
		name      string
		argv      []string
		wantToken string
		wantShort string
	}{
		{"long space", []string{"--token", secret}, "--token", ""},
		{"long equals", []string{"--token=" + secret}, "--token=<redacted>", ""},
		{"short space", []string{"-t", secret}, "-t", "t"},
		{"short attached", []string{"-t" + secret}, "-t<redacted>", "t"},
		{"cluster attached", []string{"-qt" + secret}, "-qt<redacted>", "t"},
		{"sensitive bool explicit", []string{"--quiet=" + secret}, "--quiet=<redacted>", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := p.Parse(c.argv)
			if r != nil {
				t.Fatalf("partial result %v returned", r)
			}
			var pe *ParseError
			if !errors.As(err, &pe) || !errors.Is(err, ErrInvalidValue) {
				t.Fatalf("err = %v, want *ParseError wrapping ErrInvalidValue", err)
			}
			if pe.Name != "token" && c.name != "sensitive bool explicit" {
				t.Fatalf("Name = %q", pe.Name)
			}
			if pe.Token != c.wantToken {
				t.Errorf("Token = %q, want %q", pe.Token, c.wantToken)
			}
			if pe.Short != c.wantShort {
				t.Errorf("Short = %q, want %q", pe.Short, c.wantShort)
			}
			if pe.Source != SourceNone {
				t.Errorf("Source = %v, want SourceNone", pe.Source)
			}
			if pe.Value != "<redacted>" {
				t.Errorf("Value = %q, want <redacted>", pe.Value)
			}
			text := err.Error()
			if strings.Contains(text, secret) {
				t.Errorf("error text leaks %q: %q", secret, text)
			}
			if !strings.Contains(text, "<redacted>") {
				t.Errorf("error text lacks marker: %q", text)
			}
			if ClassifyError(err) != ClassUsage {
				t.Errorf("class = %s, want usage", ClassifyError(err))
			}
		})
	}
}

func TestSensitiveLayeredConversionErrorsAreMasked(t *testing.T) {
	p, err := NewParser([]Option{
		{Long: "token", Short: "t", Type: TypeInt, EnvVar: "TOKEN", ConfigKey: "token", Sensitive: true, Repeatable: true},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "abc"

	// Environment.
	_, err = p.ParseWithSources(nil, nil, map[string]string{"TOKEN": secret})
	var pe *ParseError
	if !errors.As(err, &pe) || !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("env err = %v, want ErrInvalidValue", err)
	}
	if pe.Name != "token" || pe.Short != "" || pe.Source != SourceEnvironment || pe.Token != "" {
		t.Fatalf("env error fields = %+v", pe)
	}
	if pe.Value != "<redacted>" || strings.Contains(err.Error(), secret) {
		t.Fatalf("env error leaks value: Value=%q text=%q", pe.Value, err.Error())
	}
	if !strings.Contains(err.Error(), "TOKEN") || !strings.Contains(err.Error(), "token") {
		t.Fatalf("env error should still name binding and option: %q", err.Error())
	}
	if ClassifyError(err) != ClassConfiguration {
		t.Fatalf("class = %s, want configuration", ClassifyError(err))
	}

	// Config, including a bad element among repeatable values.
	_, err = p.ParseWithSources(nil, map[string][]string{"token": {"1", secret, "3"}}, nil)
	if !errors.As(err, &pe) || !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("config err = %v, want ErrInvalidValue", err)
	}
	if pe.Name != "token" || pe.Source != SourceConfig || pe.Token != "" || pe.Value != "<redacted>" {
		t.Fatalf("config error fields = %+v", pe)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("config error text leaks %q: %q", secret, err.Error())
	}
	if ClassifyError(err) != ClassConfiguration {
		t.Fatalf("class = %s, want configuration", ClassifyError(err))
	}
}

func TestSensitiveConfigFileConversionErrorIsMasked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("token: abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := NewParser([]Option{
		{Long: "token", Type: TypeInt, ConfigKey: "token", Sensitive: true},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.ParseConfigFiles(nil, []string{path}, nil)
	var pe *ParseError
	if !errors.As(err, &pe) || !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("err = %v, want ErrInvalidValue", err)
	}
	if pe.Source != SourceConfig || pe.Value != "<redacted>" {
		t.Fatalf("fields = %+v", pe)
	}
	if strings.Contains(err.Error(), "abc") {
		t.Fatalf("config file error leaks value: %q", err.Error())
	}
}

func TestNonSensitiveConversionErrorUnchanged(t *testing.T) {
	p, err := NewParser([]Option{{Long: "port", Short: "p", Type: TypeInt}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Parse([]string{"--port=abc"})
	var pe *ParseError
	if !errors.As(err, &pe) || !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("err = %v", err)
	}
	if pe.Value != "abc" || pe.Token != "--port=abc" {
		t.Fatalf("fields = %+v, want raw value retained", pe)
	}
	if !strings.Contains(err.Error(), "abc") {
		t.Fatalf("non-sensitive error text = %q, want the raw value", err.Error())
	}

	// Environment-layer failures keep the raw value too.
	pEnv, _ := NewParser([]Option{{Long: "port", Type: TypeInt, EnvVar: "P"}}, nil)
	_, err = pEnv.ParseWithSources(nil, nil, map[string]string{"P": "abc"})
	var pe2 *ParseError
	if !errors.As(err, &pe2) || pe2.Value != "abc" || !strings.Contains(err.Error(), "abc") {
		t.Fatalf("non-sensitive env error changed: %+v %v", pe2, err)
	}
}

func TestSensitiveOtherErrorKindsUnchanged(t *testing.T) {
	p, err := NewParser([]Option{
		{Long: "token", Short: "t", Type: TypeString, Sensitive: true, Required: true},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Missing value still names the option and is ErrMissingValue.
	_, err = p.Parse([]string{"--token"})
	var pe *ParseError
	if !errors.As(err, &pe) || !errors.Is(err, ErrMissingValue) {
		t.Fatalf("err = %v, want ErrMissingValue", err)
	}
	if pe.Name != "token" || pe.Token != "--token" {
		t.Fatalf("missing-value fields = %+v", pe)
	}

	// Required still enforced and reported.
	_, err = p.Parse(nil)
	if !errors.As(err, &pe) || !errors.Is(err, ErrRequired) || pe.Name != "token" {
		t.Fatalf("required error = %+v", pe)
	}

	// Unknown options and surplus positionals unchanged.
	_, err = p.Parse([]string{"--nope"})
	if !errors.Is(err, ErrUnknownOption) {
		t.Fatalf("unknown option err = %v", err)
	}
}

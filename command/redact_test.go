package command

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// sensitiveTestParser builds the option set used across the redaction
// tests:
//
//   - --token/-t   sensitive string (also bound to TOKEN env / token cfg)
//   - --pin        sensitive int
//   - --apikey     sensitive repeatable string
//   - --burn/-b    sensitive bool
//   - --user/-u    non-sensitive string
//   - --port/-p    non-sensitive int
//   - --verbose/-v non-sensitive bool
func sensitiveTestParser(t *testing.T) *Parser {
	t.Helper()
	p, err := NewParser([]Option{
		{Long: "token", Short: "t", Type: TypeString, Sensitive: true, EnvVar: "TOKEN", ConfigKey: "token"},
		{Long: "pin", Short: "i", Type: TypeInt, Sensitive: true},
		{Long: "apikey", Type: TypeString, Sensitive: true, Repeatable: true},
		{Long: "burn", Short: "b", Type: TypeBool, Sensitive: true},
		{Long: "user", Short: "u", Type: TypeString},
		{Long: "port", Short: "p", Type: TypeInt},
		{Long: "verbose", Short: "v", Type: TypeBool},
	}, []Positional{{Name: "rest", Variadic: true}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRedactArgsLongForms(t *testing.T) {
	p := sensitiveTestParser(t)
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"separate value", []string{"--token", "s3cr3t"}, []string{"--token", "<redacted>"}},
		{"attached value", []string{"--token=s3cr3t"}, []string{"--token=<redacted>"}},
		{"attached empty value", []string{"--token="}, []string{"--token=<redacted>"}},
		{"separate value at end untouched", []string{"--token"}, []string{"--token"}},
		{"repeatable both forms",
			[]string{"--apikey", "a", "--apikey=b", "--apikey", "c"},
			[]string{"--apikey", "<redacted>", "--apikey=<redacted>", "--apikey", "<redacted>"}},
		{"sensitive bool bare unchanged", []string{"--burn", "pos"}, []string{"--burn", "pos"}},
		{"sensitive bool explicit value masked", []string{"--burn=maybe"}, []string{"--burn=<redacted>"}},
		{"non-sensitive option unchanged", []string{"--user", "bob", "--port=9"}, []string{"--user", "bob", "--port=9"}},
		{"unknown option unchanged", []string{"--nope", "s3cr3t", "--nope=x"}, []string{"--nope", "s3cr3t", "--nope=x"}},
		{"positionals keep bytes and order",
			[]string{"s3cr3t", "pos", "--token", "k", "tail"},
			[]string{"s3cr3t", "pos", "--token", "<redacted>", "tail"}},
		{"everything after -- untouched",
			[]string{"--token", "k", "--", "--token", "s3cr3t", "-t=s3cr3t"},
			[]string{"--token", "<redacted>", "--", "--token", "s3cr3t", "-t=s3cr3t"}},
		{"bare dash is positional", []string{"-", "--token", "k"}, []string{"-", "--token", "<redacted>"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := p.RedactArgs(c.args)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("RedactArgs(%v) = %v, want %v", c.args, got, c.want)
			}
		})
	}
}

func TestRedactArgsShortForms(t *testing.T) {
	p := sensitiveTestParser(t)
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"separate value", []string{"-t", "s3cr3t"}, []string{"-t", "<redacted>"}},
		{"attached value", []string{"-ts3cr3t"}, []string{"-t<redacted>"}},
		{"attached equals value", []string{"-t=s3cr3t"}, []string{"-t=<redacted>"}},
		{"attached empty value", []string{"-t="}, []string{"-t=<redacted>"}},
		{"separate value at end untouched", []string{"-t"}, []string{"-t"}},
		{"cluster prefix preserved with unknown tail left as-is", []string{"-vqt=x"}, []string{"-vqt=x"}},
		{"bool cluster before attached", []string{"-vtsecret"}, []string{"-vt<redacted>"}},
		{"separate after bool cluster", []string{"-vt", "s3cr3t"}, []string{"-vt", "<redacted>"}},
		{"sensitive bool in cluster unchanged", []string{"-vb", "s3cr3t"}, []string{"-vb", "s3cr3t"}},
		{"sensitive bool alone unchanged", []string{"-b"}, []string{"-b"}},
		{"unknown short aborts cluster", []string{"-ztsecret", "s3cr3t"}, []string{"-ztsecret", "s3cr3t"}},
		{"unknown attached value unchanged", []string{"-z=s3cr3t"}, []string{"-z=s3cr3t"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := p.RedactArgs(c.args)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("RedactArgs(%v) = %v, want %v", c.args, got, c.want)
			}
		})
	}
}

func TestRedactArgsKnownNonSensitiveConsumesValue(t *testing.T) {
	p := sensitiveTestParser(t)
	// A known non-sensitive value option consumes the next token even when
	// that token spells a sensitive option, so the token after it is a
	// positional and keeps its bytes.
	cases := [][]string{
		{"--user", "--token", "s3cr3t"},
		{"-u", "-t", "s3cr3t"},
		{"--port", "--apikey", "s3cr3t"},
		{"-p", "-t", "s3cr3t"},
	}
	for _, args := range cases {
		got := p.RedactArgs(args)
		if !reflect.DeepEqual(got, args) {
			t.Fatalf("RedactArgs(%v) = %v, want unchanged", args, got)
		}
	}

	// Non-sensitive bools do not consume, so a following sensitive option
	// is still recognized and masked.
	got := p.RedactArgs([]string{"--verbose", "--token", "s3cr3t"})
	want := []string{"--verbose", "--token", "<redacted>"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bool then sensitive: got %v, want %v", got, want)
	}
}

func TestRedactArgsDoesNotMutateInput(t *testing.T) {
	p := sensitiveTestParser(t)
	argv := []string{"--token", "s3cr3t", "-vtsecret", "pos", "--", "--token=s3cr3t"}
	original := append([]string(nil), argv...)
	redacted := p.RedactArgs(argv)
	if !reflect.DeepEqual(argv, original) {
		t.Fatalf("input mutated: %v, want %v", argv, original)
	}
	// Mutating the returned slice must not reach the input.
	redacted[0] = "changed"
	if !reflect.DeepEqual(argv, original) {
		t.Fatalf("input shares backing storage with result: %v", argv)
	}

	if got := p.RedactArgs(nil); got != nil {
		t.Fatalf("RedactArgs(nil) = %v, want nil", got)
	}
	if got := p.RedactArgs([]string{}); len(got) != 0 {
		t.Fatalf("RedactArgs(empty) = %v", got)
	}
}

func TestRedactArgsConcurrent(t *testing.T) {
	p := sensitiveTestParser(t)
	argv := []string{"-v", "--user", "bob", "--token", "s3cr3t", "-vt=s3cr3t", "x", "--", "--token", "s3cr3t"}
	want := p.RedactArgs(argv)

	var wg sync.WaitGroup
	for g := 0; g < 24; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				got := p.RedactArgs(argv)
				if !reflect.DeepEqual(got, want) {
					t.Errorf("concurrent RedactArgs = %v, want %v", got, want)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestRedactArgsDoesNotValidate(t *testing.T) {
	// Redaction is purely syntactic: a malformed sensitive int value is
	// masked without any error (there is no error to return), and unknown
	// tokens never abort the scan.
	p := sensitiveTestParser(t)
	got := p.RedactArgs([]string{"--pin=not-a-number", "--pin", "also-bad", "--bogus", "--token", "k"})
	want := []string{"--pin=<redacted>", "--pin", "<redacted>", "--bogus", "--token", "<redacted>"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestWriteHelpSensitiveOptions(t *testing.T) {
	p, err := NewParser([]Option{
		{Long: "token", Short: "t", Type: TypeString, Sensitive: true, Default: "hidden-secret"},
		{Long: "pin", Type: TypeInt, Sensitive: true, Required: true, Repeatable: true, Default: "42"},
		{Long: "burn", Type: TypeBool, Sensitive: true, Default: "true"},
		{Long: "user", Type: TypeString, Default: "bob"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	if err := New("tool", "", nil, noop).WriteHelp(nil, p, &buf); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, secret := range []string{"hidden-secret", "42"} {
		if strings.Contains(got, secret) {
			t.Errorf("help leaks sensitive default %q:\n%s", secret, got)
		}
	}
	wantLines := []string{
		"  -t, --token <string>  (sensitive)\n",
		"  --pin <int>  (required, repeatable, sensitive)\n",
		"  --burn  (sensitive)\n",
		"  --user <string>  (default: bob)\n",
	}
	for _, line := range wantLines {
		if !strings.Contains(got, line) {
			t.Errorf("help missing line %q, got:\n%s", line, got)
		}
	}
}

func TestSensitiveInvalidDefaultIsSpecErrorWithoutLeak(t *testing.T) {
	const secret = "not-an-int"
	_, err := NewParser([]Option{{Long: "pin", Type: TypeInt, Default: secret, Sensitive: true}}, nil)
	if !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("err = %v, want ErrInvalidSpec", err)
	}
	var se *SpecError
	if !errors.As(err, &se) {
		t.Fatalf("err %T is not *SpecError", err)
	}
	if se.Name != "pin" {
		t.Errorf("SpecError.Name = %q, want pin", se.Name)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("spec error leaks default %q: %v", secret, err)
	}

	// Non-sensitive invalid defaults keep their existing, value-bearing text.
	_, err = NewParser([]Option{{Long: "pin", Type: TypeInt, Default: secret}}, nil)
	if err == nil || !strings.Contains(err.Error(), secret) {
		t.Fatalf("non-sensitive spec error = %v, want it to quote %q", err, secret)
	}
}

// assertRedactedParseError checks an invalid-value ParseError for a
// sensitive option: class fields survive, but neither the message, Value
// nor Token carries the raw value.
func assertRedactedParseError(t *testing.T, err error, name, short string, source Source, secret, wantToken string) {
	t.Helper()
	if !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("err = %v, want ErrInvalidValue", err)
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err %T is not *ParseError", err)
	}
	if pe.Name != name {
		t.Errorf("Name = %q, want %q", pe.Name, name)
	}
	if pe.Short != short {
		t.Errorf("Short = %q, want %q", pe.Short, short)
	}
	if pe.Source != source {
		t.Errorf("Source = %v, want %v", pe.Source, source)
	}
	if pe.Value != redactedReplacement {
		t.Errorf("Value = %q, want %q", pe.Value, redactedReplacement)
	}
	if pe.Token != wantToken {
		t.Errorf("Token = %q, want %q", pe.Token, wantToken)
	}
	for _, text := range []string{err.Error(), pe.Token, pe.Value} {
		if strings.Contains(text, secret) {
			t.Errorf("error text leaks %q: %q (token=%q value=%q)", secret, text, pe.Token, pe.Value)
		}
	}
	if !strings.Contains(err.Error(), redactedReplacement) {
		t.Errorf("error message lacks redaction marker: %v", err)
	}
}

func TestSensitiveCommandLineConversionFailuresRedacted(t *testing.T) {
	p := sensitiveTestParser(t)
	const secret = "s3cr3t-not-an-int"
	cases := []struct {
		args      []string
		short     string
		wantToken string
	}{
		{[]string{"--pin", secret}, "", "--pin"},
		{[]string{"--pin=" + secret}, "", "--pin=<redacted>"},
		{[]string{"-i" + secret}, "i", "-i<redacted>"},
		{[]string{"-i=" + secret}, "i", "-i=<redacted>"},
		{[]string{"-vi" + secret}, "i", "-vi<redacted>"},
		{[]string{"-i", secret}, "i", "-i"},
	}
	for _, c := range cases {
		_, err := p.Parse(c.args)
		assertRedactedParseError(t, err, "pin", c.short, SourceNone, secret, c.wantToken)
	}

	// Sensitive bool with an invalid explicit value.
	_, err := p.Parse([]string{"--burn=maybe"})
	assertRedactedParseError(t, err, "burn", "", SourceNone, "maybe", "--burn=<redacted>")
}

func TestSensitiveEnvironmentFailureRedacted(t *testing.T) {
	p := sensitiveTestParser(t)
	const secret = "env-s3cr3t"
	_, err := p.ParseWithSources(nil, nil, map[string]string{"TOKEN": ""}) // empty is fine for strings
	if err != nil {
		t.Fatalf("empty string env should convert: %v", err)
	}

	pin, err := NewParser([]Option{{Long: "pin", Type: TypeInt, Sensitive: true, EnvVar: "PIN"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pin.ParseWithSources(nil, nil, map[string]string{"PIN": secret})
	assertRedactedParseError(t, err, "pin", "", SourceEnvironment, secret, "")
	if ClassifyError(err) != ClassConfiguration {
		t.Errorf("class = %s, want configuration; redaction must not change class", ClassifyError(err))
	}
}

func TestSensitiveConfigFailureRedacted(t *testing.T) {
	p := sensitiveTestParser(t)
	const secret = "cfg-s3cr3t"
	_, err := p.ParseWithSources(nil, map[string][]string{"token": {secret}}, nil)
	if err != nil {
		t.Fatalf("string config value must still parse: %v", err)
	}

	pin, err := NewParser([]Option{{Long: "pin", Type: TypeInt, Sensitive: true, ConfigKey: "pin"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pin.ParseWithSources(nil, map[string][]string{"pin": {secret}}, nil)
	assertRedactedParseError(t, err, "pin", "", SourceConfig, secret, "")
	if ClassifyError(err) != ClassConfiguration {
		t.Errorf("class = %s, want configuration", ClassifyError(err))
	}

	// Repeatable sensitive options: an invalid element is redacted too.
	keys, err := NewParser([]Option{{Long: "key", Type: TypeInt, Sensitive: true, Repeatable: true, ConfigKey: "keys"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = keys.ParseWithSources(nil, map[string][]string{"keys": {"1", secret}}, nil)
	assertRedactedParseError(t, err, "key", "", SourceConfig, secret, "")
}

func TestSensitiveConfigFileFailureRedacted(t *testing.T) {
	pin, err := NewParser([]Option{{Long: "pin", Type: TypeInt, Sensitive: true, ConfigKey: "pin"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("pin: file-s3cr3t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = pin.ParseConfigFiles(nil, []string{path}, nil)
	if !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("err = %v, want ErrInvalidValue", err)
	}
	if strings.Contains(err.Error(), "file-s3cr3t") {
		t.Errorf("config-file error leaks value: %v", err)
	}
	var pe *ParseError
	if errors.As(err, &pe) {
		if pe.Source != SourceConfig || pe.Value != redactedReplacement {
			t.Errorf("Source=%v Value=%q", pe.Source, pe.Value)
		}
	}
}

func TestSensitiveChangesNoParseSemantics(t *testing.T) {
	p, err := NewParser([]Option{
		{Long: "token", Short: "t", Type: TypeString, Sensitive: true, EnvVar: "TOKEN", ConfigKey: "token",
			Default: "dflt-secret"},
		{Long: "pin", Type: TypeInt, Sensitive: true},
		{Long: "tag", Type: TypeString, Sensitive: true, Repeatable: true},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Result still exposes the original value through every accessor.
	r, err := p.Parse([]string{"--token", "cli-secret", "--pin=7", "--tag", "a", "--tag=b"})
	if err != nil {
		t.Fatal(err)
	}
	if r.String("token") != "cli-secret" || r.Int("pin") != 7 {
		t.Fatalf("token=%q pin=%d", r.String("token"), r.Int("pin"))
	}
	if got := r.Strings("tag"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("tags = %v", got)
	}
	if !r.Provided("token") || r.Count("tag") != 2 || r.Source("token") != SourceCommandLine {
		t.Fatalf("provided=%v count=%d source=%v", r.Provided("token"), r.Count("tag"), r.Source("token"))
	}

	// Precedence: environment and config apply to sensitive options exactly
	// as elsewhere, and defaults still surface raw.
	r, err = p.ParseWithSources(nil, map[string][]string{"token": {"cfg-secret"}}, map[string]string{"TOKEN": "env-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if r.String("token") != "env-secret" || r.Source("token") != SourceEnvironment {
		t.Fatalf("env precedence: token=%q source=%v", r.String("token"), r.Source("token"))
	}
	r, err = p.ParseWithSources(nil, map[string][]string{"token": {"cfg-secret"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.String("token") != "cfg-secret" || r.Source("token") != SourceConfig {
		t.Fatalf("config layer: token=%q source=%v", r.String("token"), r.Source("token"))
	}
	r, err = p.Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.String("token") != "dflt-secret" || r.Source("token") != SourceDefault || r.Provided("token") {
		t.Fatalf("default: token=%q source=%v provided=%v", r.String("token"), r.Source("token"), r.Provided("token"))
	}
}

func TestSensitiveNonRedactedFailuresUnchanged(t *testing.T) {
	// Missing-value and required failures carry no value text and keep
	// their existing behavior for sensitive options.
	p, err := NewParser([]Option{
		{Long: "token", Short: "t", Type: TypeString, Sensitive: true, Required: true},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Parse([]string{"--token"})
	if !errors.Is(err, ErrMissingValue) {
		t.Fatalf("missing value: %v", err)
	}
	_, err = p.Parse(nil)
	if !errors.Is(err, ErrRequired) {
		t.Fatalf("required: %v", err)
	}
	var pe *ParseError
	if errors.As(err, &pe) && pe.Name != "token" {
		t.Fatalf("required error name = %q", pe.Name)
	}
}

func TestRedactArgsWithCommandTree(t *testing.T) {
	p := sensitiveTestParser(t)
	var seen []string
	root := New("tool", "", nil, nil)
	if err := root.Add(New("run", "", nil, func(path []string, args []string, stdout, stderr io.Writer) error {
		seen = p.RedactArgs(args)
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := root.Execute(context.Background(), []string{"run", "--token", "s3cr3t", "x"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if want := []string{"--token", "<redacted>", "x"}; !reflect.DeepEqual(seen, want) {
		t.Fatalf("handler saw %v, want %v", seen, want)
	}
}

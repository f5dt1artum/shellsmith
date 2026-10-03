package command

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// This file adds an optional declarative argument parser to the command
// package. It is deliberately independent of the command tree: a handler
// that wants parsing calls (*Parser).Parse on the raw args it receives; a
// handler that does not keeps receiving them verbatim. Node, Add, Execute
// and HandlerFunc are unaffected.

// Sentinel errors describing parser-specification and parsing failures.
//
// Spec failures wrap ErrInvalidSpec in *SpecError; parse failures wrap one
// of the other sentinels in *ParseError. Callers distinguish classes with
// errors.Is and recover the offending spec name or raw token with
// errors.As.
var (
	// ErrInvalidSpec indicates an illegal parser specification: an invalid
	// or duplicated long or short option name, a default value that does
	// not match the option type, more than one variadic positional
	// argument, or a variadic argument that is not last. NewParser returns
	// no usable parser when it is wrapped in the returned error.
	ErrInvalidSpec = errors.New("command: invalid parser specification")

	// ErrUnknownOption indicates a token that looks like an option but
	// matches neither a declared long nor short option. Tokens only become
	// positional after a "--" separator.
	ErrUnknownOption = errors.New("command: unknown option")

	// ErrMissingValue indicates an option that takes a value reached the
	// end of the arguments without one.
	ErrMissingValue = errors.New("command: missing option value")

	// ErrInvalidValue indicates an option value that could not be
	// converted to the option's declared type.
	ErrInvalidValue = errors.New("command: invalid option value")

	// ErrRequired indicates a required option was not provided on the
	// command line (defaults never satisfy this) or a named positional
	// argument was not supplied.
	ErrRequired = errors.New("command: missing required argument")

	// ErrUnexpectedArgument indicates more positional tokens were given
	// than the positional specification accepts.
	ErrUnexpectedArgument = errors.New("command: unexpected positional argument")
)

// ValueType selects the strong type applied to an option's values.
type ValueType int

const (
	// TypeString accepts any value unchanged.
	TypeString ValueType = iota
	// TypeBool parses values with strconv.ParseBool; a bare option spells
	// true without consuming the next token.
	TypeBool
	// TypeInt parses decimal integers with strconv.Atoi (negatives
	// included).
	TypeInt
	// TypeDuration parses values with time.ParseDuration (e.g. "500ms",
	// "-2s").
	TypeDuration
)

// Source identifies which layer supplied an option's effective value in a
// Result, or the layer a parse failure arose from.
type Source int

const (
	// SourceNone is returned by Result.Source for an unknown name.
	SourceNone Source = iota
	// SourceCommandLine is the command-line arguments layer, the highest
	// precedence.
	SourceCommandLine
	// SourceEnvironment is the environment map layer.
	SourceEnvironment
	// SourceConfig is the configuration map layer.
	SourceConfig
	// SourceDefault is the option's declared default, the lowest
	// precedence.
	SourceDefault
)

// String renders a Source for diagnostics; unknown values (including
// SourceNone) render as "none".
func (s Source) String() string {
	switch s {
	case SourceCommandLine:
		return "command-line"
	case SourceEnvironment:
		return "environment"
	case SourceConfig:
		return "config"
	case SourceDefault:
		return "default"
	default:
		return "none"
	}
}

// Option declares one command-line option.
//
// Long is the option's canonical name used after "--" and as the key for
// result lookups; it must be non-empty, contain only ASCII letters,
// digits, hyphens and underscores, and not start with a hyphen. Short is
// either empty (no short spelling) or a single ASCII letter or digit used
// after a single "-". Default holds the textual default value, interpreted
// according to Type; the empty string denotes the type's zero value and is
// always valid. Required marks the option as needing an explicit
// occurrence; a default never satisfies this. Repeatable allows the option
// to occur more than once, with every value retained in occurrence order;
// a non-repeatable option keeps the value of its last occurrence.
//
// ConfigKey and EnvVar optionally bind the option to, respectively, a
// configuration entry and an environment variable consulted by
// ParseWithSources. Each must either be empty (no binding to that source)
// or non-empty and unique across every option: assigning the same
// non-empty ConfigKey or EnvVar to more than one option is an invalid
// specification rejected by NewParser. An empty binding never matches
// anything, and configuration keys or environment names matching no
// declared binding are ignored.
type Option struct {
	Long       string
	Short      string
	Type       ValueType
	Default    string
	Required   bool
	Repeatable bool
	// ConfigKey binds the option to an entry of the configuration map
	// handed to ParseWithSources; empty means configuration values never
	// apply to this option.
	ConfigKey string
	// EnvVar binds the option to an entry of the environment map handed
	// to ParseWithSources; empty means environment values never apply.
	EnvVar string
}

// Positional declares one named positional argument. When Variadic is true
// the positional collects zero or more trailing tokens; at most one
// variadic positional is allowed and it must be the final positional. All
// non-variadic positionals are mandatory: missing one fails parsing with
// ErrRequired.
type Positional struct {
	Name     string
	Variadic bool
}

// SpecError wraps ErrInvalidSpec and identifies the offending part of the
// specification.
type SpecError struct {
	// Name is the offending option's long name or the positional name,
	// when one is involved.
	Name string
	// Short is the offending short spelling, when one is involved.
	Short string

	msg string
}

func (e *SpecError) Error() string { return e.msg }
func (e *SpecError) Unwrap() error { return ErrInvalidSpec }

// ParseError wraps one of the parse-time sentinels (ErrUnknownOption,
// ErrMissingValue, ErrInvalidValue, ErrRequired or
// ErrUnexpectedArgument).
type ParseError struct {
	// Name is the canonical long option name or positional spec name the
	// failure refers to. It is empty for an unknown option or an
	// unexpected token, which by definition match no spec.
	Name string
	// Short is the short spelling of the option when the failure arose
	// from a short cluster.
	Short string
	// Token is the raw command-line token involved: the unknown option
	// token, the option token left without a value, or the surplus
	// positional token.
	Token string
	// Value is the raw value text that failed type conversion, when
	// applicable.
	Value string
	// Source is the layer the failing value came from. It is
	// SourceCommandLine for failures produced by Parse (and for the
	// command-line layer of ParseWithSources), and SourceEnvironment or
	// SourceConfig when a value from that layer failed conversion. It is
	// SourceNone for failures that carry no value layer (unknown options,
	// missing values, missing required arguments, unexpected positionals).
	Source Source

	kind error
	msg  string
}

func (e *ParseError) Error() string { return e.msg }
func (e *ParseError) Unwrap() error { return e.kind }

// compiledOption is an Option plus its already-parsed default value.
type compiledOption struct {
	Option
	def any
}

type compiledPositional struct {
	Positional
}

// Parser is an immutable, concurrency-safe compiled specification. A
// Parser may be shared across goroutines; each Parse or ParseWithSources
// call builds an independent Result and touches no process-level state.
type Parser struct {
	options []compiledOption
	args    []compiledPositional
	long    map[string]*compiledOption
	short   map[byte]*compiledOption
	config  map[string]*compiledOption
	env     map[string]*compiledOption
}

// NewParser compiles options and positionals into a Parser. Both slices
// are copied, so later mutations by the caller cannot affect the parser.
// An invalid specification yields a *SpecError wrapping ErrInvalidSpec and
// a nil parser. Besides name and default validation, non-empty ConfigKey
// and EnvVar bindings must each be unique across options.
func NewParser(options []Option, positionals []Positional) (*Parser, error) {
	p := &Parser{
		options: make([]compiledOption, 0, len(options)),
		args:    make([]compiledPositional, 0, len(positionals)),
		long:    make(map[string]*compiledOption, len(options)),
		short:   make(map[byte]*compiledOption, len(options)),
		config:  make(map[string]*compiledOption, len(options)),
		env:     make(map[string]*compiledOption, len(options)),
	}

	seenLong := make(map[string]struct{}, len(options))
	seenShort := make(map[byte]string, len(options))
	seenConfig := make(map[string]string, len(options))
	seenEnv := make(map[string]string, len(options))
	for i := range options {
		src := options[i]
		if !validOptionName(src.Long) {
			return nil, &SpecError{Name: src.Long, msg: fmt.Sprintf("command: invalid option name %q", src.Long)}
		}
		if _, dup := seenLong[src.Long]; dup {
			return nil, &SpecError{Name: src.Long, msg: fmt.Sprintf("command: duplicate option %q", src.Long)}
		}
		if src.Short != "" {
			if len(src.Short) != 1 || !isShortChar(src.Short[0]) {
				return nil, &SpecError{Name: src.Long, Short: src.Short,
					msg: fmt.Sprintf("command: invalid short option %q for option %q", src.Short, src.Long)}
			}
			if other, dup := seenShort[src.Short[0]]; dup {
				return nil, &SpecError{Name: src.Long, Short: src.Short,
					msg: fmt.Sprintf("command: short option %q already used by %q", src.Short, other)}
			}
		}
		if src.ConfigKey != "" {
			if other, dup := seenConfig[src.ConfigKey]; dup {
				return nil, &SpecError{Name: src.Long,
					msg: fmt.Sprintf("command: config key %q already bound to option %q", src.ConfigKey, other)}
			}
		}
		if src.EnvVar != "" {
			if other, dup := seenEnv[src.EnvVar]; dup {
				return nil, &SpecError{Name: src.Long,
					msg: fmt.Sprintf("command: environment variable %q already bound to option %q", src.EnvVar, other)}
			}
		}
		def, err := zeroOrConvert(src.Type, src.Default)
		if err != nil {
			return nil, &SpecError{Name: src.Long, Short: src.Short,
				msg: fmt.Sprintf("command: invalid default %q for option %q: %v", src.Default, src.Long, err)}
		}
		p.options = append(p.options, compiledOption{Option: src, def: def})
		seenLong[src.Long] = struct{}{}
		if src.Short != "" {
			seenShort[src.Short[0]] = src.Long
		}
		if src.ConfigKey != "" {
			seenConfig[src.ConfigKey] = src.Long
		}
		if src.EnvVar != "" {
			seenEnv[src.EnvVar] = src.Long
		}
	}
	for i := range p.options {
		co := &p.options[i]
		p.long[co.Long] = co
		if co.Short != "" {
			p.short[co.Short[0]] = co
		}
		if co.ConfigKey != "" {
			p.config[co.ConfigKey] = co
		}
		if co.EnvVar != "" {
			p.env[co.EnvVar] = co
		}
	}

	seenPos := make(map[string]struct{}, len(positionals))
	for i := range positionals {
		src := positionals[i]
		if src.Name == "" {
			return nil, &SpecError{msg: "command: positional argument name must not be empty"}
		}
		if _, dup := seenPos[src.Name]; dup {
			return nil, &SpecError{Name: src.Name, msg: fmt.Sprintf("command: duplicate positional argument %q", src.Name)}
		}
		seenPos[src.Name] = struct{}{}
		p.args = append(p.args, compiledPositional{Positional: src})
	}
	// More-than-one variadic is reported at the second (and any later)
	// variadic and takes priority over the not-last check.
	if countVariadic(p.args) > 1 {
		found := 0
		for _, a := range p.args {
			if !a.Variadic {
				continue
			}
			found++
			if found == 2 {
				return nil, &SpecError{Name: a.Name,
					msg: fmt.Sprintf("command: more than one variadic positional argument: %q", a.Name)}
			}
		}
	}
	for i, a := range p.args {
		if a.Variadic && i != len(p.args)-1 {
			return nil, &SpecError{Name: a.Name,
				msg: fmt.Sprintf("command: variadic argument %q must be the last positional argument", a.Name)}
		}
	}

	return p, nil
}

func countVariadic(args []compiledPositional) int {
	n := 0
	for _, a := range args {
		if a.Variadic {
			n++
		}
	}
	return n
}

// validOptionName mirrors the command-name character set: non-empty, no
// leading hyphen, ASCII letters, digits, hyphens and underscores.
func validOptionName(s string) bool {
	if s == "" || s[0] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_':
		default:
			return false
		}
	}
	return true
}

func isShortChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// zeroOrConvert converts textual values per t. The empty string yields the
// zero value, so an unset default is always representable.
func zeroOrConvert(t ValueType, raw string) (any, error) {
	if raw == "" {
		switch t {
		case TypeString:
			return "", nil
		case TypeBool:
			return false, nil
		case TypeInt:
			return 0, nil
		case TypeDuration:
			return time.Duration(0), nil
		default:
			return nil, fmt.Errorf("unknown value type %d", t)
		}
	}
	return convertValue(t, raw)
}

func convertValue(t ValueType, raw string) (any, error) {
	switch t {
	case TypeString:
		return raw, nil
	case TypeBool:
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, err
		}
		return v, nil
	case TypeInt:
		v, err := strconv.Atoi(raw)
		if err != nil {
			return nil, err
		}
		return v, nil
	case TypeDuration:
		v, err := time.ParseDuration(raw)
		if err != nil {
			return nil, err
		}
		return v, nil
	default:
		return nil, fmt.Errorf("unknown value type %d", t)
	}
}

// Result holds one successful parse. Values are indexed by option long
// name; a Result is independent of every other Result, the Parser, and the
// caller's source maps.
type Result struct {
	parser *Parser
	// cmd holds the values supplied on the command line in occurrence
	// order. A non-repeatable option's slice has length at most one and
	// holds its last occurrence. It is the only layer counted by Provided
	// and Count.
	cmd map[string][]any
	// eff holds the effective values after layering: the whole winning
	// layer, converted per the option's type. Scalar and slice accessors
	// read these.
	eff map[string][]any
	// src records the winning layer of each option, or SourceDefault.
	src map[string]Source
	pos []string
}

// Parse decodes args according to the parser's specification, exactly as
// ParseWithSources with no configuration or environment layer. It never
// modifies args: long options accept both "--name=value" and
// "--name value", non-bool short options accept "-p value" (and the
// attached "-pvalue"/"-p=value" forms), short boolean options cluster as
// in "-vq", and everything after a standalone "--" token is positional.
// An option awaiting a value consumes the next token even when it begins
// with a hyphen, so negative integers and durations parse; an unknown
// hyphen-prefixed token never silently becomes positional.
//
// On failure the error is a *ParseError and the returned result is nil.
// An empty args slice still applies defaults and enforces required
// options and positionals.
func (p *Parser) Parse(args []string) (*Result, error) {
	return p.ParseWithSources(args, nil, nil)
}

// ParseWithSources decodes args together with a configuration layer and
// an environment layer. None of the arguments are modified, including the
// maps and the value slices inside config.
//
// Effective precedence, consulted as whole layers, is command line, then
// environment, then configuration, then the option's Default: once a
// higher layer supplies an option the lower layers never contribute to
// it, and values are never merged across layers. The configuration map
// may list several raw values for one key: a non-repeatable option takes
// the last element and a repeatable option keeps every element; an empty
// (nil or zero-length) slice means the key was not supplied. The
// environment layer contributes at most one value per option. A present
// but empty environment value or configuration element is still an
// explicit value and is converted according to the option's type (an
// empty value therefore fails for int and duration options). Config and
// environment entries matching no declared binding are ignored; the
// process environment is never read.
//
// Required may be satisfied by the command line, environment, or
// configuration layer, never by a default. A value from the environment
// or configuration layer that fails conversion returns a *ParseError
// wrapping ErrInvalidValue with Name set to the canonical long name,
// Value to the raw value, Source to the originating layer, and an empty
// Token; no partial Result is returned. Command-line failures keep the
// *ParseError shape produced by Parse.
func (p *Parser) ParseWithSources(args []string, config map[string][]string, environ map[string]string) (*Result, error) {
	r := &Result{
		parser: p,
		cmd:    make(map[string][]any),
		eff:    make(map[string][]any),
		src:    make(map[string]Source),
	}

	onlyPositional := false
	for i := 0; i < len(args); i++ {
		token := args[i]

		if onlyPositional {
			r.pos = append(r.pos, token)
			continue
		}
		if token == "--" {
			onlyPositional = true
			continue
		}

		switch {
		case strings.HasPrefix(token, "--"):
			if err := p.parseLong(token, args, &i, r); err != nil {
				return nil, err
			}
		case len(token) > 1 && token[0] == '-':
			if err := p.parseShort(token, args, &i, r); err != nil {
				return nil, err
			}
		default:
			// A bare "-" is an ordinary positional token.
			r.pos = append(r.pos, token)
		}
	}

	if err := p.applyLowerLayers(r, config, environ); err != nil {
		return nil, err
	}
	if err := p.checkRequired(r); err != nil {
		return nil, err
	}
	return r, nil
}

func (p *Parser) parseLong(token string, args []string, i *int, r *Result) error {
	body := token[2:]
	name, raw, hasValue := strings.Cut(body, "=")
	opt := p.long[name]
	if opt == nil {
		return &ParseError{Token: token, kind: ErrUnknownOption,
			msg: fmt.Sprintf("command: unknown option %q", token)}
	}

	if opt.Type == TypeBool {
		if !hasValue {
			r.record(opt, true)
			return nil
		}
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return &ParseError{Name: opt.Long, Token: token, Value: raw, Source: SourceCommandLine, kind: ErrInvalidValue,
				msg: fmt.Sprintf("command: invalid value %q for option %q: %v", raw, opt.Long, err)}
		}
		r.record(opt, b)
		return nil
	}

	value, err := p.takeLongValue(opt, token, raw, hasValue, args, i)
	if err != nil {
		return err
	}
	converted, err := convertValue(opt.Type, value)
	if err != nil {
		return &ParseError{Name: opt.Long, Token: token, Value: value, Source: SourceCommandLine, kind: ErrInvalidValue,
			msg: fmt.Sprintf("command: invalid value %q for option %q: %v", value, opt.Long, err)}
	}
	r.record(opt, converted)
	return nil
}

// takeLongValue returns the explicit "=" value or consumes the next token,
// even when it starts with a hyphen.
func (p *Parser) takeLongValue(opt *compiledOption, token, raw string, hasValue bool, args []string, i *int) (string, error) {
	if hasValue {
		return raw, nil
	}
	if *i+1 >= len(args) {
		return "", &ParseError{Name: opt.Long, Token: token, kind: ErrMissingValue,
			msg: fmt.Sprintf("command: option %q requires a value", opt.Long)}
	}
	*i++
	return args[*i], nil
}

func (p *Parser) parseShort(token string, args []string, i *int, r *Result) error {
	for j := 1; j < len(token); j++ {
		ch := token[j]
		opt := p.short[ch]
		if opt == nil {
			return &ParseError{Token: token, kind: ErrUnknownOption,
				msg: fmt.Sprintf("command: unknown option %q", token)}
		}
		if opt.Type == TypeBool {
			r.record(opt, true)
			continue
		}

		var value string
		if j+1 < len(token) {
			// Attached value: -pvalue or -p=value; the remainder of the
			// cluster belongs to this option regardless of content.
			value = token[j+1:]
			value = strings.TrimPrefix(value, "=")
			j = len(token)
		} else {
			if *i+1 >= len(args) {
				return &ParseError{Name: opt.Long, Short: opt.Short, Token: token, kind: ErrMissingValue,
					msg: fmt.Sprintf("command: option %q requires a value", opt.Long)}
			}
			*i++
			value = args[*i]
		}
		converted, err := convertValue(opt.Type, value)
		if err != nil {
			return &ParseError{Name: opt.Long, Short: opt.Short, Token: token, Value: value, Source: SourceCommandLine, kind: ErrInvalidValue,
				msg: fmt.Sprintf("command: invalid value %q for option %q: %v", value, opt.Long, err)}
		}
		r.record(opt, converted)
	}
	return nil
}

// record stores one command-line occurrence. Non-repeatable options keep
// only the last occurrence; repeatable options retain all of them in
// order.
func (r *Result) record(opt *compiledOption, value any) {
	if !opt.Repeatable {
		r.cmd[opt.Long] = []any{value}
		return
	}
	r.cmd[opt.Long] = append(r.cmd[opt.Long], value)
}

// applyLowerLayers finalizes each option's effective slice and winning
// source. Command-line occurrences win outright when present; otherwise
// the environment map, then the configuration map, supplies converted
// values; an option supplied by none of the three keeps its default.
// Options are visited in declaration order so a conversion failure is
// reported deterministically regardless of map iteration.
func (p *Parser) applyLowerLayers(r *Result, config map[string][]string, environ map[string]string) error {
	for i := range p.options {
		opt := &p.options[i]
		if vs, ok := r.cmd[opt.Long]; ok && len(vs) > 0 {
			r.eff[opt.Long] = vs
			r.src[opt.Long] = SourceCommandLine
			continue
		}
		if opt.EnvVar != "" {
			if raw, present := environ[opt.EnvVar]; present {
				converted, err := convertValue(opt.Type, raw)
				if err != nil {
					return &ParseError{Name: opt.Long, Value: raw, Source: SourceEnvironment, kind: ErrInvalidValue,
						msg: fmt.Sprintf("command: invalid value %q for option %q from environment: %v", raw, opt.Long, err)}
				}
				r.eff[opt.Long] = []any{converted}
				r.src[opt.Long] = SourceEnvironment
				continue
			}
		}
		if opt.ConfigKey != "" {
			if raws, present := config[opt.ConfigKey]; present && len(raws) > 0 {
				// A non-repeatable option selects only the last element;
				// a repeatable one keeps every element in order.
				selected := raws
				if !opt.Repeatable {
					selected = raws[len(raws)-1:]
				}
				values := make([]any, 0, len(selected))
				for _, raw := range selected {
					converted, err := convertValue(opt.Type, raw)
					if err != nil {
						return &ParseError{Name: opt.Long, Value: raw, Source: SourceConfig, kind: ErrInvalidValue,
							msg: fmt.Sprintf("command: invalid value %q for option %q from config: %v", raw, opt.Long, err)}
					}
					values = append(values, converted)
				}
				r.eff[opt.Long] = values
				r.src[opt.Long] = SourceConfig
				continue
			}
		}
		r.eff[opt.Long] = []any{opt.def}
		r.src[opt.Long] = SourceDefault
	}
	return nil
}

func (p *Parser) checkRequired(r *Result) error {
	for i := range p.options {
		opt := &p.options[i]
		if opt.Required && len(r.cmd[opt.Long]) == 0 && r.src[opt.Long] != SourceEnvironment && r.src[opt.Long] != SourceConfig {
			return &ParseError{Name: opt.Long, Short: opt.Short, kind: ErrRequired,
				msg: fmt.Sprintf("command: missing required option %q", opt.Long)}
		}
	}

	n := len(p.args)
	fixed := n
	if n > 0 && p.args[n-1].Variadic {
		fixed = n - 1
	}
	if len(r.pos) < fixed {
		name := p.args[len(r.pos)].Name
		return &ParseError{Name: name, kind: ErrRequired,
			msg: fmt.Sprintf("command: missing required argument %q", name)}
	}
	if n == 0 || !p.args[n-1].Variadic {
		if len(r.pos) > n {
			return &ParseError{Token: r.pos[n], kind: ErrUnexpectedArgument,
				msg: fmt.Sprintf("command: unexpected argument %q", r.pos[n])}
		}
	}
	return nil
}

// scalar returns the effective single value for an option: the last value
// of the winning layer, or the parsed default. Unknown names yield nil.
func (r *Result) scalar(name string) any {
	if vs, ok := r.eff[name]; ok && len(vs) > 0 {
		return vs[len(vs)-1]
	}
	if opt, ok := r.parser.long[name]; ok {
		return opt.def
	}
	return nil
}

// String returns the effective string value for the named option.
func (r *Result) String(name string) string {
	v, _ := r.scalar(name).(string)
	return v
}

// Bool returns the effective bool value for the named option.
func (r *Result) Bool(name string) bool {
	v, _ := r.scalar(name).(bool)
	return v
}

// Int returns the effective int value for the named option.
func (r *Result) Int(name string) int {
	v, _ := r.scalar(name).(int)
	return v
}

// Duration returns the effective time.Duration value for the named option.
func (r *Result) Duration(name string) time.Duration {
	v, _ := r.scalar(name).(time.Duration)
	return v
}

// effectiveSlice returns the winning layer's converted values for the
// slice accessors. A default never contributes, so a default-only option
// yields an empty slice; the single environment value or the retained
// configuration elements are returned when those layers win.
func (r *Result) effectiveSlice(name string) []any {
	if r.src[name] == SourceDefault {
		return nil
	}
	return r.eff[name]
}

// Strings returns every effective string value for the named option in
// layer order: all occurrences from the winning layer (every command-line
// occurrence, the single environment value, or every retained
// configuration element), and none from any other layer. A default-only
// option is not included here; use String for the effective value.
func (r *Result) Strings(name string) []string {
	vs := r.effectiveSlice(name)
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Bools returns every effective bool value for the named option from the
// winning layer. A default is not included.
func (r *Result) Bools(name string) []bool {
	vs := r.effectiveSlice(name)
	out := make([]bool, 0, len(vs))
	for _, v := range vs {
		if b, ok := v.(bool); ok {
			out = append(out, b)
		}
	}
	return out
}

// Ints returns every effective int value for the named option from the
// winning layer. A default is not included.
func (r *Result) Ints(name string) []int {
	vs := r.effectiveSlice(name)
	out := make([]int, 0, len(vs))
	for _, v := range vs {
		if n, ok := v.(int); ok {
			out = append(out, n)
		}
	}
	return out
}

// Durations returns every effective duration value for the named option
// from the winning layer. A default is not included.
func (r *Result) Durations(name string) []time.Duration {
	vs := r.effectiveSlice(name)
	out := make([]time.Duration, 0, len(vs))
	for _, v := range vs {
		if d, ok := v.(time.Duration); ok {
			out = append(out, d)
		}
	}
	return out
}

// Source reports the whole layer that supplied the effective value of the
// named option: SourceCommandLine, SourceEnvironment, SourceConfig, or
// SourceDefault. An unknown name returns SourceNone.
func (r *Result) Source(name string) Source {
	if s, ok := r.src[name]; ok {
		return s
	}
	return SourceNone
}

// Provided reports whether the named option occurred explicitly on the
// command line. Values supplied by the environment or configuration layer
// and values coming from a default all report false.
func (r *Result) Provided(name string) bool {
	return len(r.cmd[name]) > 0
}

// Count returns the number of explicit command-line occurrences of the
// named option. A non-repeatable option provided at least once reports 1;
// an option supplied only by the environment, configuration, or a default
// reports 0.
func (r *Result) Count(name string) int {
	return len(r.cmd[name])
}

// Args returns a copy of all positional tokens in command-line order,
// including the tokens collected by a trailing variadic argument.
func (r *Result) Args() []string {
	return append([]string(nil), r.pos...)
}

// NArg returns the number of positional tokens.
func (r *Result) NArg() int { return len(r.pos) }

// Arg returns the value of the named non-variadic positional argument, or
// "" when no such fixed positional exists or it has no value. Use
// ArgValues for a variadic positional.
func (r *Result) Arg(name string) string {
	for i, a := range r.parser.args {
		if a.Name == name && !a.Variadic && i < len(r.pos) {
			return r.pos[i]
		}
	}
	return ""
}

// ArgValues returns the values collected by the named variadic positional
// argument in order. It returns nil for a non-variadic or unknown name.
func (r *Result) ArgValues(name string) []string {
	for i, a := range r.parser.args {
		if a.Name == name && a.Variadic && i <= len(r.pos) {
			return append([]string(nil), r.pos[i:]...)
		}
	}
	return nil
}

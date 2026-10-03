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
	// or duplicated long or short option name, a duplicated non-empty
	// ConfigKey or EnvVar, a default value that does not match the option
	// type, more than one variadic positional argument, or a variadic
	// argument that is not last. NewParser returns no usable parser when it
	// is wrapped in the returned error.
	ErrInvalidSpec = errors.New("command: invalid parser specification")

	// ErrUnknownOption indicates a token that looks like an option but
	// matches neither a declared long nor short option. Tokens only become
	// positional after a "--" separator.
	ErrUnknownOption = errors.New("command: unknown option")

	// ErrMissingValue indicates an option that takes a value reached the
	// end of the arguments without one.
	ErrMissingValue = errors.New("command: missing option value")

	// ErrInvalidValue indicates an option value that could not be
	// converted to the option's declared type. Values may come from the
	// command line, from an environment variable, or from a config
	// element; a ParseError's Source field records which.
	ErrInvalidValue = errors.New("command: invalid option value")

	// ErrRequired indicates a required option was not supplied on any
	// explicit layer (command line, environment, or config; defaults
	// never satisfy this) or a named positional argument was not
	// supplied.
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

// Source identifies which layer supplied an option's effective value.
type Source int

const (
	// SourceNone is reported for an unknown option name.
	SourceNone Source = iota
	// SourceCommandLine means the value appeared on the command line.
	SourceCommandLine
	// SourceEnvironment means the value came from the environ map passed
	// to ParseWithSources.
	SourceEnvironment
	// SourceConfig means the value came from the config map passed to
	// ParseWithSources.
	SourceConfig
	// SourceDefault means no command-line, environment, or config value
	// was supplied; the option's Default is in effect.
	SourceDefault
)

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
// ConfigKey optionally binds the option to an entry of the config map
// passed to ParseWithSources; EnvVar optionally binds it to an entry of
// the environ map. Either empty means the option does not participate in
// that layer. ConfigKey and EnvVar values must each be unique across all
// options: a non-empty key shared by two options makes the whole
// specification invalid. Parse ignores both bindings entirely.
type Option struct {
	Long       string
	Short      string
	Type       ValueType
	Default    string
	ConfigKey  string
	EnvVar     string
	Required   bool
	Repeatable bool
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
	// Source identifies the layer a value-conversion failure came from:
	// SourceEnvironment or SourceConfig for ParseWithSources, or
	// SourceNone when the failure arose on the command line (or carries no
	// value).
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
// Parser may be shared across goroutines; each Parse call builds an
// independent Result and touches no process-level state.
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
// a nil parser.
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
		def, err := zeroOrConvert(src.Type, src.Default)
		if err != nil {
			return nil, &SpecError{Name: src.Long, Short: src.Short,
				msg: fmt.Sprintf("command: invalid default %q for option %q: %v", src.Default, src.Long, err)}
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
// name; a Result is independent of every other Result and of the Parser.
type Result struct {
	parser *Parser
	// layers holds the values supplied by each layer in descending
	// priority: command line, environment, config. Command-line values are
	// always present (possibly empty); the other two are nil for Parse.
	// A non-repeatable option's slice has length at most one and holds its
	// last value; a repeatable option's slice retains every value in order.
	layers [3]optionValues
	// values is the command-line layer, kept as an alias of layers[0]
	// because record and the parse routines refer to it by that name.
	values optionValues
	pos    []string
}

// optionValues maps an option long name to the values supplied within one
// layer, in occurrence order.
type optionValues map[string][]any

// record stores one supplied value within a layer. Non-repeatable options
// keep only the last value; repeatable options retain all of them in
// order.
func (o optionValues) record(opt *compiledOption, value any) {
	if !opt.Repeatable {
		o[opt.Long] = []any{value}
		return
	}
	o[opt.Long] = append(o[opt.Long], value)
}

// Parse decodes args according to the parser's specification. It never
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
//
// Parse is exactly ParseWithSources with no config and no environment:
// ConfigKey and EnvVar bindings have no effect and the process
// environment is never read.
func (p *Parser) Parse(args []string) (*Result, error) {
	return p.ParseWithSources(args, nil, nil)
}

// ParseWithSources decodes args with two additional explicit value
// layers. Precedence is fixed, command line first: a value supplied by a
// higher layer replaces the lower layer wholesale for that option —
// layers are never concatenated. The order is args, then environ, then
// config, then the option's Default.
//
// config binds by Option.ConfigKey: a key may carry several raw values in
// order; a non-repeatable option takes the last one while a repeatable
// option retains every value. A present but empty slice counts as not
// supplied. environ binds by Option.EnvVar and contributes the single
// map value. A present-but-empty environment value or config element is
// still an explicit value and is converted according to the option's
// type (an empty element therefore fails TypeInt/TypeDuration/TypeBool
// conversion). Config keys and environment variables no option binds are
// ignored, and the process environment is never read.
//
// Neither args, config nor environ (nor the slices inside config) is
// modified or retained. On failure the error is a *ParseError — a
// conversion failure in config or environ wraps ErrInvalidValue with
// Name set to the canonical long name, Value to the raw value, Source to
// SourceConfig or SourceEnvironment, and Token empty — and the returned
// result is nil, with no partial values.
func (p *Parser) ParseWithSources(args []string, config map[string][]string, environ map[string]string) (*Result, error) {
	r := &Result{parser: p}
	r.layers[0] = make(optionValues)
	r.values = r.layers[0]

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

	if err := p.applyEnviron(r, environ); err != nil {
		return nil, err
	}
	if err := p.applyConfig(r, config); err != nil {
		return nil, err
	}

	if err := p.checkRequired(r); err != nil {
		return nil, err
	}
	return r, nil
}

// record stores one command-line occurrence.
func (r *Result) record(opt *compiledOption, value any) {
	r.values.record(opt, value)
}

// applyEnviron fills the environment layer for every option absent from
// the command line whose EnvVar is present in environ. A present binding
// always supplies exactly one value, even when it is empty.
func (p *Parser) applyEnviron(r *Result, environ map[string]string) error {
	if len(environ) == 0 {
		return nil
	}
	layer := make(optionValues)
	for i := range p.options {
		opt := &p.options[i]
		if opt.EnvVar == "" {
			continue
		}
		if _, onCLI := r.values[opt.Long]; onCLI {
			continue
		}
		raw, present := environ[opt.EnvVar]
		if !present {
			continue
		}
		converted, err := convertValue(opt.Type, raw)
		if err != nil {
			return &ParseError{Name: opt.Long, Value: raw, Source: SourceEnvironment, kind: ErrInvalidValue,
				msg: fmt.Sprintf("command: invalid environment value %q for %s %q: %v", raw, opt.EnvVar, opt.Long, err)}
		}
		layer.record(opt, converted)
	}
	r.layers[1] = layer
	return nil
}

// applyConfig fills the config layer for every option absent from both
// the command line and the environment whose ConfigKey is present in
// config. A present but empty slice supplies nothing; every element of a
// non-empty slice — including empty elements — is an explicit value.
func (p *Parser) applyConfig(r *Result, config map[string][]string) error {
	if len(config) == 0 {
		return nil
	}
	layer := make(optionValues)
	for i := range p.options {
		opt := &p.options[i]
		if opt.ConfigKey == "" {
			continue
		}
		if _, onCLI := r.values[opt.Long]; onCLI {
			continue
		}
		if _, inEnv := r.layers[1][opt.Long]; inEnv {
			continue
		}
		raws, present := config[opt.ConfigKey]
		if !present || len(raws) == 0 {
			continue
		}
		if !opt.Repeatable {
			raw := raws[len(raws)-1]
			converted, err := convertValue(opt.Type, raw)
			if err != nil {
				return &ParseError{Name: opt.Long, Value: raw, Source: SourceConfig, kind: ErrInvalidValue,
					msg: fmt.Sprintf("command: invalid config value %q for %s %q: %v", raw, opt.ConfigKey, opt.Long, err)}
			}
			layer.record(opt, converted)
			continue
		}
		for _, raw := range raws {
			converted, err := convertValue(opt.Type, raw)
			if err != nil {
				return &ParseError{Name: opt.Long, Value: raw, Source: SourceConfig, kind: ErrInvalidValue,
					msg: fmt.Sprintf("command: invalid config value %q for %s %q: %v", raw, opt.ConfigKey, opt.Long, err)}
			}
			layer.record(opt, converted)
		}
	}
	r.layers[2] = layer
	return nil
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
			return &ParseError{Name: opt.Long, Token: token, Value: raw, kind: ErrInvalidValue,
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
		return &ParseError{Name: opt.Long, Token: token, Value: value, kind: ErrInvalidValue,
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
			return &ParseError{Name: opt.Long, Short: opt.Short, Token: token, Value: value, kind: ErrInvalidValue,
				msg: fmt.Sprintf("command: invalid value %q for option %q: %v", value, opt.Long, err)}
		}
		r.record(opt, converted)
	}
	return nil
}

func (p *Parser) checkRequired(r *Result) error {
	for i := range p.options {
		opt := &p.options[i]
		if !opt.Required || r.supplied(opt.Long) {
			continue
		}
		return &ParseError{Name: opt.Long, Short: opt.Short, kind: ErrRequired,
			msg: fmt.Sprintf("command: missing required option %q", opt.Long)}
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

// supplied reports whether any of the three explicit layers carries the
// option. A default never supplies.
func (r *Result) supplied(name string) bool {
	for _, layer := range r.layers {
		if vs, ok := layer[name]; ok && len(vs) > 0 {
			return true
		}
	}
	return false
}

// effective returns the values of the highest-priority explicit layer
// carrying the option, along with that layer's Source, in descending
// priority: command line, environment, config. Values from lower layers
// are never mixed in. Unknown names and unsupplied options report
// SourceNone.
func (r *Result) effective(name string) ([]any, Source) {
	srcs := [...]Source{SourceCommandLine, SourceEnvironment, SourceConfig}
	for i, layer := range r.layers {
		if vs, ok := layer[name]; ok && len(vs) > 0 {
			return vs, srcs[i]
		}
	}
	return nil, SourceNone
}

// scalar returns the effective single value for an option: the last
// value of the highest-priority supplying layer, otherwise the parsed
// default (or the zero value). Unknown names yield nil.
func (r *Result) scalar(name string) any {
	if vs, _ := r.effective(name); len(vs) > 0 {
		return vs[len(vs)-1]
	}
	if opt, ok := r.parser.long[name]; ok {
		return opt.def
	}
	return nil
}

// Source reports which layer finally supplied the named option's
// effective value: SourceCommandLine, SourceEnvironment, SourceConfig or
// SourceDefault. An unknown name reports SourceNone.
func (r *Result) Source(name string) Source {
	if _, src := r.effective(name); src != SourceNone {
		return src
	}
	if _, ok := r.parser.long[name]; ok {
		return SourceDefault
	}
	return SourceNone
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

// Strings returns every effective string value for the named option in
// order, taken whole from the single layer Source reports: every
// command-line occurrence, the one environment value, or every config
// element. Defaults are not included; use String for the scalar value.
func (r *Result) Strings(name string) []string {
	vs, _ := r.effective(name)
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Bools returns every effective bool value for the named option from the
// supplying layer. Defaults are not included.
func (r *Result) Bools(name string) []bool {
	vs, _ := r.effective(name)
	out := make([]bool, 0, len(vs))
	for _, v := range vs {
		if b, ok := v.(bool); ok {
			out = append(out, b)
		}
	}
	return out
}

// Ints returns every effective int value for the named option from the
// supplying layer. Defaults are not included.
func (r *Result) Ints(name string) []int {
	vs, _ := r.effective(name)
	out := make([]int, 0, len(vs))
	for _, v := range vs {
		if n, ok := v.(int); ok {
			out = append(out, n)
		}
	}
	return out
}

// Durations returns every effective duration value for the named option
// from the supplying layer. Defaults are not included.
func (r *Result) Durations(name string) []time.Duration {
	vs, _ := r.effective(name)
	out := make([]time.Duration, 0, len(vs))
	for _, v := range vs {
		if d, ok := v.(time.Duration); ok {
			out = append(out, d)
		}
	}
	return out
}

// Provided reports whether the named option occurred explicitly on the
// command line. Values from the environment, config, or a default report
// false.
func (r *Result) Provided(name string) bool {
	return len(r.values[name]) > 0
}

// Count returns the number of explicit command-line occurrences of the
// named option. A non-repeatable option provided at least once reports 1;
// environment, config, and default-only options report 0.
func (r *Result) Count(name string) int {
	return len(r.values[name])
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

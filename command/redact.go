package command

import (
	"strconv"
	"strings"
)

// This file adds secret redaction to the parser: the Sensitive option
// declaration and Parser.RedactArgs. Redaction never changes parsing:
// Parse, ParseWithSources and ParseConfigFiles apply exactly the same
// syntax, type conversion, layering and required checks to a sensitive
// option as to any other; only text the framework itself emits (help,
// conversion-failure errors, and the copy RedactArgs returns) is masked.

// redactedReplacement replaces a sensitive value in any framework-produced
// text or redacted argument slice.
const redactedReplacement = "<redacted>"

// invalidValueError builds the ErrInvalidValue ParseError for a failed
// conversion, masking the raw value (and any token embedding it) when the
// option is sensitive. Name, Short, Source and the error class are
// preserved either way; only the value-bearing text differs.
//
// shortPos is -1 for a long option and for environment/config failures;
// for a short-option failure it is the index within token of the option
// character, so an attached value in a cluster ("-vqpsecret") is masked
// without disturbing the preceding cluster.
func invalidValueError(opt *compiledOption, token, value string, shortPos int, source Source, cause error) *ParseError {
	shortName := ""
	if shortPos >= 0 {
		shortName = opt.Short
	}
	if !opt.Sensitive {
		return &ParseError{Name: opt.Long, Short: shortName, Token: token, Value: value, Source: source,
			kind: ErrInvalidValue,
			msg:  invalidValueMessage(source, opt, value, false, cause)}
	}
	redactedToken := ""
	if token != "" {
		if shortPos >= 0 {
			redactedToken = redactedShortToken(token, shortPos)
		} else {
			redactedToken = redactedLongToken(token)
		}
	}
	return &ParseError{Name: opt.Long, Short: shortName, Token: redactedToken, Value: redactedReplacement, Source: source,
		kind: ErrInvalidValue,
		msg:  invalidValueMessage(source, opt, redactedReplacement, true, cause)}
}

// invalidValueMessage renders the message for a conversion failure. For a
// sensitive option the converter's cause is omitted on purpose: the
// standard conversion errors quote the offending text (e.g.
// strconv.Atoi's `parsing "abc"`) and would leak it.
func invalidValueMessage(source Source, opt *compiledOption, displayValue string, sensitive bool, cause error) string {
	reason := ""
	if !sensitive {
		reason = ": " + cause.Error()
	}
	switch source {
	case SourceEnvironment:
		return "command: invalid environment value " + strconv.Quote(displayValue) + " for " + opt.EnvVar + " " + strconv.Quote(opt.Long) + reason
	case SourceConfig:
		return "command: invalid config value " + strconv.Quote(displayValue) + " for " + opt.ConfigKey + " " + strconv.Quote(opt.Long) + reason
	}
	return "command: invalid value " + strconv.Quote(displayValue) + " for option " + strconv.Quote(opt.Long) + reason
}

// redactedLongToken masks the value embedded in an attached long token
// ("--name=value"). A token without "=" ("--name") carries no value and
// is returned unchanged.
func redactedLongToken(token string) string {
	name, _, ok := strings.Cut(token[2:], "=")
	if !ok {
		return token
	}
	return "--" + name + "=" + redactedReplacement
}

// redactedShortToken masks the attached value at or after optPos in a
// short token ("-pvalue", "-p=value"), keeping the dash, the option
// character and every cluster character before it. The separate form
// ("-p") embeds no value and is returned unchanged.
func redactedShortToken(token string, optPos int) string {
	if optPos+1 >= len(token) {
		return token
	}
	prefix := token[:optPos+1]
	if token[optPos+1] == '=' {
		return prefix + "=" + redactedReplacement
	}
	return prefix + redactedReplacement
}

// RedactArgs returns a copy of args in which values supplied to declared
// sensitive value-taking options are replaced with "<redacted>". The
// input slice and its backing array are never modified.
//
// Recognition follows Parse's syntax but performs no validation: unknown
// options, incomplete tokens and malformed values never cause an error
// and are left untouched. Before a standalone "--" separator:
//
//   - A sensitive value-taking option written as "--token value" masks the
//     following element; written as "--token=value" only the text after
//     "=" is masked.
//   - Its short form masks only the attached value in "-tvalue" and
//     "-t=value", preserving any short-option cluster preceding it
//     ("-vqtsecret" becomes "-vqt<redacted>"), and masks the following
//     element of the separate "-t value" form.
//   - Every repetition of a repeatable sensitive option is masked.
//   - A sensitive boolean option's bare long or short spellings (and
//     clusters) are left as-is; only the explicit value of "--flag=value"
//     is masked.
//
// Known non-sensitive value-taking options still consume their following
// element as a value, so it can never be mistaken for another option's
// secret. Positional arguments, unknown or incomplete tokens, and every
// element at or after a standalone "--" keep their original bytes and
// order.
//
// RedactArgs reads only the immutable parser specification and is safe
// for concurrent use on a shared Parser.
func (p *Parser) RedactArgs(args []string) []string {
	if args == nil {
		return nil
	}
	out := make([]string, len(args))
	copy(out, args)

	onlyPositional := false
	for i := 0; i < len(out); i++ {
		token := out[i]
		if onlyPositional {
			break
		}
		if token == "--" {
			onlyPositional = true
			continue
		}
		switch {
		case strings.HasPrefix(token, "--"):
			i = p.redactLong(out, i)
		case len(token) > 1 && token[0] == '-':
			i = p.redactShort(out, i)
		}
	}
	return out
}

// redactLong masks one long token at index i and returns the index of the
// last element it consumed.
func (p *Parser) redactLong(args []string, i int) int {
	token := args[i]
	name, _, hasValue := strings.Cut(token[2:], "=")
	opt := p.long[name]
	if opt == nil {
		return i
	}
	if opt.Type == TypeBool {
		if opt.Sensitive && hasValue {
			args[i] = "--" + name + "=" + redactedReplacement
		}
		return i
	}
	if !opt.Sensitive {
		// A known value-taking option consumes the following element
		// regardless of its content, exactly as Parse does.
		if !hasValue && i+1 < len(args) {
			i++
		}
		return i
	}
	if hasValue {
		args[i] = "--" + name + "=" + redactedReplacement
		return i
	}
	if i+1 < len(args) {
		args[i+1] = redactedReplacement
		i++
	}
	return i
}

// redactShort walks one short cluster at index i, masks every sensitive
// value attached to or following it, and returns the index of the last
// element it consumed. An unknown short character aborts the cluster:
// nothing is masked and no following element is taken, mirroring the
// point at which Parse would reject the token.
func (p *Parser) redactShort(args []string, i int) int {
	token := args[i]
	for j := 1; j < len(token); j++ {
		opt := p.short[token[j]]
		if opt == nil {
			return i
		}
		if opt.Type == TypeBool {
			continue
		}
		// A non-bool ends the cluster: the remainder is its attached
		// value, or the next element is its separate value.
		if j+1 < len(token) {
			if opt.Sensitive {
				args[i] = redactedShortToken(token, j)
			}
			return i
		}
		if i+1 < len(args) {
			if opt.Sensitive {
				args[i+1] = redactedReplacement
			}
			i++
		}
		return i
	}
	return i
}

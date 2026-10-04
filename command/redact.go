package command

import "strings"

// This file implements (*Parser).RedactArgs, a logging-oriented
// projection of the parser's option grammar: given a raw argument vector
// it returns an independent copy whose sensitive values are replaced with
// "<redacted>". It recognizes options with the same token-level rules as
// Parse, but it performs no validation: unknown options, missing values
// and type-incompatible values are never errors and never stop the walk.

// redactedMarker replaces every redacted sensitive value, including the
// value portion of an attached token.
const redactedMarker = "<redacted>"

// RedactArgs returns a copy of args in which values belonging to options
// declared Sensitive are replaced with "<redacted>". The returned slice is
// freshly allocated and neither it nor args is modified, so RedactArgs is
// safe to call concurrently on a shared Parser and safe to apply to the
// same args Parse is consuming.
//
// Recognition mirrors Parse up to (but never past) a standalone "--"
// token, which is itself retained; tokens at and after it keep their
// original bytes and order. For a sensitive non-boolean option:
//
//   - "--token value" replaces the following token, and every repetition
//     is handled ("--token a --token b" -> "--token <redacted> --token
//     <redacted>");
//   - "--token=value" keeps the option spelling and replaces only the
//     text after "=" ("--token=<redacted>");
//   - "-tvalue" and "-t=value" keep the preceding short cluster, if any,
//     and replace only the attached value ("-vtsecret" -> "-vt<redacted>");
//   - "-t value" replaces the following token.
//
// A sensitive boolean option carries no value: its bare long and short
// spellings (including clusters) are left unchanged, and "--flag=value"
// masks only the explicit value ("--flag=<redacted>").
//
// Options not declared sensitive, unknown or incomplete tokens, and
// positional arguments are copied byte-for-byte. In particular a known
// non-sensitive value-taking option still consumes the following token,
// so a value that merely looks like a sensitive option name is never
// mistaken for one ("--name --token" keeps "--token" as name's value).
// RedactArgs never converts values and reports no error.
func (p *Parser) RedactArgs(args []string) []string {
	out := append([]string(nil), args...)

	onlyPositional := false
	for i := 0; i < len(out); i++ {
		token := out[i]
		if onlyPositional {
			continue
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

// redactLong redacts token out[i] when it names a sensitive option and
// returns the index of the last consumed token (so the caller's loop does
// not walk into a value it took). Unknown long options consume nothing.
func (p *Parser) redactLong(out []string, i int) int {
	body := out[i][2:]
	name, _, hasValue := strings.Cut(body, "=")
	opt := p.long[name]
	if opt == nil {
		return i
	}
	if opt.Type == TypeBool {
		if hasValue && opt.Sensitive {
			out[i] = "--" + name + "=" + redactedMarker
		}
		return i
	}
	if !opt.Sensitive {
		// A known non-sensitive value-taking option still owns the next
		// token; skip it so it cannot be read as an option of its own.
		if !hasValue && i+1 < len(out) {
			i++
		}
		return i
	}
	if hasValue {
		out[i] = "--" + name + "=" + redactedMarker
		return i
	}
	if i+1 < len(out) {
		out[i+1] = redactedMarker
		i++
	}
	return i
}

// redactShort walks the short cluster of out[i] the way Parse does,
// redacting sensitive options it meets, and returns the index of the last
// consumed token. An unknown short character abandons the cluster without
// consuming anything further.
func (p *Parser) redactShort(out []string, i int) int {
	token := out[i]
	for j := 1; j < len(token); j++ {
		opt := p.short[token[j]]
		if opt == nil {
			return i
		}
		if opt.Type == TypeBool {
			continue
		}
		if j+1 < len(token) {
			// Attached value: -tvalue or -t=value, possibly preceded by a
			// cluster of boolean shorts such as "-vtsecret". Only the
			// remainder belongs to this option; keep the leading equals.
			if opt.Sensitive {
				prefix := token[:j+1]
				if token[j+1] == '=' {
					prefix = token[:j+2]
				}
				out[i] = prefix + redactedMarker
			}
			return i
		}
		if opt.Sensitive && i+1 < len(out) {
			out[i+1] = redactedMarker
		}
		// Known value-taking option, sensitive or not: the following
		// token is its value and must not be scanned as an option.
		if i+1 < len(out) {
			i++
		}
		return i
	}
	return i
}

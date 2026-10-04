package command

import (
	"fmt"
	"io"
	"strings"
)

// This file adds on-demand plain-text help to the command tree. Help is
// produced only through (*Node).WriteHelp: Execute never generates it, no
// implicit meaning is given to "--help" or "-h", and handlers still run
// exclusively from Execute.

// WriteHelp writes plain-text help for the command path selects from n to w.
//
// path holds command segments resolved from n downward, one per level; an
// empty path (or nil) selects n itself. Segments match with the same exact,
// case-sensitive rules as Execute — primary names and aliases — and every
// resolved alias is rendered as the child's primary name, so the usage line
// always carries the canonical path. The path slice is never modified.
//
// parser is optional. With a nil parser only command information is shown.
// A non-nil parser adds "[options]" to the usage line when it declares
// options, followed by the positional specifications in declaration order
// ("<name>" for required positionals, "[<name>...]" for the variadic one),
// and contributes an Options section.
//
// Layout, independent of terminal width: the first line is "Usage: " plus
// the canonical path; when the selected node has children it gains
// "[command]" if the node carries a handler and "<command>" otherwise. A
// non-empty description follows verbatim after one blank line. Children
// appear in a "Commands:" section in Add order, each line carrying the
// primary name, aliases in registration order, and the description. Options
// appear in an "Options:" section in declaration order with their short and
// long names, a "<string>", "<int>" or "<duration>" placeholder for
// non-boolean options, and "(required)", "(repeatable)", "(sensitive)" and
// "(default: value)" markers as applicable. A sensitive option never shows
// its default value. Sections are separated by exactly one blank line,
// every line ends with LF, the text ends with a single LF, and no ANSI
// sequences are emitted.
//
// If a path segment matches no child, WriteHelp returns a *LookupError
// wrapping ErrUnknownCommand; Path holds the canonical names resolved
// before the failure and Arg the original segment, and no byte reaches w.
// A write failure is returned unchanged; WriteHelp does not modify the
// tree, the parser, path, or any process state, never runs a handler, and
// is safe for concurrent use.
func (n *Node) WriteHelp(path []string, parser *Parser, w io.Writer) error {
	current := n
	canonical := []string{current.name}
	for _, segment := range path {
		next, ok := current.byName[segment]
		if !ok {
			resolved := make([]string, len(canonical))
			copy(resolved, canonical)
			return &LookupError{Path: resolved, Arg: segment}
		}
		current = next
		canonical = append(canonical, next.name)
	}

	var b strings.Builder
	appendUsage(&b, canonical, current, parser)
	if current.desc != "" {
		b.WriteByte('\n')
		b.WriteString(current.desc)
		b.WriteByte('\n')
	}
	if len(current.children) > 0 {
		appendCommands(&b, current)
	}
	if parser != nil && len(parser.options) > 0 {
		appendOptions(&b, parser)
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// appendUsage writes the single "Usage:" line for the resolved node.
func appendUsage(b *strings.Builder, canonical []string, current *Node, parser *Parser) {
	b.WriteString("Usage: ")
	b.WriteString(strings.Join(canonical, " "))
	if len(current.children) > 0 {
		if current.handler != nil {
			b.WriteString(" [command]")
		} else {
			b.WriteString(" <command>")
		}
	}
	if parser != nil {
		if len(parser.options) > 0 {
			b.WriteString(" [options]")
		}
		for _, a := range parser.args {
			if a.Variadic {
				fmt.Fprintf(b, " [<%s>...]", a.Name)
			} else {
				fmt.Fprintf(b, " <%s>", a.Name)
			}
		}
	}
	b.WriteByte('\n')
}

// appendCommands writes the "Commands:" section, children in Add order.
func appendCommands(b *strings.Builder, current *Node) {
	b.WriteString("\nCommands:\n")
	for _, child := range current.children {
		b.WriteString("  ")
		b.WriteString(child.name)
		if len(child.aliases) > 0 {
			b.WriteString(" (")
			b.WriteString(strings.Join(child.aliases, ", "))
			b.WriteByte(')')
		}
		if child.desc != "" {
			b.WriteString("  ")
			b.WriteString(child.desc)
		}
		b.WriteByte('\n')
	}
}

// appendOptions writes the "Options:" section in declaration order.
func appendOptions(b *strings.Builder, parser *Parser) {
	b.WriteString("\nOptions:\n")
	for i := range parser.options {
		opt := &parser.options[i]
		b.WriteString("  ")
		if opt.Short != "" {
			b.WriteByte('-')
			b.WriteString(opt.Short)
			b.WriteString(", ")
		}
		b.WriteString("--")
		b.WriteString(opt.Long)
		switch opt.Type {
		case TypeString:
			b.WriteString(" <string>")
		case TypeInt:
			b.WriteString(" <int>")
		case TypeDuration:
			b.WriteString(" <duration>")
		}

		var markers []string
		if opt.Required {
			markers = append(markers, "required")
		}
		if opt.Repeatable {
			markers = append(markers, "repeatable")
		}
		if opt.Sensitive {
			markers = append(markers, "sensitive")
		}
		if opt.Default != "" && !opt.Sensitive {
			markers = append(markers, "default: "+opt.Default)
		}
		if len(markers) > 0 {
			b.WriteString("  (")
			b.WriteString(strings.Join(markers, ", "))
			b.WriteByte(')')
		}
		b.WriteByte('\n')
	}
}

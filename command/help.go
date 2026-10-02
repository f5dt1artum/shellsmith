package command

import (
	"fmt"
	"io"
	"strings"
)

// This file adds plain-text help generation to the command tree. Help is
// produced only on demand through (*Node).WriteHelp: the package still
// never writes to the Execute writers, never runs handlers outside
// Execute, and gives no implicit meaning to "--help" or "-h".

// WriteHelp writes plain-text help for the command selected by path to w.
//
// path holds command names resolved from n downward, one segment per
// level; an empty path selects n itself. Segments are matched with the
// same exact, case-sensitive rules as Execute — primary names and aliases
// — and aliases are reported as the child's primary name in the output.
// The path slice is never modified.
//
// parser is optional: when nil only command information is shown; when
// non-nil the usage line also advertises "[options]" (only if the parser
// declares options) followed by the positional specifications in
// declaration order, and an Options section lists every declared option.
//
// The output does not depend on terminal width. The first line is
// "Usage: " followed by the canonical command path; when the selected
// node has subcommands the usage line gains "[command]" if the node has
// a handler and "<command>" otherwise. A non-empty description follows
// after one blank line, verbatim. Subcommands appear in a "Commands:"
// section in Add order, one per line with primary name, aliases in
// registration order, and description. Options appear in an "Options:"
// section in declaration order with short and long names, a "<string>",
// "<int>" or "<duration>" placeholder for non-boolean options, and
// required, repeatable and non-empty-default markers. Sections are
// separated by exactly one blank line, lines end with LF, the text ends
// with a single LF, and no ANSI control sequences are emitted. Repeated
// calls over the same tree and parser produce byte-identical output.
//
// When a path segment matches no child, WriteHelp returns a *LookupError
// wrapping ErrUnknownCommand — Path holds the canonical names resolved
// before the failure, Arg the original segment — and w receives no
// bytes. A write failure is returned unchanged; success returns nil.
// WriteHelp does not modify the tree, the parser, or any process state,
// never runs handlers, and is safe for concurrent use with itself and
// with Execute.
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
		canonical = append(canonical, current.name)
	}

	var b strings.Builder
	writeUsage(&b, canonical, current, parser)
	if current.desc != "" {
		b.WriteByte('\n')
		b.WriteString(current.desc)
		b.WriteByte('\n')
	}
	if len(current.children) > 0 {
		writeCommands(&b, current)
	}
	if parser != nil && len(parser.options) > 0 {
		writeOptions(&b, parser)
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// writeUsage emits the "Usage: ..." line for the selected node.
func writeUsage(b *strings.Builder, canonical []string, current *Node, parser *Parser) {
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
		for i := range parser.args {
			a := &parser.args[i]
			if a.Variadic {
				fmt.Fprintf(b, " [<%s>...]", a.Name)
			} else {
				fmt.Fprintf(b, " <%s>", a.Name)
			}
		}
	}
	b.WriteByte('\n')
}

// writeCommands emits the "Commands:" section listing the node's children
// in Add order.
func writeCommands(b *strings.Builder, current *Node) {
	b.WriteString("\nCommands:\n")
	for _, child := range current.children {
		b.WriteString("  ")
		b.WriteString(child.name)
		if len(child.aliases) > 0 {
			b.WriteString(" (")
			b.WriteString(strings.Join(child.aliases, ", "))
			b.WriteString(")")
		}
		if child.desc != "" {
			b.WriteString("  ")
			b.WriteString(child.desc)
		}
		b.WriteByte('\n')
	}
}

// writeOptions emits the "Options:" section listing the parser's options
// in declaration order.
func writeOptions(b *strings.Builder, parser *Parser) {
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
		if opt.Default != "" {
			markers = append(markers, "default: "+opt.Default)
		}
		if len(markers) > 0 {
			b.WriteString("  (")
			b.WriteString(strings.Join(markers, ", "))
			b.WriteString(")")
		}
		b.WriteByte('\n')
	}
}

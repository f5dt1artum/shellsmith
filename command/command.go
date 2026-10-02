// Package command provides a reusable command tree with arbitrary-depth
// subcommands and aliases.
//
// A node carries a primary name, a description, optional aliases and an
// optional handler. Child nodes are attached with [Command.Add] and a tree is
// dispatched with [Command.Execute]. The framework only performs exact,
// case-sensitive segment matching: it never guesses prefixes, never generates
// help or completion text, and never writes to the supplied output streams on
// its own.
package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Sentinel errors. All validation and dispatch failures reported by this
// package match one of these through errors.Is.
var (
	// ErrInvalidName is returned when a primary name or alias is empty,
	// contains characters outside [A-Za-z0-9_-], or starts with a hyphen.
	ErrInvalidName = errors.New("command: invalid name")

	// ErrInvalidCommand is returned when Add receives a nil node.
	ErrInvalidCommand = errors.New("command: invalid command")

	// ErrNameConflict is returned when a primary name or alias duplicates
	// any primary name or alias already registered under the same parent,
	// including a name repeated within the node being added.
	ErrNameConflict = errors.New("command: name conflict")

	// ErrAlreadyAttached is returned when the node passed to Add already
	// belongs to another (or the same) parent node.
	ErrAlreadyAttached = errors.New("command: command already attached")

	// ErrUnknownCommand is returned when no child matches the next
	// argument. The returned error is a [LookupError].
	ErrUnknownCommand = errors.New("command: unknown command")

	// ErrCommandRequired is returned when dispatch reaches a node that
	// carries no handler.
	ErrCommandRequired = errors.New("command: command required")
)

// Handler runs a matched command. path holds the canonical primary-name path
// that was matched (aliases resolve to their node's primary name), args holds
// every argument left after traversal in their original order, and stdout and
// stderr are the writers supplied to Execute. The framework returns whatever
// error the handler returns without inspecting or rewriting it.
type Handler func(ctx context.Context, path []string, args []string, stdout, stderr io.Writer) error

// Command is a node in the command tree. A node with children groups
// subcommands; a node without children is a leaf and, when it carries a
// handler, consumes all remaining arguments through Execute.
type Command struct {
	name        string
	description string
	aliases     []string
	handler     Handler

	parent   *Command
	children []*Command
}

// New creates a node with the given primary name, description, aliases and
// optional handler. It validates the name and aliases and returns
// ErrInvalidName (wrapped, usable with errors.Is) when any of them is invalid;
// it returns ErrNameConflict when the aliases repeat the primary name or each
// other. New does not attach the node to any parent.
func New(name, description string, aliases []string, handler Handler) (*Command, error) {
	if !validName(name) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	seen := make(map[string]struct{}, len(aliases)+1)
	seen[name] = struct{}{}
	cleaned := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		if !validName(alias) {
			return nil, fmt.Errorf("%w: %q", ErrInvalidName, alias)
		}
		if _, dup := seen[alias]; dup {
			return nil, fmt.Errorf("%w: %q", ErrNameConflict, alias)
		}
		seen[alias] = struct{}{}
		cleaned = append(cleaned, alias)
	}
	return &Command{
		name:        name,
		description: description,
		aliases:     cleaned,
		handler:     handler,
	}, nil
}

// validName reports whether name is non-empty, starts with a character other
// than '-', and contains only ASCII letters, digits, hyphens and underscores.
func validName(name string) bool {
	if name == "" || name[0] == '-' {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case 'a' <= c && c <= 'z':
		case 'A' <= c && c <= 'Z':
		case '0' <= c && c <= '9':
		case c == '-' || c == '_':
		default:
			return false
		}
	}
	return true
}

// Add attaches a child node and returns the receiver so calls can chain. The
// tree is left untouched when Add returns an error:
//
//   - ErrInvalidCommand when child is nil;
//   - ErrAlreadyAttached when child already has a parent;
//   - ErrNameConflict when the child's primary name or any alias duplicates a
//     primary name or alias registered under this parent.
//
// Successful registrations preserve the order in which children were added.
func (c *Command) Add(child *Command) (*Command, error) {
	if child == nil {
		return c, fmt.Errorf("%w: nil command", ErrInvalidCommand)
	}
	if child.parent != nil {
		return c, fmt.Errorf("%w: %q", ErrAlreadyAttached, child.name)
	}
	taken := make(map[string]struct{})
	for _, sibling := range c.children {
		for _, n := range sibling.names() {
			taken[n] = struct{}{}
		}
	}
	for _, offered := range child.names() {
		if _, dup := taken[offered]; dup {
			return c, fmt.Errorf("%w: %q", ErrNameConflict, offered)
		}
	}
	child.parent = c
	c.children = append(c.children, child)
	return c, nil
}

// names returns the primary name followed by the aliases, in registration
// order. New rejects duplicates, so this yields each name at most once.
func (c *Command) names() []string {
	out := make([]string, 0, 1+len(c.aliases))
	out = append(out, c.name)
	out = append(out, c.aliases...)
	return out
}

// LookupError describes a failed child lookup. It wraps ErrUnknownCommand and
// is usable with errors.Is.
type LookupError struct {
	// Path is the canonical primary-name path resolved before the failure.
	Path []string
	// Arg is the argument that matched no child.
	Arg string
}

func (e *LookupError) Error() string {
	return fmt.Sprintf("%s: %q (path %s)", ErrUnknownCommand, e.Arg, strings.Join(e.Path, " "))
}

func (e *LookupError) Unwrap() error { return ErrUnknownCommand }

// Execute walks the tree from the receiver using the leading elements of args,
// matching each argument exactly and case-sensitively against a child's
// primary name or aliases. On reaching a leaf node all remaining arguments are
// handed to its handler together with the matched canonical path and the
// supplied writers.
//
// If a node has children but the next argument matches none, Execute returns
// a [LookupError] without calling any handler or writing output. If arguments
// run out on a node without a handler, it returns ErrCommandRequired; a node
// with a handler is called even when no arguments remain. Handler errors are
// returned unchanged. A context already cancelled or expired before dispatch
// yields its context.Canceled or context.DeadlineExceeded directly.
func (c *Command) Execute(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	current := c
	// The canonical path reflects tree position, so start from the root even
	// when Execute is invoked on a node below it.
	path := canonicalPath(c)
	for len(current.children) > 0 {
		if len(args) == 0 {
			break
		}
		next, ok := current.findChild(args[0])
		if !ok {
			return &LookupError{Path: append([]string(nil), path...), Arg: args[0]}
		}
		current = next
		path = append(path, current.name)
		args = args[1:]
	}
	if current.handler == nil {
		return fmt.Errorf("%w: %s", ErrCommandRequired, strings.Join(path, " "))
	}
	return current.handler(ctx, path, args, stdout, stderr)
}

// canonicalPath returns the primary names from the tree root down to c.
func canonicalPath(c *Command) []string {
	depth := 0
	for n := c; n != nil; n = n.parent {
		depth++
	}
	path := make([]string, depth)
	for n := c; n != nil; n = n.parent {
		depth--
		path[depth] = n.name
	}
	return path
}

// findChild returns the child whose primary name or alias equals arg. Matching
// is exact and case-sensitive; the returned node is always the canonical
// child, so an alias hit still resolves to the primary name in the path.
func (c *Command) findChild(arg string) (*Command, bool) {
	for _, child := range c.children {
		if child.name == arg {
			return child, true
		}
		for _, alias := range child.aliases {
			if alias == arg {
				return child, true
			}
		}
	}
	return nil, false
}

// Name returns the node's primary name.
func (c *Command) Name() string { return c.name }

// Description returns the node's description.
func (c *Command) Description() string { return c.description }

// Aliases returns a copy of the node's aliases in registration order.
func (c *Command) Aliases() []string {
	return append([]string(nil), c.aliases...)
}

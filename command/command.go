// Package command implements a reusable command tree with arbitrary-depth
// subcommands and aliases.
//
// Nodes are created with New and attached to their parent with (*Node).Add.
// Execution starts at any node through (*Node).Execute, which descends one
// argument at a time, matching only exact, case-sensitive primary names and
// aliases; aliases resolve to the canonical primary name reported to the
// handler. Plain-text help is generated on demand through
// (*Node).WriteHelp; neither Execute nor WriteHelp interprets "--help" or
// "-h". Shell completion scripts for bash, zsh, fish, and powershell are
// generated on demand through (*Node).WriteCompletion. Apart from the
// WriteHelp and WriteCompletion outputs themselves the package never
// produces help text, completions, or log output, and it never writes to
// the stdout/stderr writers passed to Execute — only node handlers do.
package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Sentinel errors describing registration and execution failures.
//
// Callers must use errors.Is rather than direct comparison where the
// documentation promises a wrapping error type (ErrInvalidName and
// ErrUnknownCommand); the others are returned directly.
var (
	// ErrInvalidName indicates an empty or otherwise illegal primary name
	// or alias. It is wrapped by *NameError carrying the offending name.
	ErrInvalidName = errors.New("command: invalid command name")

	// ErrInvalidCommand is returned when Add receives a nil node (or a node
	// that cannot legally be attached, such as an ancestor of the parent).
	ErrInvalidCommand = errors.New("command: invalid command node")

	// ErrNameConflict indicates a duplicated primary name or alias under
	// the same parent, including aliases duplicated inside one node. It is
	// wrapped by *ConflictError carrying the duplicated name.
	ErrNameConflict = errors.New("command: command name conflict")

	// ErrAlreadyAttached indicates that Add was called for a node that
	// already has a parent. It is wrapped by *AttachedError.
	ErrAlreadyAttached = errors.New("command: command already attached to a parent")

	// ErrUnknownCommand marks a LookupError produced when an argument
	// matches neither a child's primary name nor one of its aliases.
	ErrUnknownCommand = errors.New("command: unknown command")

	// ErrCommandRequired is returned when arguments run out at a node that
	// has no handler, so no action can run.
	ErrCommandRequired = errors.New("command: command required but no argument remained")
)

// NameError wraps ErrInvalidName and reports the offending primary name or
// alias.
type NameError struct {
	// Name is the primary name or alias that failed validation.
	Name string
}

func (e *NameError) Error() string {
	return fmt.Sprintf("command: invalid command name %q", e.Name)
}

// Unwrap exposes ErrInvalidName for errors.Is and errors.As.
func (e *NameError) Unwrap() error { return ErrInvalidName }

// ConflictError wraps ErrNameConflict and reports the name that collided
// with an already registered primary name or alias.
type ConflictError struct {
	// Name is the primary name or alias that collided.
	Name string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("command: command name %q is already registered", e.Name)
}

// Unwrap exposes ErrNameConflict for errors.Is and errors.As.
func (e *ConflictError) Unwrap() error { return ErrNameConflict }

// AttachedError wraps ErrAlreadyAttached and reports the node that was
// attached for a second time.
type AttachedError struct {
	// Name is the primary name of the node that already has a parent.
	Name string
}

func (e *AttachedError) Error() string {
	return fmt.Sprintf("command: command %q is already attached to a parent", e.Name)
}

// Unwrap exposes ErrAlreadyAttached for errors.Is and errors.As.
func (e *AttachedError) Unwrap() error { return ErrAlreadyAttached }

// LookupError wraps ErrUnknownCommand. It is returned when a node with
// children is asked to resolve an argument that matches none of them. The
// handler is not invoked and nothing is written to the Execute writers.
type LookupError struct {
	// Path holds the canonical primary names resolved before the failure,
	// starting with the execution root.
	Path []string
	// Arg is the argument that could not be resolved.
	Arg string
}

func (e *LookupError) Error() string {
	if len(e.Path) == 0 {
		return fmt.Sprintf("command: unknown command %q", e.Arg)
	}
	return fmt.Sprintf("command: unknown command %q for %q", e.Arg, strings.Join(e.Path, " "))
}

// Unwrap exposes ErrUnknownCommand for errors.Is and errors.As.
func (e *LookupError) Unwrap() error { return ErrUnknownCommand }

// HandlerFunc is the action attached to a node.
//
// path holds the canonical primary names from the execution root to the
// matched node; an alias used during resolution is replaced by the child's
// primary name. args holds the arguments not consumed by resolution, in
// their original order and with their original content. stdout and stderr
// are the writers passed to Execute. Whatever error the handler returns is
// returned by Execute without wrapping.
type HandlerFunc func(path []string, args []string, stdout, stderr io.Writer) error

// Node is one command in the tree, identified by a primary name and zero or
// more aliases. A node may carry a handler, children, or both.
type Node struct {
	name     string
	desc     string
	aliases  []string
	handler  HandlerFunc
	parent   *Node
	children []*Node
	byName   map[string]*Node
}

// New creates an unattached node. handler may be nil; the aliases slice is
// copied so later caller mutations cannot affect the node. A node is not
// validated until it is attached with Add.
func New(name, description string, aliases []string, handler HandlerFunc) *Node {
	return &Node{
		name:    name,
		desc:    description,
		aliases: append([]string(nil), aliases...),
		handler: handler,
		byName:  make(map[string]*Node),
	}
}

// Name returns the node's primary name.
func (n *Node) Name() string { return n.name }

// Description returns the node's description.
func (n *Node) Description() string { return n.desc }

// Aliases returns a copy of the node's aliases in registration order.
func (n *Node) Aliases() []string { return append([]string(nil), n.aliases...) }

// validName reports whether s is a legal primary name or alias: non-empty,
// restricted to ASCII letters, digits, hyphens and underscores, and not
// starting with a hyphen.
func validName(s string) bool {
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

// validateOwnNames checks the primary name and aliases of an unattached
// candidate node: every name must be valid, and no name may repeat.
func validateOwnNames(child *Node) error {
	if !validName(child.name) {
		return &NameError{Name: child.name}
	}
	seen := make(map[string]struct{}, len(child.aliases)+1)
	seen[child.name] = struct{}{}
	for _, alias := range child.aliases {
		if !validName(alias) {
			return &NameError{Name: alias}
		}
		if _, dup := seen[alias]; dup {
			return &ConflictError{Name: alias}
		}
		seen[alias] = struct{}{}
	}
	return nil
}

// Add attaches child to n. The tree is only modified after every check has
// succeeded, so a failed Add leaves the tree untouched. On success children
// remain in the order in which they were added.
func (n *Node) Add(child *Node) error {
	if child == nil {
		return ErrInvalidCommand
	}
	if err := validateOwnNames(child); err != nil {
		return err
	}
	if child.parent != nil {
		return &AttachedError{Name: child.name}
	}
	// Attaching an ancestor (including n itself) would create a cycle and
	// corrupt traversal; treat it as an invalid node.
	for ancestor := n; ancestor != nil; ancestor = ancestor.parent {
		if ancestor == child {
			return fmt.Errorf("command: cannot attach %q into its own subtree: %w", child.name, ErrInvalidCommand)
		}
	}
	keys := make([]string, 0, len(child.aliases)+1)
	keys = append(keys, child.name)
	keys = append(keys, child.aliases...)
	for _, key := range keys {
		if _, exists := n.byName[key]; exists {
			return &ConflictError{Name: key}
		}
	}

	for _, key := range keys {
		n.byName[key] = child
	}
	n.children = append(n.children, child)
	child.parent = n
	return nil
}

// Execute resolves args against the tree rooted at n and runs the matched
// handler. While the current node has children, the next argument selects a
// child by exact primary name or alias; a miss yields a *LookupError
// wrapping ErrUnknownCommand. Once a node without
// children is reached, all remaining arguments go to its handler. A node
// with children but no next argument runs its own handler when present and
// returns ErrCommandRequired otherwise.
//
// If ctx is already canceled or past its deadline before resolution starts,
// ctx.Err() is returned directly. Handler errors are returned unchanged and
// the package itself never writes to stdout or stderr.
func (n *Node) Execute(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	current := n
	path := []string{current.name}

	for len(current.children) > 0 {
		if len(args) == 0 {
			if current.handler != nil {
				return current.handler(path, args, stdout, stderr)
			}
			return ErrCommandRequired
		}
		next, ok := current.byName[args[0]]
		if !ok {
			resolved := make([]string, len(path))
			copy(resolved, path)
			return &LookupError{Path: resolved, Arg: args[0]}
		}
		current = next
		path = append(path, current.name)
		args = args[1:]
	}

	if current.handler == nil {
		return ErrCommandRequired
	}
	return current.handler(path, args, stdout, stderr)
}

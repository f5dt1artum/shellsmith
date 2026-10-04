package command

import (
	"context"
	"errors"
)

// This file adds a single process-facing concern on top of the command
// tree, parser, configuration loader and completion generator: mapping an
// error to a stable class and process exit code. Classification is a pure
// function of the error value: it runs no handler, writes nothing, reads
// no environment, configuration file or other process state, never
// mutates the error or its fields, and is safe for concurrent use.

// ErrorClass is the stable textual class of an error.
type ErrorClass string

const (
	// ErrorNone means no error was supplied (nil).
	ErrorNone ErrorClass = "none"
	// ErrorUsage means the command line was used incorrectly: an unknown
	// command, a missing command, bad command-line arguments, or an
	// unsupported shell name.
	ErrorUsage ErrorClass = "usage"
	// ErrorConfiguration means configuration supplied outside the command
	// line (a configuration file or an environment variable) was missing,
	// unreadable, malformed, or carried an invalid value.
	ErrorConfiguration ErrorClass = "configuration"
	// ErrorDefinition means the program itself was built wrong: an invalid
	// command name, a name conflict, a node attached twice, an invalid
	// parser specification, or an attempt to attach an invalid node.
	ErrorDefinition ErrorClass = "definition"
	// ErrorCanceled means the context was canceled.
	ErrorCanceled ErrorClass = "canceled"
	// ErrorTimeout means the context's deadline was exceeded.
	ErrorTimeout ErrorClass = "timeout"
	// ErrorFailure means any other non-nil error.
	ErrorFailure ErrorClass = "failure"
)

// ClassifyError maps err to a stable ErrorClass by walking its full
// unwrap graph with errors.Is and errors.As, so wrapping with fmt.Errorf
// "%w" and errors.Join do not hide a cause.
//
// nil maps to ErrorNone. A joined or multiply wrapped error is classified
// by its highest-priority member, in the order
// canceled, timeout, configuration, usage, definition, failure: for
// example a join of a usage error with a context.Canceled is ErrorCanceled.
// Error text never affects the result; any non-nil error matching no
// known class maps to ErrorFailure.
func ClassifyError(err error) ErrorClass {
	if err == nil {
		return ErrorNone
	}
	switch bestClass(err) {
	case ErrorCanceled:
		return ErrorCanceled
	case ErrorTimeout:
		return ErrorTimeout
	case ErrorConfiguration:
		return ErrorConfiguration
	case ErrorUsage:
		return ErrorUsage
	case ErrorDefinition:
		return ErrorDefinition
	default:
		return ErrorFailure
	}
}

// ExitCode maps err to the process exit code for its class:
//
//	none          0
//	usage         2
//	configuration 78
//	definition    70
//	canceled      130
//	timeout       124
//	failure       1
func ExitCode(err error) int {
	switch ClassifyError(err) {
	case ErrorNone:
		return 0
	case ErrorUsage:
		return 2
	case ErrorConfiguration:
		return 78
	case ErrorDefinition:
		return 70
	case ErrorCanceled:
		return 130
	case ErrorTimeout:
		return 124
	default:
		return 1
	}
}

// errorClassRank orders classes by precedence; a larger rank wins when an
// error carries more than one recognizable cause. failure is rank 0, the
// implicit class of every non-nil error.
var errorClassRank = map[ErrorClass]int{
	ErrorFailure:       0,
	ErrorDefinition:    1,
	ErrorUsage:         2,
	ErrorConfiguration: 3,
	ErrorTimeout:       4,
	ErrorCanceled:      5,
}

// bestClass walks err's complete unwrap graph — including every branch of
// an errors.Join-style multi-cause error — and returns the class with the
// highest precedence found among its members, or ErrorFailure when no
// member matches a known class.
func bestClass(err error) ErrorClass {
	visited := make(map[error]bool)
	best := ErrorFailure

	var walk func(error)
	walk = func(e error) {
		if e == nil || visited[e] {
			return
		}
		visited[e] = true
		if errorClassRank[classifyOne(e)] > errorClassRank[best] {
			best = classifyOne(e)
		}
		for _, cause := range unwrapCauses(e) {
			walk(cause)
		}
	}
	walk(err)
	return best
}

// classifyOne maps a single graph node to its class without following
// unwraps. Membership is tested with errors.Is and errors.As.
func classifyOne(e error) ErrorClass {
	switch {
	case errors.Is(e, context.Canceled):
		return ErrorCanceled
	case errors.Is(e, context.DeadlineExceeded):
		return ErrorTimeout
	}

	var configErr *ConfigError
	if errors.As(e, &configErr) {
		return ErrorConfiguration
	}

	var parseErr *ParseError
	if errors.As(e, &parseErr) {
		if errors.Is(e, ErrInvalidValue) &&
			(parseErr.Source == SourceEnvironment || parseErr.Source == SourceConfig) {
			return ErrorConfiguration
		}
		return ErrorUsage
	}

	// Usage-class errors that are not ParseErrors. ErrCommandRequired is a
	// bare sentinel; LookupError wraps ErrUnknownCommand and ShellError
	// wraps ErrUnsupportedShell.
	switch {
	case errors.Is(e, ErrCommandRequired):
		return ErrorUsage
	case errors.Is(e, ErrUnknownCommand):
		return ErrorUsage
	case errors.Is(e, ErrUnsupportedShell):
		return ErrorUsage
	}

	// Definition-class errors: failures of the program's own tree or parser
	// specification.
	switch {
	case errors.Is(e, ErrInvalidCommand):
		return ErrorDefinition
	case errors.Is(e, ErrInvalidName):
		return ErrorDefinition
	case errors.Is(e, ErrNameConflict):
		return ErrorDefinition
	case errors.Is(e, ErrAlreadyAttached):
		return ErrorDefinition
	case errors.Is(e, ErrInvalidSpec):
		return ErrorDefinition
	}

	return ErrorFailure
}

// unwrapCauses returns the direct causes of e: one for an Unwrap() error
// wrapper, several for an Unwrap() []error multi-cause error as produced
// by errors.Join and fmt.Errorf with multiple %w verbs.
func unwrapCauses(e error) []error {
	if multi, ok := e.(interface{ Unwrap() []error }); ok {
		return multi.Unwrap()
	}
	if single, ok := e.(interface{ Unwrap() error }); ok {
		if cause := single.Unwrap(); cause != nil {
			return []error{cause}
		}
	}
	return nil
}

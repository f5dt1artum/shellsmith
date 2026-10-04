package command

import (
	"context"
	"errors"
)

// This file adds a uniform mapping from process errors to stable classes
// and process exit codes. Classification is a pure function of the error:
// it only inspects err through errors.Is and errors.As, never runs a
// handler, writes output, reads the environment, configuration files, or
// any other process state, never mutates err (including the Path, Token,
// Value and Source fields of the package's error types), and is safe for
// concurrent use.

// ErrorClass is the stable classification of a process error. Its String
// values are part of the public API and never change; the integer values
// are not.
type ErrorClass int

const (
	// ClassNone means no error; ClassifyError(nil) returns it.
	ClassNone ErrorClass = iota
	// ClassUsage means the command line was invoked incorrectly: an
	// unknown command, a missing command, command-line parse failures,
	// or an unsupported completion shell.
	ClassUsage
	// ClassConfiguration means configuration (configuration files or
	// environment-supplied values) is invalid or unavailable.
	ClassConfiguration
	// ClassDefinition means the program's own command tree or parser
	// specification is invalid.
	ClassDefinition
	// ClassCanceled means the operation was canceled through its
	// context.
	ClassCanceled
	// ClassTimeout means the operation exceeded its context deadline.
	ClassTimeout
	// ClassFailure is the class of every other non-nil error.
	ClassFailure
)

// String returns the stable textual value of the class: "none", "usage",
// "configuration", "definition", "canceled", "timeout" or "failure".
func (c ErrorClass) String() string {
	switch c {
	case ClassNone:
		return "none"
	case ClassUsage:
		return "usage"
	case ClassConfiguration:
		return "configuration"
	case ClassDefinition:
		return "definition"
	case ClassCanceled:
		return "canceled"
	case ClassTimeout:
		return "timeout"
	default:
		return "failure"
	}
}

// ExitCode returns the conventional process exit code for c: 0 for
// ClassNone, 2 for ClassUsage, 78 for ClassConfiguration (EX_CONFIG), 70
// for ClassDefinition (EX_SOFTWARE), 130 for ClassCanceled, 124 for
// ClassTimeout, and 1 for ClassFailure and any out-of-range value.
func (c ErrorClass) ExitCode() int {
	switch c {
	case ClassNone:
		return 0
	case ClassUsage:
		return 2
	case ClassConfiguration:
		return 78
	case ClassDefinition:
		return 70
	case ClassCanceled:
		return 130
	case ClassTimeout:
		return 124
	default:
		return 1
	}
}

// ClassifyError maps err to a stable ErrorClass, seeing through wrapping
// (fmt.Errorf("%w")) and through every cause joined by errors.Join via
// errors.Is and errors.As. The error text never affects the result. A nil
// error is ClassNone; an unknown non-nil error is ClassFailure.
//
// Direct rules:
//
//   - context.Canceled is ClassCanceled; context.DeadlineExceeded is
//     ClassTimeout.
//   - *ConfigError, and a *ParseError wrapping ErrInvalidValue whose
//     Source is SourceEnvironment or SourceConfig, are
//     ClassConfiguration.
//   - *LookupError, ErrCommandRequired, *ShellError, command-line
//     *ParseError values, and every remaining *ParseError are
//     ClassUsage.
//   - *NameError, *ConflictError, *AttachedError, *SpecError and
//     ErrInvalidCommand are ClassDefinition.
//
// When more than one cause is reachable (multi-layer wrapping or
// errors.Join), the highest-priority class wins in the fixed order
// canceled, timeout, configuration, usage, definition, failure. The
// error and the fields of any typed error it contains are only read,
// never modified.
func ClassifyError(err error) ErrorClass {
	if err == nil {
		return ClassNone
	}
	return highestClass(err)
}

// ExitCode returns the process exit code assigned to err:
// ClassifyError(err).ExitCode(). It returns 0 for nil and 1 for an
// unknown non-nil error.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	return highestClass(err).ExitCode()
}

// Class precedence ranks, highest first: canceled, timeout,
// configuration, usage, definition, failure. Every non-nil error matches
// at least failureRank.
const (
	failureRank = iota
	definitionRank
	usageRank
	configurationRank
	timeoutRank
	canceledRank
)

// highestClass walks the whole error graph reachable from err — Unwrap
// chains and Unwrap() []error groups as produced by errors.Join — and
// returns the class of the highest-ranking node. The walk mirrors the
// traversal in errors.Is/errors.As so joined causes are all considered.
func highestClass(err error) ErrorClass {
	best := failureRank

	var walk func(error)
	walk = func(err error) {
		if err == nil {
			return
		}
		if rank := directRank(err); rank > best {
			best = rank
		}
		// Unwrap chains and joined/sliced causes. The slice form is
		// checked first, matching the traversal in errors.Is and
		// errors.As, so errors.Join results are classified by their
		// highest-priority member.
		if u, ok := err.(interface{ Unwrap() []error }); ok {
			for _, cause := range u.Unwrap() {
				walk(cause)
			}
			return
		}
		if u, ok := err.(interface{ Unwrap() error }); ok {
			walk(u.Unwrap())
		}
	}
	walk(err)

	switch best {
	case canceledRank:
		return ClassCanceled
	case timeoutRank:
		return ClassTimeout
	case configurationRank:
		return ClassConfiguration
	case usageRank:
		return ClassUsage
	case definitionRank:
		return ClassDefinition
	default:
		return ClassFailure
	}
}

// directRank classifies err itself, without following its Unwrap chain;
// the caller walks the graph. errors.Is and errors.As are still used so a
// node's own Is/As methods are honored exactly as the standard library
// does.
func directRank(err error) int {
	// Context errors are matched first: they are comparable sentinel
	// values, and canceled/timeout outrank every other class.
	switch {
	case errors.Is(err, context.Canceled):
		return canceledRank
	case errors.Is(err, context.DeadlineExceeded):
		return timeoutRank
	}

	// ParseError is the one type whose class depends on its fields: an
	// invalid value from the environment or config is a configuration
	// failure; every other parse error (including command-line invalid
	// values, which carry SourceNone) is a command-line usage failure.
	var pe *ParseError
	if errors.As(err, &pe) {
		if errors.Is(err, ErrInvalidValue) &&
			(pe.Source == SourceEnvironment || pe.Source == SourceConfig) {
			return configurationRank
		}
		return usageRank
	}

	var ce *ConfigError
	if errors.As(err, &ce) {
		return configurationRank
	}

	switch {
	case errors.As(err, new(*LookupError)):
		return usageRank
	case errors.Is(err, ErrCommandRequired):
		return usageRank
	case errors.As(err, new(*ShellError)):
		return usageRank
	}

	switch {
	case errors.As(err, new(*NameError)):
		return definitionRank
	case errors.As(err, new(*ConflictError)):
		return definitionRank
	case errors.As(err, new(*AttachedError)):
		return definitionRank
	case errors.As(err, new(*SpecError)):
		return definitionRank
	case errors.Is(err, ErrInvalidCommand):
		return definitionRank
	}

	return failureRank
}

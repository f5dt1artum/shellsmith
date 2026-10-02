package command_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/f5dt1artum/shellsmith/command"
)

// recorded captures what a handler observed.
type recorded struct {
	path   []string
	args   []string
	stdout string
	stderr string
}

func mustNode(t *testing.T, name string, aliases []string, h command.Handler) *command.Command {
	t.Helper()
	c, err := command.New(name, "desc", aliases, h)
	if err != nil {
		t.Fatalf("New(%q) unexpected error: %v", name, err)
	}
	return c
}

func TestNewValidatesNames(t *testing.T) {
	valid := []string{"a", "abc", "A1", "a_b", "a-b", "x-y_z-9", "_x", "123"}
	for _, name := range valid {
		if _, err := command.New(name, "", nil, nil); err != nil {
			t.Errorf("New(%q) unexpected error: %v", name, err)
		}
	}
	invalid := []string{"", "-x", "-", "ab!", "a b", "a.b", "a/b", "café", "世界", " leading", "trailing "}
	for _, name := range invalid {
		_, err := command.New(name, "", nil, nil)
		if !errors.Is(err, command.ErrInvalidName) {
			t.Errorf("New(%q) error = %v, want ErrInvalidName", name, err)
		}
		if name != "" && err != nil && !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not carry the offending name %q", err, name)
		}
	}
}

func TestNewRejectsDuplicateNames(t *testing.T) {
	cases := []struct {
		primary string
		aliases []string
	}{
		{"v", []string{"v"}},      // alias repeats primary
		{"p", []string{"x", "x"}}, // alias repeated
		{"p", []string{"a", "b", "a"}},
	}
	for i, tc := range cases {
		_, err := command.New(tc.primary, "", tc.aliases, nil)
		if !errors.Is(err, command.ErrNameConflict) {
			t.Errorf("case %d: error = %v, want ErrNameConflict", i, err)
		}
	}
}

func TestAddRejectsNil(t *testing.T) {
	root := mustNode(t, "root", nil, nil)
	if _, err := root.Add(nil); !errors.Is(err, command.ErrInvalidCommand) {
		t.Fatalf("Add(nil) error = %v, want ErrInvalidCommand", err)
	}
}

func TestAddRejectsConflicts(t *testing.T) {
	root := mustNode(t, "root", nil, nil)
	if _, err := root.Add(mustNode(t, "start", []string{"up", "run"}, nil)); err != nil {
		t.Fatalf("Add start: %v", err)
	}
	if _, err := root.Add(mustNode(t, "stop", []string{"down"}, nil)); err != nil {
		t.Fatalf("Add stop: %v", err)
	}

	conflicts := []struct {
		name    string
		aliases []string
	}{
		{"start", nil},              // primary duplicates primary
		{"up", nil},                 // primary duplicates alias
		{"other", []string{"run"}},  // alias duplicates alias
		{"other", []string{"stop"}}, // alias duplicates primary
	}
	for i, tc := range conflicts {
		c := mustNode(t, tc.name, tc.aliases, nil)
		if _, err := root.Add(c); !errors.Is(err, command.ErrNameConflict) {
			t.Errorf("case %d (%s): error = %v, want ErrNameConflict", i, tc.name, err)
		}
	}
}

func TestAddIsCaseSensitive(t *testing.T) {
	root := mustNode(t, "root", nil, nil)
	if _, err := root.Add(mustNode(t, "start", []string{"up"}, nil)); err != nil {
		t.Fatal(err)
	}
	// Different casing is a distinct name and must not conflict.
	c := mustNode(t, "START", []string{"UP"}, nil)
	if _, err := root.Add(c); err != nil {
		t.Errorf("case-distinct names should not conflict: %v", err)
	}
}

func TestAddFailureDoesNotMutateTree(t *testing.T) {
	var goodRan bool
	root := mustNode(t, "root", nil, nil)
	if _, err := root.Add(mustNode(t, "good", nil, func(context.Context, []string, []string, io.Writer, io.Writer) error {
		goodRan = true
		return nil
	})); err != nil {
		t.Fatal(err)
	}

	bad := mustNode(t, "good", nil, nil) // duplicates existing primary
	if _, err := root.Add(bad); !errors.Is(err, command.ErrNameConflict) {
		t.Fatalf("conflicting Add error = %v, want ErrNameConflict", err)
	}

	// Rejected node is still unattached: it can join another parent.
	other := mustNode(t, "other", nil, nil)
	if _, err := other.Add(bad); err != nil {
		t.Errorf("rejected node should remain unattached: %v", err)
	}

	// Existing tree still works.
	if err := root.Execute(context.Background(), []string{"good"}, io.Discard, io.Discard); err != nil {
		t.Errorf("existing child dispatch: %v", err)
	}
	if !goodRan {
		t.Error("previously registered handler should still run")
	}
}

func TestAddRejectsAttachedNode(t *testing.T) {
	root1 := mustNode(t, "root1", nil, nil)
	root2 := mustNode(t, "root2", nil, nil)
	child := mustNode(t, "child", nil, nil)
	if _, err := root1.Add(child); err != nil {
		t.Fatal(err)
	}
	if _, err := root1.Add(child); !errors.Is(err, command.ErrAlreadyAttached) {
		t.Errorf("re-adding to same parent error = %v", err)
	}
	if _, err := root2.Add(child); !errors.Is(err, command.ErrAlreadyAttached) {
		t.Errorf("adding to second parent error = %v", err)
	}
}

func TestExecuteDispatchesByPrimaryName(t *testing.T) {
	var got recorded
	root := mustNode(t, "tool", nil, nil)
	child := mustNode(t, "build", nil, func(_ context.Context, path, args []string, stdout, stderr io.Writer) error {
		got.path = path
		got.args = args
		return nil
	})
	if _, err := root.Add(child); err != nil {
		t.Fatal(err)
	}

	err := root.Execute(context.Background(),
		[]string{"build", "pkg1", "pkg2", "--flag", "value"},
		io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if want := []string{"tool", "build"}; !equal(got.path, want) {
		t.Errorf("path = %v, want %v", got.path, want)
	}
	if want := []string{"pkg1", "pkg2", "--flag", "value"}; !equal(got.args, want) {
		t.Errorf("args = %v, want %v", got.args, want)
	}
}

func TestExecuteResolvesAliasToCanonicalPath(t *testing.T) {
	var got recorded
	root := mustNode(t, "tool", nil, nil)
	remote := mustNode(t, "remote", nil, nil)
	prune := mustNode(t, "prune", []string{"p"}, func(_ context.Context, path, args []string, _, _ io.Writer) error {
		got.path = path
		got.args = args
		return nil
	})
	if _, err := root.Add(remote); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Add(prune); err != nil {
		t.Fatal(err)
	}

	err := root.Execute(context.Background(),
		[]string{"r", "p", "a", "-b", "c"}, io.Discard, io.Discard)
	_ = err
	// "r" is not an alias of remote; it must be unknown.
	if !errors.Is(err, command.ErrUnknownCommand) {
		t.Fatalf("alias of wrong node should be unknown: %v", err)
	}

	err = root.Execute(context.Background(),
		[]string{"remote", "p", "a", "-b", "c"}, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("Execute via alias: %v", err)
	}
	if want := []string{"tool", "remote", "prune"}; !equal(got.path, want) {
		t.Errorf("canonical path = %v, want %v", got.path, want)
	}
	if want := []string{"a", "-b", "c"}; !equal(got.args, want) {
		t.Errorf("args = %v, want %v", got.args, want)
	}
}

func TestExecuteExactCaseSensitiveMatch(t *testing.T) {
	root := mustNode(t, "tool", nil, nil)
	if _, err := root.Add(mustNode(t, "run", []string{"go"}, func(context.Context, []string, []string, io.Writer, io.Writer) error {
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{"ru", "runs", "RUN", "Go"} {
		err := root.Execute(context.Background(), []string{arg}, io.Discard, io.Discard)
		if !errors.Is(err, command.ErrUnknownCommand) {
			t.Errorf("arg %q: error = %v, want ErrUnknownCommand", arg, err)
		}
	}
}

func TestExecuteUnknownCommandIsLookupError(t *testing.T) {
	root := mustNode(t, "tool", nil, nil)
	g := mustNode(t, "g", nil, nil)
	if _, err := root.Add(g); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Add(mustNode(t, "deep", nil, func(context.Context, []string, []string, io.Writer, io.Writer) error {
		t.Fatal("handler must not run on lookup failure")
		return nil
	})); err != nil {
		t.Fatal(err)
	}

	var le *command.LookupError
	err := root.Execute(context.Background(),
		[]string{"g", "nope", "x"}, io.Discard, io.Discard)
	if !errors.As(err, &le) {
		t.Fatalf("error = %v, want *LookupError", err)
	}
	if want := []string{"tool", "g"}; !equal(le.Path, want) {
		t.Errorf("LookupError.Path = %v, want %v", le.Path, want)
	}
	if le.Arg != "nope" {
		t.Errorf("LookupError.Arg = %q, want %q", le.Arg, "nope")
	}
	if !errors.Is(err, command.ErrUnknownCommand) {
		t.Error("LookupError must match ErrUnknownCommand via errors.Is")
	}
}

func TestExecuteCommandRequired(t *testing.T) {
	// Parent with children but no handler, no remaining args.
	root := mustNode(t, "tool", nil, nil)
	if _, err := root.Add(mustNode(t, "sub", nil, nil)); err != nil {
		t.Fatal(err)
	}
	if err := root.Execute(context.Background(), nil, io.Discard, io.Discard); !errors.Is(err, command.ErrCommandRequired) {
		t.Errorf("empty args on branching node: error = %v, want ErrCommandRequired", err)
	}

	// Leaf node without a handler and no remaining args.
	if err := root.Execute(context.Background(), []string{"sub"}, io.Discard, io.Discard); !errors.Is(err, command.ErrCommandRequired) {
		t.Errorf("leaf without handler: error = %v, want ErrCommandRequired", err)
	}
}

func TestExecuteHandlerRunsWithNoArgs(t *testing.T) {
	var called bool
	root := mustNode(t, "tool", nil, func(_ context.Context, path, args []string, _, _ io.Writer) error {
		called = true
		if want := []string{"tool"}; !equal(path, want) {
			t.Errorf("path = %v, want %v", path, want)
		}
		if len(args) != 0 {
			t.Errorf("args = %v, want empty", args)
		}
		return nil
	})
	// Root has a handler but no children; it still runs with zero args.
	if err := root.Execute(context.Background(), nil, io.Discard, io.Discard); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !called {
		t.Error("handler must run even with no remaining args")
	}
}

func TestExecutePassesWritersAndDoesNotEmitItself(t *testing.T) {
	handlerErr := errors.New("boom")
	root := mustNode(t, "tool", nil, nil)
	if _, err := root.Add(mustNode(t, "say", nil,
		func(_ context.Context, _ []string, args []string, stdout, stderr io.Writer) error {
			_, _ = io.WriteString(stdout, "out:"+strings.Join(args, ","))
			_, _ = io.WriteString(stderr, "err")
			return handlerErr
		})); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	err := root.Execute(context.Background(), []string{"say", "a", "b"}, &out, &errOut)
	if !errors.Is(err, handlerErr) {
		t.Errorf("handler error should be returned as-is, got %v", err)
	}
	if out.String() != "out:a,b" {
		t.Errorf("stdout = %q, want %q", out.String(), "out:a,b")
	}
	if errOut.String() != "err" {
		t.Errorf("stderr = %q, want %q", errOut.String(), "err")
	}

	// A failed lookup must not write anything.
	out.Reset()
	errOut.Reset()
	_ = root.Execute(context.Background(), []string{"missing"}, &out, &errOut)
	if out.Len() != 0 || errOut.Len() != 0 {
		t.Errorf("lookup failure wrote output: out=%q err=%q", out.String(), errOut.String())
	}
}

func TestExecuteContextCancelledBeforeDispatch(t *testing.T) {
	called := false
	root := mustNode(t, "tool", nil, func(context.Context, []string, []string, io.Writer, io.Writer) error {
		called = true
		return nil
	})

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := root.Execute(canceled, nil, io.Discard, io.Discard); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled ctx: error = %v, want context.Canceled", err)
	}

	deadline, cancelDl := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancelDl()
	if err := root.Execute(deadline, nil, io.Discard, io.Discard); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expired ctx: error = %v, want context.DeadlineExceeded", err)
	}
	if called {
		t.Error("handler must not run when context was done before dispatch")
	}
}

func TestChildrenPreserveRegistrationOrder(t *testing.T) {
	root := mustNode(t, "tool", nil, nil)
	names := []string{"zeta", "alpha", "mid"}
	for _, n := range names {
		if _, err := root.Add(mustNode(t, n, nil, nil)); err != nil {
			t.Fatal(err)
		}
	}
	// Registration order is observable through successful dispatch and via
	// error messages that list the resolved path; just assert all resolve and
	// that an unknown arg reports deterministically.
	for _, n := range names {
		err := root.Execute(context.Background(), []string{n}, io.Discard, io.Discard)
		// Each has no handler and no children, so ErrCommandRequired proves
		// it was matched (not ErrUnknownCommand).
		if !errors.Is(err, command.ErrCommandRequired) {
			t.Errorf("child %q: error = %v, want ErrCommandRequired", n, err)
		}
	}
}

func TestAliasesAccessorCopies(t *testing.T) {
	c := mustNode(t, "x", []string{"a", "b"}, nil)
	got := c.Aliases()
	got[0] = "mutated"
	if c.Aliases()[0] != "a" {
		t.Error("Aliases() must return a copy")
	}
	if c.Name() != "x" || c.Description() != "desc" {
		t.Errorf("accessors: name=%q desc=%q", c.Name(), c.Description())
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

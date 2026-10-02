package command

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"
)

// noop is a handler that records nothing and succeeds.
func noop(path []string, args []string, stdout, stderr io.Writer) error { return nil }

func TestValidNamesAccepted(t *testing.T) {
	root := New("tool", "root", nil, noop)
	cases := []struct {
		name    string
		aliases []string
	}{
		{"a", nil},
		{"A1-_", []string{"x", "Y", "0", "_ok", "a-b_c9"}},
		{"serve", []string{"srv", "S"}},
		{"_", []string{"9", "a_B-C"}},
	}
	for _, c := range cases {
		if err := root.Add(New(c.name, "", c.aliases, noop)); err != nil {
			t.Errorf("Add(%q, %v) unexpected error: %v", c.name, c.aliases, err)
		}
	}
}

func TestInvalidPrimaryNamesRejected(t *testing.T) {
	bad := []string{
		"",      // empty
		"-x",    // leading hyphen
		"-",     // only a hyphen
		"a b",   // space
		"a.b",   // punctuation
		"a/b",   // slash
		"日本",    // non-ASCII
		"café",  // non-ASCII byte
		"a\nb",  // control character
		"a--b ", // trailing space
	}
	for _, name := range bad {
		root := New("tool", "", nil, noop)
		err := root.Add(New(name, "", nil, noop))
		if !errors.Is(err, ErrInvalidName) {
			t.Errorf("Add(%q) error = %v, want errors.Is ErrInvalidName", name, err)
		}
		var ne *NameError
		if !errors.As(err, &ne) || ne.Name != name {
			t.Errorf("Add(%q) error does not carry *NameError{Name: %q}: %v", name, name, err)
		}
	}
}

func TestInvalidAliasRejected(t *testing.T) {
	root := New("tool", "", nil, noop)
	err := root.Add(New("good", "", []string{"-bad"}, noop))
	if !errors.Is(err, ErrInvalidName) {
		t.Fatalf("error = %v, want ErrInvalidName", err)
	}
	var ne *NameError
	if !errors.As(err, &ne) || ne.Name != "-bad" {
		t.Fatalf("error must carry offending alias %q, got %v", "-bad", err)
	}
}

func TestNilChildRejected(t *testing.T) {
	root := New("tool", "", nil, noop)
	if err := root.Add(nil); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("Add(nil) error = %v, want ErrInvalidCommand", err)
	}
}

func TestDuplicateNamesInsideOneNodeConflict(t *testing.T) {
	root := New("tool", "", nil, noop)
	if err := root.Add(New("dup", "", []string{"dup"}, noop)); !errors.Is(err, ErrNameConflict) {
		t.Fatalf("alias equal to primary name: error = %v, want ErrNameConflict", err)
	}
	err := root.Add(New("node", "", []string{"a", "a"}, noop))
	if !errors.Is(err, ErrNameConflict) {
		t.Fatalf("repeated alias: error = %v, want ErrNameConflict", err)
	}
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Name != "a" {
		t.Fatalf("conflict must carry name %q, got %v", "a", err)
	}
}

func TestSiblingNameConflicts(t *testing.T) {
	root := New("tool", "", nil, noop)
	if err := root.Add(New("serve", "", []string{"s"}, noop)); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		label   string
		name    string
		aliases []string
	}{
		{"primary repeats primary", "serve", nil},
		{"primary repeats alias", "s", nil},
		{"alias repeats primary", "other", []string{"serve"}},
		{"alias repeats alias", "other", []string{"s"}},
		{"case-sensitive distinct is allowed", "SERVE", []string{"S"}},
	}
	for i, c := range cases {
		err := root.Add(New(c.name, "", c.aliases, noop))
		if i == len(cases)-1 {
			if err != nil {
				t.Fatalf("%s: unexpected error %v", c.label, err)
			}
			continue
		}
		if !errors.Is(err, ErrNameConflict) {
			t.Errorf("%s: error = %v, want ErrNameConflict", c.label, err)
		}
	}
}

func TestAlreadyAttached(t *testing.T) {
	root := New("tool", "", nil, noop)
	other := New("other", "", nil, noop)
	child := New("child", "", nil, noop)
	if err := root.Add(child); err != nil {
		t.Fatal(err)
	}
	err := other.Add(child)
	if !errors.Is(err, ErrAlreadyAttached) {
		t.Fatalf("error = %v, want ErrAlreadyAttached", err)
	}
	var ae *AttachedError
	if !errors.As(err, &ae) || ae.Name != "child" {
		t.Fatalf("error must carry node name %q, got %v", "child", err)
	}
}

func TestCannotAttachIntoOwnSubtree(t *testing.T) {
	root := New("tool", "", nil, noop)
	if err := root.Add(root); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("root.Add(root) error = %v, want ErrInvalidCommand", err)
	}
}

func TestFailedAddLeavesTreeUntouched(t *testing.T) {
	root := New("tool", "", nil, noop)
	if err := root.Add(New("keep", "", nil, noop)); err != nil {
		t.Fatal(err)
	}
	before := len(root.children)
	for _, child := range []*Node{
		nil,
		New("keep", "", nil, noop), // conflicts
		New("bad!", "", nil, noop), // invalid
		New("x", "", []string{"x"}, noop),
	} {
		if err := root.Add(child); err == nil {
			t.Fatalf("Add(%v) unexpectedly succeeded", child)
		}
	}
	if len(root.children) != before {
		t.Fatalf("children count changed from %d to %d after failed adds", before, len(root.children))
	}
	// A valid add after failures still works, proving no partial mutation.
	if err := root.Add(New("after", "", nil, noop)); err != nil {
		t.Fatalf("valid Add after failures: %v", err)
	}
}

func TestRegistrationOrderPreserved(t *testing.T) {
	root := New("tool", "", nil, noop)
	names := []string{"zebra", "apple", "mango", "banana"}
	for _, n := range names {
		if err := root.Add(New(n, "", nil, noop)); err != nil {
			t.Fatal(err)
		}
	}
	got := make([]string, len(root.children))
	for i, c := range root.children {
		got[i] = c.name
	}
	if !reflect.DeepEqual(got, names) {
		t.Fatalf("children order = %v, want %v", got, names)
	}
}

func TestAliasesCopied(t *testing.T) {
	src := []string{"a", "b"}
	n := New("n", "desc", src, nil)
	src[0] = "mutated"
	if got := n.Aliases(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("constructor did not copy aliases: %v", got)
	}
	out := n.Aliases()
	out[0] = "mutated"
	if got := n.Aliases(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("Aliases returned shared slice: %v", got)
	}
	if n.Name() != "n" || n.Description() != "desc" {
		t.Fatalf("accessors returned %q, %q", n.Name(), n.Description())
	}
}

type call struct {
	path   []string
	args   []string
	stdout string
	stderr string
}

func recordingHandler(c *call) HandlerFunc {
	return func(path []string, args []string, stdout, stderr io.Writer) error {
		c.path = append([]string(nil), path...)
		c.args = append([]string(nil), args...)
		return nil
	}
}

func buildTree(t *testing.T, leafCall, aliasCall *call) *Node {
	t.Helper()
	root := New("tool", "root command", nil, noop)
	remote := New("remote", "manage remotes", []string{"r"}, nil)
	if err := root.Add(remote); err != nil {
		t.Fatal(err)
	}
	if err := remote.Add(New("add", "add remote", []string{"a"}, recordingHandler(aliasCall))); err != nil {
		t.Fatal(err)
	}
	serve := New("serve", "serve over http", []string{"srv", "go"}, recordingHandler(leafCall))
	if err := root.Add(serve); err != nil {
		t.Fatal(err)
	}
	leaf := New("status", "show status", nil, recordingHandler(leafCall))
	if err := root.Add(leaf); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestExecuteLeafGetsRemainingArgs(t *testing.T) {
	var got call
	root := buildTree(t, &got, nil)
	var stdout, stderr bytes.Buffer
	err := root.Execute(context.Background(),
		[]string{"serve", "--port", "9090", "extra"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"tool", "serve"}; !reflect.DeepEqual(got.path, want) {
		t.Fatalf("path = %v, want %v", got.path, want)
	}
	if want := []string{"--port", "9090", "extra"}; !reflect.DeepEqual(got.args, want) {
		t.Fatalf("args = %v, want %v", got.args, want)
	}
}

func TestExecuteAliasReportsCanonicalPath(t *testing.T) {
	var remoteAdd call
	root := buildTree(t, nil, &remoteAdd)
	err := root.Execute(context.Background(),
		[]string{"r", "a", "origin", "http://x"}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"tool", "remote", "add"}; !reflect.DeepEqual(remoteAdd.path, want) {
		t.Fatalf("canonical path = %v, want %v", remoteAdd.path, want)
	}
	if want := []string{"origin", "http://x"}; !reflect.DeepEqual(remoteAdd.args, want) {
		t.Fatalf("args = %v, want %v", remoteAdd.args, want)
	}
}

func TestExecuteAliasOnLeaf(t *testing.T) {
	var got call
	root := buildTree(t, &got, nil)
	err := root.Execute(context.Background(), []string{"go", "-v"}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"tool", "serve"}; !reflect.DeepEqual(got.path, want) {
		t.Fatalf("path = %v, want %v", got.path, want)
	}
	if want := []string{"-v"}; !reflect.DeepEqual(got.args, want) {
		t.Fatalf("args = %v, want %v", got.args, want)
	}
}

func TestExactAndCaseSensitiveOnly(t *testing.T) {
	var got call
	root := buildTree(t, &got, nil)

	// Prefixes must not match.
	err := root.Execute(context.Background(), []string{"ser"}, io.Discard, io.Discard)
	if !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("prefix match: error = %v, want ErrUnknownCommand", err)
	}
	var le *LookupError
	if !errors.As(err, &le) {
		t.Fatalf("expected *LookupError, got %v", err)
	}
	if want := []string{"tool"}; !reflect.DeepEqual(le.Path, want) || le.Arg != "ser" {
		t.Fatalf("LookupError = %+v, want Path %v Arg %q", le, want, "ser")
	}

	// Wrong case must not match.
	if err := root.Execute(context.Background(), []string{"Serve"}, io.Discard, io.Discard); !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("case-insensitive match succeeded: %v", err)
	}
	if got.path != nil {
		t.Fatal("handler must not be called on unknown command")
	}
}

func TestUnknownCommandCarriesPathAndArg(t *testing.T) {
	root := buildTree(t, nil, nil)
	err := root.Execute(context.Background(),
		[]string{"remote", "remove", "x"}, io.Discard, io.Discard)
	if !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("error = %v, want ErrUnknownCommand", err)
	}
	var le *LookupError
	if !errors.As(err, &le) {
		t.Fatalf("expected *LookupError, got %T", err)
	}
	if want := []string{"tool", "remote"}; !reflect.DeepEqual(le.Path, want) {
		t.Fatalf("Path = %v, want %v", le.Path, want)
	}
	if le.Arg != "remove" {
		t.Fatalf("Arg = %q, want %q", le.Arg, "remove")
	}
}

func TestLookupErrorWritesNothing(t *testing.T) {
	root := buildTree(t, nil, nil)
	var stdout, stderr bytes.Buffer
	_ = root.Execute(context.Background(), []string{"nope"}, &stdout, &stderr)
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("framework wrote output on lookup failure: %q / %q", stdout.String(), stderr.String())
	}
}

func TestArgsExhausted(t *testing.T) {
	// Parent without handler and children present.
	root := New("tool", "", nil, nil)
	remote := New("remote", "", nil, nil)
	if err := root.Add(remote); err != nil {
		t.Fatal(err)
	}
	if err := remote.Add(New("add", "", nil, noop)); err != nil {
		t.Fatal(err)
	}
	err := root.Execute(context.Background(), nil, io.Discard, io.Discard)
	if !errors.Is(err, ErrCommandRequired) {
		t.Fatalf("root no args: error = %v, want ErrCommandRequired", err)
	}
	err = root.Execute(context.Background(), []string{"remote"}, io.Discard, io.Discard)
	if !errors.Is(err, ErrCommandRequired) {
		t.Fatalf("branch no args: error = %v, want ErrCommandRequired", err)
	}

	// Handler present: called even with zero remaining arguments.
	var got call
	h := New("h", "", nil, recordingHandler(&got))
	if err := h.Execute(context.Background(), nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if want := []string{"h"}; !reflect.DeepEqual(got.path, want) {
		t.Fatalf("path = %v, want %v", got.path, want)
	}
	if len(got.args) != 0 {
		t.Fatalf("args = %v, want empty", got.args)
	}

	// Branch node carrying both children and a handler: no args runs it.
	var rootCall call
	r2 := New("r2", "", nil, recordingHandler(&rootCall))
	if err := r2.Add(New("child", "", nil, noop)); err != nil {
		t.Fatal(err)
	}
	if err := r2.Execute(context.Background(), nil, io.Discard, io.Discard); err != nil {
		t.Fatalf("branch with handler, no args: %v", err)
	}
	if want := []string{"r2"}; !reflect.DeepEqual(rootCall.path, want) || len(rootCall.args) != 0 {
		t.Fatalf("branch handler got path %v args %v", rootCall.path, rootCall.args)
	}
}

func TestHandlerErrorReturnedUnchanged(t *testing.T) {
	sentinel := errors.New("boom")
	n := New("n", "", nil, func(path []string, args []string, stdout, stderr io.Writer) error {
		return sentinel
	})
	if err := n.Execute(context.Background(), nil, io.Discard, io.Discard); !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want handler error unchanged", err)
	}
}

func TestWritersPassedToHandler(t *testing.T) {
	n := New("n", "", nil, func(path []string, args []string, stdout, stderr io.Writer) error {
		_, _ = io.WriteString(stdout, "out")
		_, _ = io.WriteString(stderr, "err")
		return nil
	})
	var stdout, stderr bytes.Buffer
	if err := n.Execute(context.Background(), []string{"x"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "out" || stderr.String() != "err" {
		t.Fatalf("writers not passed through: %q %q", stdout.String(), stderr.String())
	}
}

func TestContextCanceledBeforeParse(t *testing.T) {
	called := false
	n := New("n", "", nil, func(path []string, args []string, stdout, stderr io.Writer) error {
		called = true
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := n.Execute(ctx, nil, io.Discard, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ctx: error = %v, want context.Canceled", err)
	}
	ctx2, c2 := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer c2()
	if err := n.Execute(ctx2, nil, io.Discard, io.Discard); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired ctx: error = %v, want context.DeadlineExceeded", err)
	}
	if called {
		t.Fatal("handler must not run for a pre-canceled context")
	}
}

func TestDeeplyNestedTree(t *testing.T) {
	var got call
	depth := 8
	root := New("lvl0", "", nil, nil)
	cur := root
	for i := 1; i < depth; i++ {
		next := New("lvl"+digit(i), "", []string{"a" + digit(i)}, nil)
		if err := cur.Add(next); err != nil {
			t.Fatal(err)
		}
		cur = next
	}
	cur.handler = recordingHandler(&got)

	argv := []string{"a1", "lvl2", "a3", "lvl4", "a5", "lvl6", "a7", "tail1", "tail2"}
	if err := root.Execute(context.Background(), argv, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	var wantPath []string
	for i := 0; i < depth; i++ {
		wantPath = append(wantPath, "lvl"+digit(i))
	}
	if !reflect.DeepEqual(got.path, wantPath) {
		t.Fatalf("path = %v, want %v", got.path, wantPath)
	}
	if want := []string{"tail1", "tail2"}; !reflect.DeepEqual(got.args, want) {
		t.Fatalf("args = %v, want %v", got.args, want)
	}
}

func digit(i int) string {
	return string(rune('0' + i))
}

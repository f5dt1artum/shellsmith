package command

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// sampleHelpTree builds:
//
//	tool (handler, desc "the root")
//	├── serve (aliases srv, s; handler; desc "run a server")
//	│   └── stop (no aliases, no handler, desc "stop the server")
//	└── config (no aliases, no handler, no desc)
func sampleHelpTree(t *testing.T) *Node {
	t.Helper()
	root := New("tool", "the root", nil, noop)
	serve := New("serve", "run a server", []string{"srv", "s"}, noop)
	if err := serve.Add(New("stop", "stop the server", nil, nil)); err != nil {
		t.Fatalf("Add(stop): %v", err)
	}
	if err := root.Add(serve); err != nil {
		t.Fatalf("Add(serve): %v", err)
	}
	if err := root.Add(New("config", "", nil, nil)); err != nil {
		t.Fatalf("Add(config): %v", err)
	}
	return root
}

func renderHelp(t *testing.T, n *Node, path []string, p *Parser) string {
	t.Helper()
	var buf bytes.Buffer
	if err := n.WriteHelp(path, p, &buf); err != nil {
		t.Fatalf("WriteHelp(%v): %v", path, err)
	}
	return buf.String()
}

func TestWriteHelpBareLeaf(t *testing.T) {
	got := renderHelp(t, New("tool", "", nil, noop), nil, nil)
	if got != "Usage: tool\n" {
		t.Fatalf("bare leaf help = %q", got)
	}
}

func TestWriteHelpEmptyPathEqualsCurrentNode(t *testing.T) {
	root := sampleHelpTree(t)
	viaNil := renderHelp(t, root, nil, nil)
	viaEmpty := renderHelp(t, root, []string{}, nil)
	if viaNil != viaEmpty {
		t.Fatalf("nil and empty paths differ:\n%q\n%q", viaNil, viaEmpty)
	}
	if !strings.HasPrefix(viaNil, "Usage: tool ") {
		t.Fatalf("empty path did not select root: %q", viaNil)
	}
}

func TestWriteHelpCommandMarkers(t *testing.T) {
	root := sampleHelpTree(t)

	// Children and a handler: [command].
	if got := renderHelp(t, root, nil, nil); !strings.HasPrefix(got, "Usage: tool [command]\n") {
		t.Errorf("branch with handler usage = %q", got)
	}
	// Children without a handler: <command>.
	parent := New("group", "", nil, nil)
	if err := parent.Add(New("child", "", nil, noop)); err != nil {
		t.Fatal(err)
	}
	if got := renderHelp(t, parent, nil, nil); !strings.HasPrefix(got, "Usage: group <command>\n") {
		t.Errorf("branch without handler usage = %q", got)
	}
	// Leaf: neither marker.
	if got := renderHelp(t, root, []string{"serve", "stop"}, nil); got != "Usage: tool serve stop\n\nstop the server\n" {
		t.Errorf("leaf usage = %q", got)
	}
}

func TestWriteHelpCanonicalPaths(t *testing.T) {
	root := sampleHelpTree(t)
	cases := []struct {
		path []string
		want string
	}{
		{[]string{"s"}, "Usage: tool serve [command]\n"},
		{[]string{"srv"}, "Usage: tool serve [command]\n"},
		{[]string{"s", "stop"}, "Usage: tool serve stop\n"},
		{[]string{"srv", "stop"}, "Usage: tool serve stop\n"},
	}
	for _, c := range cases {
		if got := renderHelp(t, root, c.path, nil); !strings.HasPrefix(got, c.want) {
			t.Errorf("path %v rendered %q, want prefix %q", c.path, got, c.want)
		}
	}
}

func TestWriteHelpDescriptionLayout(t *testing.T) {
	root := sampleHelpTree(t)
	got := renderHelp(t, root, nil, nil)
	want := "Usage: tool [command]\n" +
		"\n" +
		"the root\n" +
		"\n" +
		"Commands:\n" +
		"  serve (srv, s)  run a server\n" +
		"  config\n"
	if got != want {
		t.Errorf("help =\n%q\nwant\n%q", got, want)
	}

	// Descriptions are emitted verbatim, including embedded newlines, and a
	// trailing description leaves no section separator behind it.
	multi := New("m", "first\nsecond", nil, noop)
	if got := renderHelp(t, multi, nil, nil); got != "Usage: m\n\nfirst\nsecond\n" {
		t.Errorf("multiline description = %q", got)
	}
}

func TestWriteHelpCommandsInAddOrder(t *testing.T) {
	root := New("tool", "", nil, noop)
	names := []string{"zebra", "alpha", "middle"}
	for _, name := range names {
		if err := root.Add(New(name, "d-"+name, nil, noop)); err != nil {
			t.Fatal(err)
		}
	}
	got := renderHelp(t, root, nil, nil)
	var lines []string
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "  ") {
			lines = append(lines, line)
		}
	}
	want := []string{"  zebra  d-zebra", "  alpha  d-alpha", "  middle  d-middle"}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("command lines = %v, want %v", lines, want)
	}
}

func TestWriteHelpAliasesInRegistrationOrder(t *testing.T) {
	root := New("tool", "", nil, noop)
	if err := root.Add(New("serve", "", []string{"b", "a", "c"}, noop)); err != nil {
		t.Fatal(err)
	}
	got := renderHelp(t, root, nil, nil)
	if !strings.Contains(got, "  serve (b, a, c)\n") {
		t.Errorf("aliases not in registration order: %q", got)
	}
}

func TestWriteHelpParserUsage(t *testing.T) {
	p, err := NewParser(
		[]Option{{Long: "verbose", Short: "v", Type: TypeBool}},
		[]Positional{{Name: "src"}, {Name: "dst"}, {Name: "rest", Variadic: true}},
	)
	if err != nil {
		t.Fatal(err)
	}
	leaf := New("cp", "", nil, noop)
	got := renderHelp(t, leaf, nil, p)
	want := "Usage: cp [options] <src> <dst> [<rest>...]\n" +
		"\n" +
		"Options:\n" +
		"  -v, --verbose\n"
	if got != want {
		t.Errorf("help =\n%q\nwant\n%q", got, want)
	}
}

func TestWriteHelpParserPositionalsOnly(t *testing.T) {
	p, err := NewParser(nil, []Positional{{Name: "file"}})
	if err != nil {
		t.Fatal(err)
	}
	got := renderHelp(t, New("cat", "", nil, noop), nil, p)
	if got != "Usage: cat <file>\n" {
		t.Errorf("help = %q, want usage line only", got)
	}
}

func TestWriteHelpEmptyParserChangesNothing(t *testing.T) {
	p, err := NewParser(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf := New("cat", "", nil, noop)
	if got, want := renderHelp(t, leaf, nil, p), "Usage: cat\n"; got != want {
		t.Errorf("empty parser help = %q, want %q", got, want)
	}
}

func TestWriteHelpOptionsSection(t *testing.T) {
	p, err := NewParser([]Option{
		{Long: "verbose", Short: "v", Type: TypeBool},
		{Long: "repeat", Short: "r", Type: TypeString, Repeatable: true},
		{Long: "name", Short: "n", Type: TypeString, Required: true},
		{Long: "port", Type: TypeInt, Default: "8080"},
		{Long: "timeout", Short: "t", Type: TypeDuration, Default: "5s", Required: true, Repeatable: true},
		{Long: "enabled", Type: TypeBool, Default: "true"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := renderHelp(t, New("svc", "", nil, noop), nil, p)
	want := "Usage: svc [options]\n" +
		"\n" +
		"Options:\n" +
		"  -v, --verbose\n" +
		"  -r, --repeat <string>  (repeatable)\n" +
		"  -n, --name <string>  (required)\n" +
		"  --port <int>  (default: 8080)\n" +
		"  -t, --timeout <duration>  (required, repeatable, default: 5s)\n" +
		"  --enabled  (default: true)\n"
	if got != want {
		t.Errorf("help =\n%q\nwant\n%q", got, want)
	}
}

func TestWriteHelpFullLayout(t *testing.T) {
	p, err := NewParser(
		[]Option{{Long: "level", Short: "l", Type: TypeInt, Default: "2"}},
		[]Positional{{Name: "target"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	root := sampleHelpTree(t)
	got := renderHelp(t, root, []string{"serve"}, p)
	want := "Usage: tool serve [command] [options] <target>\n" +
		"\n" +
		"run a server\n" +
		"\n" +
		"Commands:\n" +
		"  stop  stop the server\n" +
		"\n" +
		"Options:\n" +
		"  -l, --level <int>  (default: 2)\n"
	if got != want {
		t.Errorf("help =\n%q\nwant\n%q", got, want)
	}
}

func TestWriteHelpByteHygieneAndDeterminism(t *testing.T) {
	p, err := NewParser(
		[]Option{{Long: "verbose", Short: "v", Type: TypeBool}, {Long: "n", Type: TypeInt, Default: "3"}},
		[]Positional{{Name: "src"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	root := sampleHelpTree(t)
	first := renderHelp(t, root, nil, p)
	for i := 0; i < 6; i++ {
		if got := renderHelp(t, root, nil, p); got != first {
			t.Fatalf("render %d differs:\n%q\n%q", i, got, first)
		}
	}
	if !strings.HasSuffix(first, "\n") || strings.HasSuffix(first, "\n\n") {
		t.Errorf("must end with exactly one LF: %q", first)
	}
	if strings.ContainsAny(first, "\r\x1b") {
		t.Errorf("contains CR or ANSI escape: %q", first)
	}
	if strings.Contains(first, "\n\n\n") {
		t.Errorf("contains consecutive blank lines: %q", first)
	}
}

func TestWriteHelpUnknownSegment(t *testing.T) {
	root := sampleHelpTree(t)
	var buf bytes.Buffer
	err := root.WriteHelp([]string{"serve", "bogus", "later"}, nil, &buf)
	if !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("error = %v, want ErrUnknownCommand", err)
	}
	var le *LookupError
	if !errors.As(err, &le) {
		t.Fatalf("error is %T, want *LookupError", err)
	}
	if !reflect.DeepEqual(le.Path, []string{"tool", "serve"}) {
		t.Errorf("Path = %v, want [tool serve]", le.Path)
	}
	if le.Arg != "bogus" {
		t.Errorf("Arg = %q, want bogus", le.Arg)
	}
	if buf.Len() != 0 {
		t.Errorf("writer received %d bytes on failure", buf.Len())
	}
}

func TestWriteHelpUnknownSegmentAtRoot(t *testing.T) {
	root := sampleHelpTree(t)
	err := root.WriteHelp([]string{"Serve"}, nil, io.Discard)
	var le *LookupError
	if !errors.As(err, &le) {
		t.Fatalf("error is %T, want *LookupError", err)
	}
	if !reflect.DeepEqual(le.Path, []string{"tool"}) || le.Arg != "Serve" {
		t.Errorf("LookupError = Path %v Arg %q", le.Path, le.Arg)
	}
}

func TestWriteHelpDoesNotMutateInputs(t *testing.T) {
	root := sampleHelpTree(t)
	p, err := NewParser(
		[]Option{{Long: "verbose", Short: "v", Type: TypeBool}},
		[]Positional{{Name: "file"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	path := []string{"s", "stop"}
	pathCopy := append([]string(nil), path...)
	_ = renderHelp(t, root, path, p)
	if !reflect.DeepEqual(path, pathCopy) {
		t.Errorf("input path mutated: %v", path)
	}
	// A second render proves tree/parser state survived unchanged.
	if got := renderHelp(t, root, path, p); !strings.HasPrefix(got, "Usage: tool serve stop") {
		t.Errorf("second render after call differs: %q", got)
	}
}

type errWriter struct{ err error }

func (w errWriter) Write(p []byte) (int, error) { return 0, w.err }

func TestWriteHelpWriterFailureReturnedUnchanged(t *testing.T) {
	root := sampleHelpTree(t)
	sentinel := errors.New("broken pipe")
	err := root.WriteHelp(nil, nil, errWriter{err: sentinel})
	if err != sentinel {
		t.Fatalf("error = %v, want writer error unchanged", err)
	}
}

func TestWriteHelpNeverRunsHandlers(t *testing.T) {
	called := 0
	handler := func(path []string, args []string, stdout, stderr io.Writer) error {
		called++
		return nil
	}
	root := New("tool", "", nil, handler)
	if err := root.Add(New("sub", "", nil, handler)); err != nil {
		t.Fatal(err)
	}
	for _, path := range [][]string{nil, []string{"sub"}} {
		if err := root.WriteHelp(path, nil, io.Discard); err != nil {
			t.Fatalf("WriteHelp(%v): %v", path, err)
		}
	}
	if called != 0 {
		t.Errorf("handler ran %d times during help generation", called)
	}
}

func TestWriteHelpConcurrent(t *testing.T) {
	p, err := NewParser(
		[]Option{{Long: "port", Short: "p", Type: TypeInt, Default: "80"}},
		[]Positional{{Name: "dest"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	root := sampleHelpTree(t)
	want := renderHelp(t, root, []string{"serve"}, p)

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(aliasPath bool) {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				path := []string{"serve"}
				if aliasPath {
					path = []string{"srv"}
				}
				var buf bytes.Buffer
				if err := root.WriteHelp(path, p, &buf); err != nil {
					t.Errorf("concurrent WriteHelp: %v", err)
					return
				}
				if buf.String() != want {
					t.Errorf("concurrent output differs:\n%q\n%q", buf.String(), want)
					return
				}
			}
		}(i%2 == 0)
	}
	wg.Wait()
}

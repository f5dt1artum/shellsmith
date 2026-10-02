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

// helpTree builds a fixed tree used by several help tests:
//
//	tool (handler, desc "root tool")
//	├── serve (aliases srv, s; handler; desc "serve requests")
//	│   └── stop (no aliases, no handler, desc "stop serving")
//	└── config (no handler, no desc)
func helpTree(t *testing.T) *Node {
	t.Helper()
	root := New("tool", "root tool", nil, noop)
	serve := New("serve", "serve requests", []string{"srv", "s"}, noop)
	stop := New("stop", "stop serving", nil, nil)
	if err := serve.Add(stop); err != nil {
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

func helpString(t *testing.T, n *Node, path []string, p *Parser) string {
	t.Helper()
	var buf bytes.Buffer
	if err := n.WriteHelp(path, p, &buf); err != nil {
		t.Fatalf("WriteHelp(%v) error: %v", path, err)
	}
	return buf.String()
}

func TestWriteHelpLeafNoParser(t *testing.T) {
	leaf := New("tool", "", nil, noop)
	got := helpString(t, leaf, nil, nil)
	want := "Usage: tool\n"
	if got != want {
		t.Errorf("help = %q, want %q", got, want)
	}
}

func TestWriteHelpEmptyPathSelectsCurrentNode(t *testing.T) {
	root := helpTree(t)
	got := helpString(t, root, []string{}, nil)
	if !strings.HasPrefix(got, "Usage: tool ") {
		t.Errorf("empty path should select the current node, got %q", got)
	}
}

func TestWriteHelpUsageCommandMarkers(t *testing.T) {
	root := helpTree(t)

	// Node with children and a handler: [command].
	got := helpString(t, root, nil, nil)
	if !strings.HasPrefix(got, "Usage: tool [command]\n") {
		t.Errorf("root usage = %q, want [command] marker", got)
	}

	// Node with children but no handler: <command>.
	config := New("cfg", "", nil, nil)
	if err := config.Add(New("sub", "", nil, noop)); err != nil {
		t.Fatal(err)
	}
	got = helpString(t, config, nil, nil)
	if !strings.HasPrefix(got, "Usage: cfg <command>\n") {
		t.Errorf("handlerless parent usage = %q, want <command> marker", got)
	}

	// Leaf: no command marker at all.
	got = helpString(t, root, []string{"serve", "stop"}, nil)
	if got != "Usage: tool serve stop\n\nstop serving\n" {
		t.Errorf("leaf help = %q", got)
	}
}

func TestWriteHelpAliasResolvesToCanonicalPath(t *testing.T) {
	root := helpTree(t)
	got := helpString(t, root, []string{"s"}, nil)
	if !strings.HasPrefix(got, "Usage: tool serve ") {
		t.Errorf("alias path should render canonical names, got %q", got)
	}
	got = helpString(t, root, []string{"srv", "stop"}, nil)
	if !strings.HasPrefix(got, "Usage: tool serve stop\n") {
		t.Errorf("nested alias path should render canonical names, got %q", got)
	}
}

func TestWriteHelpCaseSensitiveMiss(t *testing.T) {
	root := helpTree(t)
	var buf bytes.Buffer
	err := root.WriteHelp([]string{"Serve"}, nil, &buf)
	if !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("WriteHelp(Serve) error = %v, want ErrUnknownCommand", err)
	}
	if buf.Len() != 0 {
		t.Errorf("writer received %d bytes on lookup failure", buf.Len())
	}
}

func TestWriteHelpDescriptionVerbatim(t *testing.T) {
	root := helpTree(t)
	got := helpString(t, root, nil, nil)
	if !strings.Contains(got, "\n\nroot tool\n") {
		t.Errorf("description should follow one blank line verbatim, got %q", got)
	}
	multi := New("m", "line one\nline two", nil, noop)
	got = helpString(t, multi, nil, nil)
	if got != "Usage: m\n\nline one\nline two\n" {
		t.Errorf("multi-line description not preserved verbatim: %q", got)
	}
}

func TestWriteHelpCommandsSection(t *testing.T) {
	root := helpTree(t)
	got := helpString(t, root, nil, nil)
	want := "Usage: tool [command]\n" +
		"\n" +
		"root tool\n" +
		"\n" +
		"Commands:\n" +
		"  serve (srv, s)  serve requests\n" +
		"  config\n"
	if got != want {
		t.Errorf("help =\n%q\nwant\n%q", got, want)
	}
}

func TestWriteHelpCommandsKeepAddOrder(t *testing.T) {
	root := New("tool", "", nil, noop)
	for _, name := range []string{"zeta", "alpha", "mid"} {
		if err := root.Add(New(name, "", nil, noop)); err != nil {
			t.Fatal(err)
		}
	}
	got := helpString(t, root, nil, nil)
	zi, ai, mi := strings.Index(got, "zeta"), strings.Index(got, "alpha"), strings.Index(got, "mid")
	if !(zi < ai && ai < mi) {
		t.Errorf("commands not in Add order: %q", got)
	}
}

func TestWriteHelpUsageWithParser(t *testing.T) {
	p, err := NewParser(
		[]Option{{Long: "verbose", Short: "v", Type: TypeBool}},
		[]Positional{{Name: "src"}, {Name: "dst", Variadic: true}},
	)
	if err != nil {
		t.Fatal(err)
	}
	leaf := New("cp", "", nil, noop)
	got := helpString(t, leaf, nil, p)
	want := "Usage: cp [options] <src> [<dst>...]\n" +
		"\n" +
		"Options:\n" +
		"  -v, --verbose\n"
	if got != want {
		t.Errorf("help =\n%q\nwant\n%q", got, want)
	}
}

func TestWriteHelpParserWithoutOptionsOmitsOptionsMarker(t *testing.T) {
	p, err := NewParser(nil, []Positional{{Name: "file"}})
	if err != nil {
		t.Fatal(err)
	}
	leaf := New("cat", "", nil, noop)
	got := helpString(t, leaf, nil, p)
	if got != "Usage: cat <file>\n" {
		t.Errorf("help = %q, want no [options] and no Options section", got)
	}
}

func TestWriteHelpOptionsSection(t *testing.T) {
	p, err := NewParser([]Option{
		{Long: "verbose", Short: "v", Type: TypeBool, Repeatable: true},
		{Long: "output", Short: "o", Type: TypeString, Required: true},
		{Long: "port", Type: TypeInt, Default: "8080"},
		{Long: "timeout", Short: "t", Type: TypeDuration, Default: "1s", Repeatable: true},
		{Long: "mode", Type: TypeString},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf := New("svc", "", nil, noop)
	got := helpString(t, leaf, nil, p)
	want := "Usage: svc [options]\n" +
		"\n" +
		"Options:\n" +
		"  -v, --verbose  (repeatable)\n" +
		"  -o, --output <string>  (required)\n" +
		"  --port <int>  (default: 8080)\n" +
		"  -t, --timeout <duration>  (repeatable, default: 1s)\n" +
		"  --mode <string>\n"
	if got != want {
		t.Errorf("help =\n%q\nwant\n%q", got, want)
	}
}

func TestWriteHelpFullLayout(t *testing.T) {
	p, err := NewParser(
		[]Option{{Long: "level", Short: "l", Type: TypeInt, Default: "1"}},
		[]Positional{{Name: "target"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	root := helpTree(t)
	got := helpString(t, root, []string{"serve"}, p)
	want := "Usage: tool serve [command] [options] <target>\n" +
		"\n" +
		"serve requests\n" +
		"\n" +
		"Commands:\n" +
		"  stop  stop serving\n" +
		"\n" +
		"Options:\n" +
		"  -l, --level <int>  (default: 1)\n"
	if got != want {
		t.Errorf("help =\n%q\nwant\n%q", got, want)
	}
}

func TestWriteHelpDeterministicAndCleanBytes(t *testing.T) {
	p, err := NewParser([]Option{
		{Long: "verbose", Short: "v", Type: TypeBool},
		{Long: "port", Type: TypeInt, Default: "80"},
	}, []Positional{{Name: "src"}})
	if err != nil {
		t.Fatal(err)
	}
	root := helpTree(t)
	first := helpString(t, root, nil, p)
	for i := 0; i < 5; i++ {
		if got := helpString(t, root, nil, p); got != first {
			t.Fatalf("call %d differs:\n%q\nvs\n%q", i, got, first)
		}
	}
	if !strings.HasSuffix(first, "\n") || strings.HasSuffix(first, "\n\n") {
		t.Errorf("output must end with exactly one LF: %q", first)
	}
	if strings.ContainsRune(first, '\r') || strings.ContainsRune(first, 0x1b) {
		t.Errorf("output contains CR or ANSI escape: %q", first)
	}
	if strings.Contains(first, "\n\n\n") {
		t.Errorf("output contains more than one consecutive blank line: %q", first)
	}
}

func TestWriteHelpLookupError(t *testing.T) {
	root := helpTree(t)
	var buf bytes.Buffer
	err := root.WriteHelp([]string{"serve", "bogus", "extra"}, nil, &buf)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrUnknownCommand) {
		t.Errorf("error = %v, want errors.Is ErrUnknownCommand", err)
	}
	var le *LookupError
	if !errors.As(err, &le) {
		t.Fatalf("error is %T, want *LookupError", err)
	}
	if !reflect.DeepEqual(le.Path, []string{"tool", "serve"}) {
		t.Errorf("Path = %v, want [tool serve]", le.Path)
	}
	if le.Arg != "bogus" {
		t.Errorf("Arg = %q, want %q", le.Arg, "bogus")
	}
	if buf.Len() != 0 {
		t.Errorf("writer received %d bytes on lookup failure", buf.Len())
	}
}

func TestWriteHelpLookupErrorAtRoot(t *testing.T) {
	root := helpTree(t)
	err := root.WriteHelp([]string{"nope"}, nil, io.Discard)
	var le *LookupError
	if !errors.As(err, &le) {
		t.Fatalf("error is %T, want *LookupError", err)
	}
	if !reflect.DeepEqual(le.Path, []string{"tool"}) || le.Arg != "nope" {
		t.Errorf("LookupError = %+v, want Path [tool] Arg nope", le)
	}
}

// failWriter always fails after optionally accepting some bytes.
type failWriter struct{ err error }

func (w failWriter) Write([]byte) (int, error) { return 0, w.err }

func TestWriteHelpWriterErrorReturnedUnchanged(t *testing.T) {
	root := helpTree(t)
	sentinel := errors.New("disk full")
	err := root.WriteHelp(nil, nil, failWriter{err: sentinel})
	if err != sentinel {
		t.Errorf("error = %v, want the writer's exact error", err)
	}
}

func TestWriteHelpDoesNotRunHandlersOrMutate(t *testing.T) {
	called := false
	handler := func(path []string, args []string, stdout, stderr io.Writer) error {
		called = true
		return nil
	}
	root := New("tool", "", nil, handler)
	if err := root.Add(New("sub", "", nil, handler)); err != nil {
		t.Fatal(err)
	}
	path := []string{"sub"}
	if err := root.WriteHelp(path, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("WriteHelp must not run handlers")
	}
	if !reflect.DeepEqual(path, []string{"sub"}) {
		t.Errorf("input path mutated: %v", path)
	}
	if got := root.Name(); got != "tool" {
		t.Errorf("tree mutated, root name = %q", got)
	}
	if aliases := root.Aliases(); len(aliases) != 0 {
		t.Errorf("tree mutated, root aliases = %v", aliases)
	}
}

func TestWriteHelpConcurrent(t *testing.T) {
	p, err := NewParser([]Option{
		{Long: "verbose", Short: "v", Type: TypeBool},
		{Long: "port", Type: TypeInt, Default: "80"},
	}, []Positional{{Name: "src"}})
	if err != nil {
		t.Fatal(err)
	}
	root := helpTree(t)
	want := helpString(t, root, []string{"serve"}, p)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				if got := helpString(t, root, []string{"srv"}, p); got != want {
					t.Errorf("concurrent help differs:\n%q\nvs\n%q", got, want)
					return
				}
			}
		}()
	}
	wg.Wait()
}

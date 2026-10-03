package command

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// completionFixture builds:
//
//	my-tool (alias mt; no handler)
//	├── serve (alias srv; parser: verbose -v bool, output -o string,
//	│          tag -t repeatable string, count int; positional file)
//	├── config (alias cfg; no parser)
//	└── admin
//	    └── user (alias u; parser: force -f bool)
func completionFixture(t *testing.T) (*Node, map[*Node]*Parser) {
	t.Helper()
	root := New("my-tool", "root command", []string{"mt"}, nil)
	serve := New("serve", "Serve files", []string{"srv"}, noop)
	config := New("config", "Manage settings", []string{"cfg"}, noop)
	admin := New("admin", "Administration", nil, nil)
	user := New("user", "Manage users", []string{"u"}, noop)
	if err := admin.Add(user); err != nil {
		t.Fatalf("Add(user): %v", err)
	}
	for _, child := range []*Node{serve, config, admin} {
		if err := root.Add(child); err != nil {
			t.Fatalf("Add(%s): %v", child.Name(), err)
		}
	}

	serveParser, err := NewParser([]Option{
		{Long: "verbose", Short: "v", Type: TypeBool},
		{Long: "output", Short: "o", Type: TypeString},
		{Long: "tag", Short: "t", Type: TypeString, Repeatable: true},
		{Long: "count", Type: TypeInt},
	}, []Positional{{Name: "file"}})
	if err != nil {
		t.Fatalf("NewParser(serve): %v", err)
	}
	userParser, err := NewParser([]Option{
		{Long: "force", Short: "f", Type: TypeBool},
	}, nil)
	if err != nil {
		t.Fatalf("NewParser(user): %v", err)
	}
	return root, map[*Node]*Parser{serve: serveParser, user: userParser}
}

func renderCompletion(t *testing.T, n *Node, shell string, parsers map[*Node]*Parser) string {
	t.Helper()
	var buf bytes.Buffer
	if err := n.WriteCompletion(shell, parsers, &buf); err != nil {
		t.Fatalf("WriteCompletion(%q): %v", shell, err)
	}
	return buf.String()
}

var allShells = []string{"bash", "zsh", "fish", "powershell"}

func TestWriteCompletionUnsupportedShell(t *testing.T) {
	root, parsers := completionFixture(t)
	for _, shell := range []string{"", "Bash", "sh", "zsh5", "pwsh", "powershell.exe", " fish"} {
		var buf bytes.Buffer
		err := root.WriteCompletion(shell, parsers, &buf)
		if !errors.Is(err, ErrUnsupportedShell) {
			t.Errorf("shell %q: error = %v, want errors.Is ErrUnsupportedShell", shell, err)
		}
		var se *ShellError
		if !errors.As(err, &se) || se.Shell != shell {
			t.Errorf("shell %q: error does not carry *ShellError{Shell: %q}: %v", shell, shell, err)
		}
		if buf.Len() != 0 {
			t.Errorf("shell %q: %d bytes written on error, want 0", shell, buf.Len())
		}
	}
}

func TestWriteCompletionInvalidRootName(t *testing.T) {
	for _, name := range []string{"", "-x", "a b", "a.b", "a/b", "日本", "a\nb"} {
		root := New(name, "", nil, noop)
		var buf bytes.Buffer
		err := root.WriteCompletion("bash", nil, &buf)
		if !errors.Is(err, ErrInvalidName) {
			t.Errorf("root %q: error = %v, want errors.Is ErrInvalidName", name, err)
		}
		var ne *NameError
		if !errors.As(err, &ne) || ne.Name != name {
			t.Errorf("root %q: error does not carry *NameError{Name: %q}: %v", name, name, err)
		}
		if buf.Len() != 0 {
			t.Errorf("root %q: %d bytes written on error, want 0", name, buf.Len())
		}
	}
}

func TestWriteCompletionDeterministicAndLF(t *testing.T) {
	root, parsers := completionFixture(t)
	for _, shell := range allShells {
		first := renderCompletion(t, root, shell, parsers)
		second := renderCompletion(t, root, shell, parsers)
		if first != second {
			t.Errorf("%s: two runs differ", shell)
		}
		if strings.ContainsRune(first, '\r') {
			t.Errorf("%s: output contains CR", shell)
		}
		if !strings.HasSuffix(first, "\n") || strings.HasSuffix(first, "\n\n") {
			t.Errorf("%s: output must end with exactly one LF, got %q", shell, first[len(first)-8:])
		}
	}
}

func TestWriteCompletionRegistration(t *testing.T) {
	root, parsers := completionFixture(t)
	want := map[string]string{
		"bash":       "complete -o default -F _my_tool_complete my-tool",
		"zsh":        "compdef _my_tool_complete my-tool",
		"fish":       "complete -c my-tool -a '(_my_tool_complete)'",
		"powershell": "Register-ArgumentCompleter -Native -CommandName 'my-tool'",
	}
	for shell, line := range want {
		if got := renderCompletion(t, root, shell, parsers); !strings.Contains(got, line) {
			t.Errorf("%s: script does not register the root name, want line %q", shell, line)
		}
	}
}

func TestWriteCompletionNilAndEmptyParserMap(t *testing.T) {
	root, _ := completionFixture(t)
	for _, shell := range allShells {
		viaNil := renderCompletion(t, root, shell, nil)
		viaEmpty := renderCompletion(t, root, shell, map[*Node]*Parser{})
		if viaNil != viaEmpty {
			t.Errorf("%s: nil and empty parser maps differ", shell)
		}
		if !strings.Contains(viaNil, "my-tool") {
			t.Errorf("%s: script without parsers does not mention root", shell)
		}
	}
}

func TestWriteCompletionIgnoresForeignParsers(t *testing.T) {
	root, parsers := completionFixture(t)
	foreign := New("foreign", "", nil, noop)
	foreignParser, err := NewParser([]Option{{Long: "x"}}, nil)
	if err != nil {
		t.Fatalf("NewParser: %v", err)
	}
	mixed := map[*Node]*Parser{foreign: foreignParser}
	for node, p := range parsers {
		mixed[node] = p
	}
	for _, shell := range allShells {
		if got, want := renderCompletion(t, root, shell, mixed), renderCompletion(t, root, shell, parsers); got != want {
			t.Errorf("%s: foreign parser entries changed the output", shell)
		}
	}
	// A map holding only foreign nodes behaves like an empty map.
	only := map[*Node]*Parser{foreign: foreignParser}
	for _, shell := range allShells {
		if got, want := renderCompletion(t, root, shell, only), renderCompletion(t, root, shell, nil); got != want {
			t.Errorf("%s: foreign-only parser map changed the output", shell)
		}
	}
}

func TestWriteCompletionDoesNotRunHandlers(t *testing.T) {
	boom := func(path []string, args []string, stdout, stderr io.Writer) error {
		t.Fatal("handler ran during completion generation")
		return nil
	}
	root := New("tool", "", nil, boom)
	child := New("sub", "", nil, boom)
	if err := root.Add(child); err != nil {
		t.Fatalf("Add: %v", err)
	}
	for _, shell := range allShells {
		renderCompletion(t, root, shell, nil)
	}
}

// failWriter fails every write with errCompletionWrite after writing nothing.
type failWriter struct{ calls *int }

var errCompletionWrite = errors.New("write failed")

func (w failWriter) Write(p []byte) (int, error) {
	*w.calls++
	return 0, errCompletionWrite
}

func TestWriteCompletionWriteErrorReturnedUnchanged(t *testing.T) {
	root, parsers := completionFixture(t)
	for _, shell := range allShells {
		calls := 0
		err := root.WriteCompletion(shell, parsers, failWriter{&calls})
		if !errors.Is(err, errCompletionWrite) {
			t.Errorf("%s: error = %v, want the writer's error", shell, err)
		}
		if calls != 1 {
			t.Errorf("%s: %d write calls, want exactly 1 (render fully, then write)", shell, calls)
		}
	}
}

// countWriter records how many Write calls it receives.
type countWriter struct {
	calls int
	bytes int
}

func (w *countWriter) Write(p []byte) (int, error) {
	w.calls++
	w.bytes += len(p)
	return len(p), nil
}

func TestWriteCompletionSingleWriteAfterSuccess(t *testing.T) {
	root, parsers := completionFixture(t)
	for _, shell := range allShells {
		w := &countWriter{}
		if err := root.WriteCompletion(shell, parsers, w); err != nil {
			t.Fatalf("%s: %v", shell, err)
		}
		if w.calls != 1 {
			t.Errorf("%s: %d write calls, want 1", shell, w.calls)
		}
	}
}

func TestWriteCompletionConcurrent(t *testing.T) {
	root, parsers := completionFixture(t)
	want := map[string]string{}
	for _, shell := range allShells {
		want[shell] = renderCompletion(t, root, shell, parsers)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 16; i++ {
		for _, shell := range allShells {
			wg.Add(1)
			go func(shell string) {
				defer wg.Done()
				var buf bytes.Buffer
				if err := root.WriteCompletion(shell, parsers, &buf); err != nil {
					errs <- err
					return
				}
				if buf.String() != want[shell] {
					errs <- fmt.Errorf("%s: concurrent output differs", shell)
				}
			}(shell)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestWriteCompletionEscaping(t *testing.T) {
	desc := "it's a \"test\"\nwith $dollar `backtick` \\backslash\\ and [brackets]"
	root := New("tool", "", nil, nil)
	if err := root.Add(New("run", desc, nil, noop)); err != nil {
		t.Fatalf("Add: %v", err)
	}

	zsh := renderCompletion(t, root, "zsh", nil)
	if !strings.Contains(zsh, "'it'\\''s a \"test\"\nwith $dollar `backtick` \\backslash\\ and [brackets]'") {
		t.Errorf("zsh: description not single-quote escaped:\n%s", zsh)
	}
	fish := renderCompletion(t, root, "fish", nil)
	if !strings.Contains(fish, `'it\'s a "test"`+"\n"+`with $dollar `+"`backtick`"+` \\backslash\\ and [brackets]'`) {
		t.Errorf("fish: description not escaped:\n%s", fish)
	}
	ps := renderCompletion(t, root, "powershell", nil)
	if !strings.Contains(ps, "'it''s a \"test\"\nwith $dollar `backtick` \\backslash\\ and [brackets]'") {
		t.Errorf("powershell: description not single-quote escaped:\n%s", ps)
	}

	// The generated scripts must still parse under their shells when the
	// shells are available.
	syntaxCheck := map[string][]string{
		"bash": {"bash", "-n"},
		"zsh":  {"zsh", "-n"},
		"fish": {"fish", "-n"},
	}
	for shell, argv := range syntaxCheck {
		path, err := exec.LookPath(argv[0])
		if err != nil {
			continue
		}
		file := filepath.Join(t.TempDir(), "comp."+shell)
		if err := os.WriteFile(file, []byte(renderCompletion(t, root, shell, nil)), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(path, append(argv[1:], file)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s syntax check failed: %v\n%s", shell, err, out)
		}
	}
}

// runBashCompletion feeds the generated bash script a command line and
// returns the COMPREPLY entries the completion function produces.
func runBashCompletion(t *testing.T, script string, cword int, words ...string) []string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "comp.bash")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	harness := `script="$1"; shift
cword="$1"; shift
source "$script"
COMP_WORDS=("$@")
COMP_CWORD=$cword
_my_tool_complete
for c in ${COMPREPLY[@]+"${COMPREPLY[@]}"}; do printf '%s\n' "$c"; done
`
	harnessPath := filepath.Join(dir, "harness.bash")
	if err := os.WriteFile(harnessPath, []byte(harness), 0o644); err != nil {
		t.Fatal(err)
	}
	args := append([]string{harnessPath, scriptPath, fmt.Sprint(cword)}, words...)
	out, err := exec.Command(bash, args...).Output()
	if err != nil {
		t.Fatalf("bash harness: %v", err)
	}
	text := strings.TrimSuffix(string(out), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func TestBashCompletionBehavior(t *testing.T) {
	root, parsers := completionFixture(t)
	script := renderCompletion(t, root, "bash", parsers)

	cases := []struct {
		label string
		cword int
		words []string
		want  []string
	}{
		{"root candidates in Add order with aliases", 1, []string{"my-tool", ""},
			[]string{"serve", "srv", "config", "cfg", "admin"}},
		{"prefix filter", 1, []string{"my-tool", "s"}, []string{"serve", "srv"}},
		{"case sensitive", 1, []string{"my-tool", "S"}, nil},
		{"leaf long options", 2, []string{"my-tool", "serve", "--"},
			[]string{"--verbose", "--output", "--tag", "--count"}},
		{"dash offers shorts and longs", 2, []string{"my-tool", "serve", "-"},
			[]string{"-v", "-o", "-t", "--verbose", "--output", "--tag", "--count"}},
		{"long prefix filter", 2, []string{"my-tool", "serve", "--v"}, []string{"--verbose"}},
		{"seen non-repeatable hidden", 3, []string{"my-tool", "serve", "--verbose", "--"},
			[]string{"--output", "--tag", "--count"}},
		{"seen via short hidden", 3, []string{"my-tool", "serve", "-v", "--"},
			[]string{"--output", "--tag", "--count"}},
		{"repeatable stays", 4, []string{"my-tool", "serve", "--tag", "a", "--tag"},
			[]string{"--tag"}},
		{"long value context", 3, []string{"my-tool", "serve", "--output", "--"}, nil},
		{"long equals value recognized", 3, []string{"my-tool", "serve", "--output=x", "--"},
			[]string{"--verbose", "--tag", "--count"}},
		{"short value context", 3, []string{"my-tool", "serve", "-o", "--"}, nil},
		{"cluster value context", 3, []string{"my-tool", "serve", "-vo", "--"}, nil},
		{"bool does not consume next", 3, []string{"my-tool", "serve", "--verbose", "--"},
			[]string{"--output", "--tag", "--count"}},
		{"double dash stops", 3, []string{"my-tool", "serve", "--", "--"}, nil},
		{"unknown intermediate stops", 2, []string{"my-tool", "bogus", "--"}, nil},
		{"alias enters subcommand", 2, []string{"my-tool", "srv", "--"},
			[]string{"--verbose", "--output", "--tag", "--count"}},
		{"nested children", 2, []string{"my-tool", "admin", ""}, []string{"user", "u"}},
		{"nested leaf options", 3, []string{"my-tool", "admin", "user", "--"}, []string{"--force"}},
		{"nested alias enters", 3, []string{"my-tool", "admin", "u", "--"}, []string{"--force"}},
		{"leaf without parser", 2, []string{"my-tool", "config", "--"}, nil},
		{"positional keeps default", 2, []string{"my-tool", "serve", ""}, nil},
		{"value may start with dash", 4, []string{"my-tool", "serve", "--count", "-5", "--"},
			[]string{"--verbose", "--output", "--tag"}},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			got := runBashCompletion(t, script, c.cword, c.words...)
			if strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("words %v cword %d: got %v, want %v", c.words, c.cword, got, c.want)
			}
		})
	}
}

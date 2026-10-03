package command

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// completionFixture builds a tree with an internal node, a leaf carrying a
// parser, a leaf without one, and a deeper level, plus the parser map.
func completionFixture(t *testing.T) (*Node, map[*Node]*Parser) {
	t.Helper()
	root := New("myapp", "test app", nil, nil)
	serve := New("serve", "start the server", []string{"s"}, nil)
	list := New("list", "list items", nil, nil)
	deep := New("deep", "deep node", nil, nil)
	leaf := New("leaf", "leaf node", []string{"l"}, nil)
	for _, pair := range [][2]*Node{{root, serve}, {root, list}, {root, deep}, {deep, leaf}} {
		if err := pair[0].Add(pair[1]); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	parser, err := NewParser([]Option{
		{Long: "output", Short: "o", Type: TypeString},
		{Long: "verbose", Short: "v", Type: TypeBool},
		{Long: "tag", Short: "t", Type: TypeString, Repeatable: true},
		{Long: "count", Type: TypeInt},
	}, nil)
	if err != nil {
		t.Fatalf("NewParser: %v", err)
	}
	return root, map[*Node]*Parser{serve: parser}
}

func generateCompletion(t *testing.T, shell string, root *Node, parsers map[*Node]*Parser) string {
	t.Helper()
	var buf bytes.Buffer
	if err := root.WriteCompletion(shell, parsers, &buf); err != nil {
		t.Fatalf("WriteCompletion(%q): %v", shell, err)
	}
	return buf.String()
}

func TestWriteCompletionUnsupportedShell(t *testing.T) {
	root, parsers := completionFixture(t)
	for _, shell := range []string{"", "sh", "BASH", "pwsh", "zsh5", " powershell"} {
		var buf bytes.Buffer
		err := root.WriteCompletion(shell, parsers, &buf)
		if err == nil {
			t.Fatalf("shell %q: expected error", shell)
		}
		if !errors.Is(err, ErrUnsupportedShell) {
			t.Errorf("shell %q: error %v does not wrap ErrUnsupportedShell", shell, err)
		}
		var shellErr *ShellError
		if !errors.As(err, &shellErr) {
			t.Fatalf("shell %q: error is not a *ShellError", shell)
		}
		if shellErr.Shell != shell {
			t.Errorf("ShellError.Shell = %q, want %q", shellErr.Shell, shell)
		}
		if buf.Len() != 0 {
			t.Errorf("shell %q: %d bytes written on error", shell, buf.Len())
		}
	}
}

func TestWriteCompletionInvalidRootName(t *testing.T) {
	for _, name := range []string{"", "-bad", "bad name", "bad/name", "héllo"} {
		root := New(name, "desc", nil, nil)
		for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
			var buf bytes.Buffer
			err := root.WriteCompletion(shell, nil, &buf)
			if err == nil {
				t.Fatalf("root %q shell %q: expected error", name, shell)
			}
			if !errors.Is(err, ErrInvalidName) {
				t.Errorf("root %q: error %v does not wrap ErrInvalidName", name, err)
			}
			var nameErr *NameError
			if !errors.As(err, &nameErr) || nameErr.Name != name {
				t.Errorf("root %q: NameError = %+v", name, nameErr)
			}
			if buf.Len() != 0 {
				t.Errorf("root %q: %d bytes written on error", name, buf.Len())
			}
		}
	}
}

func TestWriteCompletionByteStabilityAndLineEndings(t *testing.T) {
	root, parsers := completionFixture(t)
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		first := generateCompletion(t, shell, root, parsers)
		second := generateCompletion(t, shell, root, parsers)
		if first != second {
			t.Errorf("shell %q: output not byte-stable", shell)
		}
		if !strings.HasSuffix(first, "\n") || strings.HasSuffix(first, "\n\n") {
			t.Errorf("shell %q: output must end with exactly one LF", shell)
		}
		if strings.Contains(first, "\r") {
			t.Errorf("shell %q: output contains CR", shell)
		}
	}
}

func TestWriteCompletionRegistrations(t *testing.T) {
	root, parsers := completionFixture(t)
	checks := map[string]string{
		"bash":       "complete -o default -F __myapp_complete myapp",
		"zsh":        "compdef __myapp_complete myapp",
		"fish":       "complete -c myapp -a '(__myapp_complete)'",
		"powershell": "Register-ArgumentCompleter -Native -CommandName 'myapp'",
	}
	for shell, want := range checks {
		got := generateCompletion(t, shell, root, parsers)
		if !strings.Contains(got, want) {
			t.Errorf("shell %q: registration %q not found in:\n%s", shell, want, got)
		}
	}
}

func TestWriteCompletionIgnoresForeignAndEmptyMaps(t *testing.T) {
	root, _ := completionFixture(t)
	foreign := New("foreign", "not in the tree", nil, nil)
	parser, err := NewParser([]Option{{Long: "x", Type: TypeString}}, nil)
	if err != nil {
		t.Fatalf("NewParser: %v", err)
	}
	for _, parsers := range []map[*Node]*Parser{nil, {}, {foreign: parser}} {
		for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
			out := generateCompletion(t, shell, root, parsers)
			if strings.Contains(out, "foreign") || strings.Contains(out, "--x") {
				t.Errorf("shell %q: foreign parser leaked into output", shell)
			}
		}
	}
}

func TestWriteCompletionWriteErrorPropagates(t *testing.T) {
	root, parsers := completionFixture(t)
	sentinel := errors.New("disk full")
	err := root.WriteCompletion("bash", parsers, errWriter{err: sentinel})
	if err != sentinel {
		t.Fatalf("got %v, want the writer's exact error", err)
	}
}

func TestWriteCompletionConcurrent(t *testing.T) {
	root, parsers := completionFixture(t)
	want := map[string]string{}
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		want[shell] = generateCompletion(t, shell, root, parsers)
	}
	done := make(chan string, 32)
	for i := 0; i < 8; i++ {
		for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
			go func(shell string) {
				var buf bytes.Buffer
				if err := root.WriteCompletion(shell, parsers, &buf); err != nil {
					done <- shell + "\x00error: " + err.Error()
					return
				}
				done <- shell + "\x00" + buf.String()
			}(shell)
		}
	}
	for i := 0; i < 32; i++ {
		parts := strings.SplitN(<-done, "\x00", 2)
		if parts[1] != want[parts[0]] {
			t.Errorf("shell %q: concurrent output differs", parts[0])
		}
	}
}

func TestWriteCompletionEscaping(t *testing.T) {
	root := New("myapp", "root", nil, nil)
	desc := "quote ' backslash \\ dollar $ backtick ` newline\n tab\t"
	tricky := New("run", desc, nil, nil)
	if err := root.Add(tricky); err != nil {
		t.Fatalf("Add: %v", err)
	}

	zsh := generateCompletion(t, "zsh", root, nil)
	wantZsh := "'" + strings.ReplaceAll(desc, "'", `'\''`) + "'"
	if !strings.Contains(zsh, wantZsh) {
		t.Errorf("zsh: description not safely single-quoted:\n%s", zsh)
	}

	fish := generateCompletion(t, "fish", root, nil)
	wantFish := "'" + strings.ReplaceAll(strings.ReplaceAll("run\t"+desc, `\`, `\\`), `'`, `\'`) + "'"
	if !strings.Contains(fish, wantFish) {
		t.Errorf("fish: description not safely single-quoted:\n%s", fish)
	}

	ps := generateCompletion(t, "powershell", root, nil)
	wantPS := "'" + strings.ReplaceAll(desc, "'", "''") + "'"
	if !strings.Contains(ps, wantPS) {
		t.Errorf("powershell: description not safely single-quoted:\n%s", ps)
	}

	// The generated bash script must still parse.
	dir := t.TempDir()
	bashScript := generateCompletion(t, "bash", root, nil)
	path := filepath.Join(dir, "comp.bash")
	if err := os.WriteFile(path, []byte(bashScript), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
		t.Errorf("bash -n rejected script: %v\n%s", err, out)
	}
}

// runBashCompletion sources the generated bash script and evaluates the
// completion function with a simulated command line. words must include
// the command name at index 0 and the word being completed last.
func runBashCompletion(t *testing.T, script string, words ...string) []string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "comp.bash")
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	var driver strings.Builder
	fmt.Fprintf(&driver, "source %q\n", path)
	driver.WriteString("COMP_WORDS=(")
	for _, w := range words {
		fmt.Fprintf(&driver, "%q ", w)
	}
	driver.WriteString(")\n")
	fmt.Fprintf(&driver, "COMP_CWORD=%d\n", len(words)-1)
	driver.WriteString("__myapp_complete\n")
	driver.WriteString("printf '%s\\n' \"${COMPREPLY[@]}\"\n")
	out, err := exec.Command("bash", "-c", driver.String()).CombinedOutput()
	if err != nil {
		t.Fatalf("bash completion failed: %v\n%s", err, out)
	}
	trimmed := strings.TrimRight(string(out), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func TestWriteCompletionBashBehavior(t *testing.T) {
	root, parsers := completionFixture(t)
	script := generateCompletion(t, "bash", root, parsers)

	cases := []struct {
		name  string
		words []string
		want  []string
	}{
		{"root candidates in add order", []string{"myapp", ""}, []string{"serve", "s", "list", "deep"}},
		{"prefix filter", []string{"myapp", "s"}, []string{"serve", "s"}},
		{"prefix filter longer", []string{"myapp", "se"}, []string{"serve"}},
		{"case sensitive", []string{"myapp", "S"}, nil},
		{"leaf options", []string{"myapp", "serve", ""}, []string{"--output", "-o", "--verbose", "-v", "--tag", "-t", "--count"}},
		{"alias enters child", []string{"myapp", "s", ""}, []string{"--output", "-o", "--verbose", "-v", "--tag", "-t", "--count"}},
		{"single dash", []string{"myapp", "serve", "-"}, []string{"--output", "-o", "--verbose", "-v", "--tag", "-t", "--count"}},
		{"double dash prefix", []string{"myapp", "serve", "--"}, []string{"--output", "--verbose", "--tag", "--count"}},
		{"short prefix", []string{"myapp", "serve", "-v"}, []string{"-v"}},
		{"bool does not swallow", []string{"myapp", "serve", "--verbose", ""}, []string{"--output", "-o", "--tag", "-t", "--count"}},
		{"long awaits value", []string{"myapp", "serve", "--output", ""}, nil},
		{"long value consumed", []string{"myapp", "serve", "--output", "x", ""}, []string{"--verbose", "-v", "--tag", "-t", "--count"}},
		{"long equals value", []string{"myapp", "serve", "--output=x", ""}, []string{"--verbose", "-v", "--tag", "-t", "--count"}},
		{"short awaits value", []string{"myapp", "serve", "-o", ""}, nil},
		{"short attached value", []string{"myapp", "serve", "-ox", ""}, []string{"--verbose", "-v", "--tag", "-t", "--count"}},
		{"cluster bool then value option", []string{"myapp", "serve", "-vo", ""}, nil},
		{"cluster with attached value", []string{"myapp", "serve", "-vox", ""}, []string{"--tag", "-t", "--count"}},
		{"repeatable stays", []string{"myapp", "serve", "--tag", "a", "--tag", "b", ""}, []string{"--output", "-o", "--verbose", "-v", "--tag", "-t", "--count"}},
		{"after double dash", []string{"myapp", "serve", "--", ""}, nil},
		{"unknown intermediate", []string{"myapp", "bogus", ""}, nil},
		{"unknown intermediate deeper", []string{"myapp", "bogus", "serve", ""}, nil},
		{"leaf without parser", []string{"myapp", "list", ""}, nil},
		{"deeper level", []string{"myapp", "deep", ""}, []string{"leaf", "l"}},
		{"deeper leaf no parser", []string{"myapp", "deep", "leaf", ""}, nil},
		{"deeper unknown", []string{"myapp", "deep", "bogus", ""}, nil},
		{"long option value context via equals", []string{"myapp", "serve", "--output="}, nil},
	}
	for _, tc := range cases {
		got := runBashCompletion(t, script, tc.words...)
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestWriteCompletionBashRootLeaf(t *testing.T) {
	root := New("myapp", "test app", nil, nil)
	parser, err := NewParser([]Option{
		{Long: "level", Short: "l", Type: TypeInt},
		{Long: "force", Type: TypeBool},
	}, []Positional{{Name: "target"}})
	if err != nil {
		t.Fatalf("NewParser: %v", err)
	}
	script := generateCompletion(t, "bash", root, map[*Node]*Parser{root: parser})

	cases := []struct {
		name  string
		words []string
		want  []string
	}{
		{"root options", []string{"myapp", ""}, []string{"--level", "-l", "--force"}},
		{"options still offered after positional", []string{"myapp", "file", ""}, []string{"--level", "-l", "--force"}},
		{"int option awaits value", []string{"myapp", "--level", ""}, nil},
		{"bool then options", []string{"myapp", "--force", ""}, []string{"--level", "-l"}},
	}
	for _, tc := range cases {
		got := runBashCompletion(t, script, tc.words...)
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestWriteCompletionShellSyntax loads each generated script with its real
// interpreter when one is available on the system.
func TestWriteCompletionShellSyntax(t *testing.T) {
	root := New("myapp", "root", nil, nil)
	tricky := New("run", "quote ' backslash \\ dollar $ backtick ` newline\nsemi;", []string{"r"}, nil)
	if err := root.Add(tricky); err != nil {
		t.Fatalf("Add: %v", err)
	}
	dir := t.TempDir()
	write := func(shell string) string {
		t.Helper()
		path := filepath.Join(dir, "comp."+shell)
		if err := os.WriteFile(path, []byte(generateCompletion(t, shell, root, nil)), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	if zsh, err := exec.LookPath("zsh"); err == nil {
		if out, err := exec.Command(zsh, "-n", write("zsh")).CombinedOutput(); err != nil {
			t.Errorf("zsh -n rejected script: %v\n%s", err, out)
		}
	}
	if fish, err := exec.LookPath("fish"); err == nil {
		if out, err := exec.Command(fish, "-c", "source "+write("fish")).CombinedOutput(); err != nil {
			t.Errorf("fish could not load script: %v\n%s", err, out)
		}
	}
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		pwsh, err = exec.LookPath("powershell")
	}
	if err == nil {
		driver := filepath.Join(dir, "check.ps1")
		script := `
$errors = $null; $tokens = $null
[System.Management.Automation.Language.Parser]::ParseFile($args[0], [ref]$tokens, [ref]$errors) | Out-Null
if ($errors.Count -gt 0) { $errors | ForEach-Object { Write-Host $_.Message }; exit 1 }
. $args[0]
`
		if err := os.WriteFile(driver, []byte(script), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(pwsh, "-NoProfile", "-File", driver, write("powershell")).CombinedOutput(); err != nil {
			t.Errorf("pwsh could not load script: %v\n%s", err, out)
		}
	}
}

func TestWriteCompletionDoesNotMutateInputs(t *testing.T) {
	root, parsers := completionFixture(t)
	help1 := helpText(t, root, parsers)
	generateCompletion(t, "bash", root, parsers)
	generateCompletion(t, "zsh", root, parsers)
	generateCompletion(t, "fish", root, parsers)
	generateCompletion(t, "powershell", root, parsers)
	help2 := helpText(t, root, parsers)
	if help1 != help2 {
		t.Errorf("tree changed by completion generation:\n%s\n---\n%s", help1, help2)
	}
}

func helpText(t *testing.T, root *Node, parsers map[*Node]*Parser) string {
	t.Helper()
	var b strings.Builder
	var walk func(n *Node, path []string)
	walk = func(n *Node, path []string) {
		if err := root.WriteHelp(path, parsers[n], &b); err != nil {
			t.Fatalf("WriteHelp: %v", err)
		}
		for _, c := range n.children {
			walk(c, append(append([]string(nil), path...), c.name))
		}
	}
	walk(root, nil)
	return b.String()
}

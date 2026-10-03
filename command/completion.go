package command

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// This file adds shell completion script generation to the command tree.
// Generation is a pure function of the frozen tree and the parser map: it
// never runs handlers, never starts a shell or the target program, and
// never touches the environment, the network, or the filesystem. The
// script is fully rendered before the first byte reaches the writer.

// ErrUnsupportedShell indicates a shell name outside the supported set
// ("bash", "zsh", "fish", "powershell"). It is wrapped by *ShellError
// carrying the offending value.
var ErrUnsupportedShell = errors.New("command: unsupported shell")

// ShellError wraps ErrUnsupportedShell and reports the shell name that was
// rejected.
type ShellError struct {
	// Shell is the unsupported shell name as supplied by the caller.
	Shell string
}

func (e *ShellError) Error() string {
	return fmt.Sprintf("command: unsupported shell %q", e.Shell)
}

// Unwrap exposes ErrUnsupportedShell for errors.Is and errors.As.
func (e *ShellError) Unwrap() error { return ErrUnsupportedShell }

// WriteCompletion generates a shell completion script for the tree rooted
// at n and writes it to w.
//
// shell must be one of "bash", "zsh", "fish" or "powershell"; any other
// value yields a *ShellError wrapping ErrUnsupportedShell and no byte is
// written. The root's primary name must satisfy the same rules Add
// enforces; otherwise a *NameError wrapping ErrInvalidName is returned and
// no byte is written. A write failure is returned unchanged.
//
// parsers associates nodes of the tree with the parser a handler would use
// for that node's arguments; it may be nil or empty, and entries for nodes
// that are not part of this tree are ignored. The tree, the parsers, the
// map, and all caller data are only read, never modified, and concurrent
// calls against the same frozen inputs are safe.
//
// The generated script is native to the target shell, loadable directly
// without third-party tools, and registers completion for the root's
// primary name. Once loaded, the completion walks the words before the
// cursor one at a time: a word equal to a child's primary name or alias
// (exact, case-sensitive) descends into that child; an unknown word stops
// framework completion entirely instead of skipping ahead or adopting a
// sibling's parser. While the current node has children, candidates are
// the primary name and every alias of each child in Add order, each
// carrying the child's description where the shell can display one (bash
// cannot). Only at a leaf node does the associated parser contribute its
// long and short options, in declaration order. Candidates are always
// filtered by the current word as a case-sensitive prefix.
//
// Option tracking mirrors (*Parser).Parse: boolean options never consume
// the next word, while other options take their value from the next word,
// from a "--name=value" suffix, or from the attached remainder of a short
// option cluster. A word awaited as an option value suppresses all command
// and option candidates. A non-repeatable option already present on the
// command line is no longer suggested; a repeatable one still is. A
// standalone "--" ends framework completion, positional arguments (fixed
// or variadic) never produce invented values, and whenever the framework
// has no candidates the shell's own default completion (e.g. file names)
// remains in effect.
//
// All names, aliases, and descriptions are escaped for the target shell,
// so quotes, backslashes, newlines, and shell metacharacters in the tree
// cannot alter the script's meaning. For the same frozen tree, parser map,
// and shell the output is byte-stable, uses LF line endings only, and ends
// with exactly one LF.
func (n *Node) WriteCompletion(shell string, parsers map[*Node]*Parser, w io.Writer) error {
	var render func(string, []completionNode) string
	switch shell {
	case "bash":
		render = renderBashCompletion
	case "zsh":
		render = renderZshCompletion
	case "fish":
		render = renderFishCompletion
	case "powershell":
		render = renderPowerShellCompletion
	default:
		return &ShellError{Shell: shell}
	}
	if !validName(n.name) {
		return &NameError{Name: n.name}
	}
	script := render(n.name, buildCompletionModel(n, parsers))
	_, err := io.WriteString(w, script)
	return err
}

// completionChildGroup holds the candidate names of one child command: the
// primary name followed by its aliases, all resolving to target.
type completionChildGroup struct {
	names  []string
	desc   string
	target int
}

// completionOption is the completion-relevant projection of one Option.
type completionOption struct {
	long       string
	short      string
	isBool     bool
	repeatable bool
}

// completionNode is the completion-relevant projection of one tree node.
// groups is empty for leaf nodes; options is empty for internal nodes and
// for leaves without an associated parser.
type completionNode struct {
	groups  []completionChildGroup
	options []completionOption
}

// buildCompletionModel flattens the tree rooted at root into a slice
// indexed by node id, assigned in depth-first pre-order so the output is
// deterministic. Only reads are performed on the tree, the parsers, and
// the map.
func buildCompletionModel(root *Node, parsers map[*Node]*Parser) []completionNode {
	ids := make(map[*Node]int)
	var order []*Node
	var walk func(n *Node)
	walk = func(n *Node) {
		ids[n] = len(order)
		order = append(order, n)
		for _, c := range n.children {
			walk(c)
		}
	}
	walk(root)

	nodes := make([]completionNode, len(order))
	for i, n := range order {
		for _, c := range n.children {
			group := completionChildGroup{
				names:  append([]string{c.name}, c.aliases...),
				desc:   c.desc,
				target: ids[c],
			}
			nodes[i].groups = append(nodes[i].groups, group)
		}
		if len(n.children) > 0 {
			continue
		}
		if p := parsers[n]; p != nil {
			for j := range p.options {
				co := &p.options[j]
				nodes[i].options = append(nodes[i].options, completionOption{
					long:       co.Long,
					short:      co.Short,
					isBool:     co.Type == TypeBool,
					repeatable: co.Repeatable,
				})
			}
		}
	}
	return nodes
}

// escapePOSIXSingle quotes s as a single-quoted string for bash and zsh.
// Newlines stay literal inside the quotes.
func escapePOSIXSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// escapeFishSingle quotes s as a fish single-quoted string, where only
// backslash and single quote are escapable.
func escapeFishSingle(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
}

// escapePowerShellSingle quotes s as a PowerShell single-quoted string,
// where a single quote is escaped by doubling it.
func escapePowerShellSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// renderBashCompletion renders the bash script. Descriptions are omitted:
// plain programmable completion has no way to display them.
func renderBashCompletion(root string, nodes []completionNode) string {
	fn := "__" + root + "_complete"
	var b strings.Builder
	fmt.Fprintf(&b, "# bash completion for %s, generated by the shellsmith command package.\n", root)
	b.WriteString("# Source this file to register completion for the command.\n")
	fmt.Fprintf(&b, "%s() {\n", fn)
	b.WriteString("    local cur word lname ch\n")
	b.WriteString("    local node=0 expect_value=0 after_ddash=0 dead=0 blocked=0\n")
	b.WriteString("    local used=\" \"\n")
	b.WriteString("    local -a candidates\n")
	b.WriteString("    local i j\n")
	b.WriteString("    candidates=()\n")
	b.WriteString("    cur=\"${COMP_WORDS[COMP_CWORD]}\"\n")
	b.WriteString("    for (( i = 1; i < COMP_CWORD; i++ )); do\n")
	b.WriteString("        word=\"${COMP_WORDS[i]}\"\n")
	b.WriteString("        if (( expect_value )); then\n")
	b.WriteString("            expect_value=0\n")
	b.WriteString("            continue\n")
	b.WriteString("        fi\n")
	b.WriteString("        if (( after_ddash || dead )); then\n")
	b.WriteString("            continue\n")
	b.WriteString("        fi\n")
	b.WriteString("        case \"$node\" in\n")
	for id := range nodes {
		writeCaseWalkArm(&b, id, nodes[id])
	}
	b.WriteString("        esac\n")
	b.WriteString("    done\n")
	b.WriteString("    if (( dead || expect_value || after_ddash )); then\n")
	b.WriteString("        blocked=1\n")
	b.WriteString("    fi\n")
	b.WriteString("    if (( ! blocked )); then\n")
	b.WriteString("        case \"$node\" in\n")
	for id := range nodes {
		writeBashCandidateArm(&b, id, nodes[id])
	}
	b.WriteString("        esac\n")
	b.WriteString("    fi\n")
	b.WriteString("    COMPREPLY=(\"${candidates[@]}\")\n")
	b.WriteString("}\n")
	fmt.Fprintf(&b, "complete -o default -F %s %s\n", fn, root)
	return b.String()
}

// writeCaseWalkArm emits the per-node word-consumption arm shared by the
// bash and zsh scripts: internal nodes match child names, leaf nodes with
// a parser track option usage and value contexts.
func writeCaseWalkArm(b *strings.Builder, id int, nd completionNode) {
	if len(nd.groups) > 0 {
		fmt.Fprintf(b, "            %d)\n", id)
		b.WriteString("                case \"$word\" in\n")
		for _, g := range nd.groups {
			fmt.Fprintf(b, "                    %s)\n", strings.Join(g.names, "|"))
			fmt.Fprintf(b, "                        node=%d\n", g.target)
			b.WriteString("                        ;;\n")
		}
		b.WriteString("                    *)\n")
		b.WriteString("                        dead=1\n")
		b.WriteString("                        ;;\n")
		b.WriteString("                esac\n")
		b.WriteString("                ;;\n")
		return
	}
	if len(nd.options) == 0 {
		return
	}
	fmt.Fprintf(b, "            %d)\n", id)
	b.WriteString("                case \"$word\" in\n")
	b.WriteString("                    --)\n")
	b.WriteString("                        after_ddash=1\n")
	b.WriteString("                        ;;\n")
	b.WriteString("                    --?*)\n")
	b.WriteString("                        lname=\"${word%%=*}\"\n")
	b.WriteString("                        case \"$lname\" in\n")
	for _, o := range nd.options {
		fmt.Fprintf(b, "                            --%s)\n", o.long)
		if !o.repeatable {
			fmt.Fprintf(b, "                                used=\"${used}%s \"\n", o.long)
		}
		if !o.isBool {
			b.WriteString("                                if [[ \"$word\" != *=* ]]; then\n")
			b.WriteString("                                    expect_value=1\n")
			b.WriteString("                                fi\n")
		}
		b.WriteString("                                ;;\n")
	}
	b.WriteString("                        esac\n")
	b.WriteString("                        ;;\n")
	if hasShortOptions(nd) {
		b.WriteString("                    -?*)\n")
		b.WriteString("                        for (( j = 1; j < ${#word}; j++ )); do\n")
		b.WriteString("                            ch=\"${word:$j:1}\"\n")
		b.WriteString("                            case \"$ch\" in\n")
		for _, o := range nd.options {
			if o.short == "" {
				continue
			}
			fmt.Fprintf(b, "                                %s)\n", o.short)
			if !o.repeatable {
				fmt.Fprintf(b, "                                    used=\"${used}%s \"\n", o.long)
			}
			if !o.isBool {
				b.WriteString("                                    if (( j + 1 == ${#word} )); then\n")
				b.WriteString("                                        expect_value=1\n")
				b.WriteString("                                    fi\n")
				b.WriteString("                                    break\n")
			}
			b.WriteString("                                    ;;\n")
		}
		b.WriteString("                                *)\n")
		b.WriteString("                                    break\n")
		b.WriteString("                                    ;;\n")
		b.WriteString("                            esac\n")
		b.WriteString("                        done\n")
		b.WriteString("                        ;;\n")
	}
	b.WriteString("                esac\n")
	b.WriteString("                ;;\n")
}

func hasShortOptions(nd completionNode) bool {
	for _, o := range nd.options {
		if o.short != "" {
			return true
		}
	}
	return false
}

// writeBashCandidateArm emits the per-node candidate block for bash.
func writeBashCandidateArm(b *strings.Builder, id int, nd completionNode) {
	if len(nd.groups) == 0 && len(nd.options) == 0 {
		return
	}
	fmt.Fprintf(b, "            %d)\n", id)
	for _, g := range nd.groups {
		for _, name := range g.names {
			fmt.Fprintf(b, "                if [[ \"%s\" == \"$cur\"* ]]; then\n", name)
			fmt.Fprintf(b, "                    candidates+=(\"%s\")\n", name)
			b.WriteString("                fi\n")
		}
	}
	for _, o := range nd.options {
		writeBashOptionCandidate(b, o, "--"+o.long)
		if o.short != "" {
			writeBashOptionCandidate(b, o, "-"+o.short)
		}
	}
	b.WriteString("                ;;\n")
}

func writeBashOptionCandidate(b *strings.Builder, o completionOption, spelling string) {
	guard := ""
	if !o.repeatable {
		guard = fmt.Sprintf("[[ \"$used\" != *\" %s \"* ]] && ", o.long)
	}
	fmt.Fprintf(b, "                if %s[[ \"%s\" == \"$cur\"* ]]; then\n", guard, spelling)
	fmt.Fprintf(b, "                    candidates+=(\"%s\")\n", spelling)
	b.WriteString("                fi\n")
}

// renderZshCompletion renders the zsh script. The walking arms are
// syntactically identical to bash; candidate emission carries descriptions
// through compadd.
func renderZshCompletion(root string, nodes []completionNode) string {
	fn := "__" + root + "_complete"
	var b strings.Builder
	fmt.Fprintf(&b, "#compdef %s\n", root)
	fmt.Fprintf(&b, "# zsh completion for %s, generated by the shellsmith command package.\n", root)
	b.WriteString("# Source this file (or place it in a directory in $fpath) to register completion.\n")
	fmt.Fprintf(&b, "%s() {\n", fn)
	b.WriteString("    local cur word lname ch\n")
	b.WriteString("    local node=0 expect_value=0 after_ddash=0 dead=0 blocked=0\n")
	b.WriteString("    local used=\" \"\n")
	b.WriteString("    local -a candidates descriptions\n")
	b.WriteString("    local i j\n")
	b.WriteString("    candidates=()\n")
	b.WriteString("    descriptions=()\n")
	b.WriteString("    cur=\"$PREFIX\"\n")
	b.WriteString("    for (( i = 2; i < CURRENT; i++ )); do\n")
	b.WriteString("        word=\"${words[i]}\"\n")
	b.WriteString("        if (( expect_value )); then\n")
	b.WriteString("            expect_value=0\n")
	b.WriteString("            continue\n")
	b.WriteString("        fi\n")
	b.WriteString("        if (( after_ddash || dead )); then\n")
	b.WriteString("            continue\n")
	b.WriteString("        fi\n")
	b.WriteString("        case \"$node\" in\n")
	for id := range nodes {
		writeCaseWalkArm(&b, id, nodes[id])
	}
	b.WriteString("        esac\n")
	b.WriteString("    done\n")
	b.WriteString("    if (( dead || expect_value || after_ddash )); then\n")
	b.WriteString("        blocked=1\n")
	b.WriteString("    fi\n")
	b.WriteString("    if (( ! blocked )); then\n")
	b.WriteString("        case \"$node\" in\n")
	for id := range nodes {
		writeZshCandidateArm(&b, id, nodes[id])
	}
	b.WriteString("        esac\n")
	b.WriteString("    fi\n")
	b.WriteString("    if (( ${#candidates[@]} > 0 )); then\n")
	b.WriteString("        compadd -V unsorted -d descriptions -- \"${candidates[@]}\"\n")
	b.WriteString("        return 0\n")
	b.WriteString("    fi\n")
	b.WriteString("    _files\n")
	b.WriteString("}\n")
	b.WriteString("if (( $+functions[compdef] )); then\n")
	fmt.Fprintf(&b, "    compdef %s %s\n", fn, root)
	b.WriteString("fi\n")
	return b.String()
}

// writeZshCandidateArm emits the per-node candidate block for zsh, filling
// the candidates and descriptions arrays in parallel.
func writeZshCandidateArm(b *strings.Builder, id int, nd completionNode) {
	if len(nd.groups) == 0 && len(nd.options) == 0 {
		return
	}
	fmt.Fprintf(b, "            %d)\n", id)
	for _, g := range nd.groups {
		for _, name := range g.names {
			fmt.Fprintf(b, "                if [[ \"%s\" == \"$cur\"* ]]; then\n", name)
			fmt.Fprintf(b, "                    candidates+=(\"%s\")\n", name)
			fmt.Fprintf(b, "                    descriptions+=(%s)\n", escapePOSIXSingle(g.desc))
			b.WriteString("                fi\n")
		}
	}
	for _, o := range nd.options {
		writeZshOptionCandidate(b, o, "--"+o.long)
		if o.short != "" {
			writeZshOptionCandidate(b, o, "-"+o.short)
		}
	}
	b.WriteString("                ;;\n")
}

func writeZshOptionCandidate(b *strings.Builder, o completionOption, spelling string) {
	guard := ""
	if !o.repeatable {
		guard = fmt.Sprintf("[[ \"$used\" != *\" %s \"* ]] && ", o.long)
	}
	fmt.Fprintf(b, "                if %s[[ \"%s\" == \"$cur\"* ]]; then\n", guard, spelling)
	fmt.Fprintf(b, "                    candidates+=(\"%s\")\n", spelling)
	b.WriteString("                    descriptions+=('')\n")
	b.WriteString("                fi\n")
}

// renderFishCompletion renders the fish script: a helper function prints
// one "candidate\tdescription" line per match, and a single complete
// registration invokes it. File completion stays enabled so the shell
// default applies whenever the framework has no candidates.
func renderFishCompletion(root string, nodes []completionNode) string {
	fn := "__" + root + "_complete"
	var b strings.Builder
	fmt.Fprintf(&b, "# fish completion for %s, generated by the shellsmith command package.\n", root)
	b.WriteString("# Source this file (or place it in $__fish_config_dir/completions) to register completion.\n")
	fmt.Fprintf(&b, "function %s\n", fn)
	b.WriteString("    set -l tokens (commandline -opc)\n")
	b.WriteString("    set -l cur (commandline -ct)\n")
	b.WriteString("    set -l node 0\n")
	b.WriteString("    set -l expect_value 0\n")
	b.WriteString("    set -l after_ddash 0\n")
	b.WriteString("    set -l dead 0\n")
	b.WriteString("    set -l used ' '\n")
	b.WriteString("    set -l n (count $tokens)\n")
	b.WriteString("    if test $n -ge 2\n")
	b.WriteString("        for i in (seq 2 $n)\n")
	b.WriteString("            set -l word $tokens[$i]\n")
	b.WriteString("            if test $expect_value -eq 1\n")
	b.WriteString("                set expect_value 0\n")
	b.WriteString("                continue\n")
	b.WriteString("            end\n")
	b.WriteString("            if test $after_ddash -eq 1; or test $dead -eq 1\n")
	b.WriteString("                continue\n")
	b.WriteString("            end\n")
	b.WriteString("            switch $node\n")
	for id := range nodes {
		writeFishWalkArm(&b, id, nodes[id])
	}
	b.WriteString("            end\n")
	b.WriteString("        end\n")
	b.WriteString("    end\n")
	b.WriteString("    set -l out\n")
	b.WriteString("    if test $expect_value -eq 0; and test $after_ddash -eq 0; and test $dead -eq 0\n")
	b.WriteString("        set -l curlen (string length -- \"$cur\")\n")
	b.WriteString("        switch $node\n")
	for id := range nodes {
		writeFishCandidateArm(&b, id, nodes[id])
	}
	b.WriteString("        end\n")
	b.WriteString("    end\n")
	b.WriteString("    for line in $out\n")
	b.WriteString("        printf '%s\\n' \"$line\"\n")
	b.WriteString("    end\n")
	b.WriteString("end\n")
	fmt.Fprintf(&b, "complete -c %s -a '(%s)'\n", root, fn)
	return b.String()
}

// writeFishWalkArm emits the per-node word-consumption arm for fish. The
// fish case builtin is only given patterns that cannot start with a dash;
// option-shaped words are classified with string match instead.
func writeFishWalkArm(b *strings.Builder, id int, nd completionNode) {
	if len(nd.groups) > 0 {
		fmt.Fprintf(b, "                case %d\n", id)
		b.WriteString("                    switch \"$word\"\n")
		for _, g := range nd.groups {
			fmt.Fprintf(b, "                        case %s\n", strings.Join(g.names, " "))
			fmt.Fprintf(b, "                            set node %d\n", g.target)
		}
		b.WriteString("                        case '*'\n")
		b.WriteString("                            set dead 1\n")
		b.WriteString("                    end\n")
		return
	}
	if len(nd.options) == 0 {
		return
	}
	fmt.Fprintf(b, "                case %d\n", id)
	b.WriteString("                    if string match -q -- '--' \"$word\"\n")
	b.WriteString("                        set after_ddash 1\n")
	b.WriteString("                    else if string match -q -- '--*' \"$word\"\n")
	b.WriteString("                        set -l parts (string split -m 1 = -- \"$word\")\n")
	b.WriteString("                        set -l lname $parts[1]\n")
	for i, o := range nd.options {
		keyword := "if"
		if i > 0 {
			keyword = "else if"
		}
		fmt.Fprintf(b, "                        %s test \"$lname\" = '--%s'\n", keyword, o.long)
		writeFishOptionEffect(b, o, "                            ", "long")
	}
	b.WriteString("                        end\n")
	if hasShortOptions(nd) {
		b.WriteString("                    else if string match -q -- '-*' \"$word\"\n")
		b.WriteString("                        set -l wlen (string length -- \"$word\")\n")
		b.WriteString("                        set -l j 1\n")
		b.WriteString("                        while test $j -lt $wlen\n")
		b.WriteString("                            set j (math $j + 1)\n")
		b.WriteString("                            set -l ch (string sub -s $j -l 1 -- \"$word\")\n")
		first := true
		for _, o := range nd.options {
			if o.short == "" {
				continue
			}
			keyword := "if"
			if !first {
				keyword = "else if"
			}
			first = false
			fmt.Fprintf(b, "                            %s test \"$ch\" = '%s'\n", keyword, o.short)
			writeFishOptionEffect(b, o, "                                ", "short")
		}
		b.WriteString("                            else\n")
		b.WriteString("                                break\n")
		b.WriteString("                            end\n")
		b.WriteString("                        end\n")
	}
	b.WriteString("                    end\n")
}

// writeFishOptionEffect emits the state updates for one matched option in
// the fish walking logic. kind is "long" or "short" and selects how a
// value-taking option consumes its value.
func writeFishOptionEffect(b *strings.Builder, o completionOption, indent, kind string) {
	emitted := false
	if !o.repeatable {
		fmt.Fprintf(b, "%sset used \"$used\"'%s '\n", indent, o.long)
		emitted = true
	}
	if !o.isBool {
		if kind == "long" {
			b.WriteString(indent + "if not string match -q -- '*=*' \"$word\"\n")
			b.WriteString(indent + "    set expect_value 1\n")
			b.WriteString(indent + "end\n")
		} else {
			b.WriteString(indent + "if test $j -eq $wlen\n")
			b.WriteString(indent + "    set expect_value 1\n")
			b.WriteString(indent + "end\n")
			b.WriteString(indent + "break\n")
		}
		emitted = true
	}
	if !emitted {
		// A repeatable boolean needs no bookkeeping, but a fish if body
		// must not be empty.
		b.WriteString(indent + "true\n")
	}
}

// writeFishCandidateArm emits the per-node candidate block for fish.
func writeFishCandidateArm(b *strings.Builder, id int, nd completionNode) {
	if len(nd.groups) == 0 && len(nd.options) == 0 {
		return
	}
	fmt.Fprintf(b, "            case %d\n", id)
	for _, g := range nd.groups {
		for _, name := range g.names {
			line := escapeFishSingle(name)
			if g.desc != "" {
				line = escapeFishSingle(name + "\t" + g.desc)
			}
			writeFishCandidate(b, "                ", name, line)
		}
	}
	for _, o := range nd.options {
		writeFishOptionCandidate(b, o, "--"+o.long)
		if o.short != "" {
			writeFishOptionCandidate(b, o, "-"+o.short)
		}
	}
}

func writeFishCandidate(b *strings.Builder, indent, cand, line string) {
	fmt.Fprintf(b, "%sset -l p (string sub -s 1 -l $curlen -- '%s')\n", indent, cand)
	fmt.Fprintf(b, "%sif test \"$p\" = \"$cur\"\n", indent)
	fmt.Fprintf(b, "%s    set out $out %s\n", indent, line)
	fmt.Fprintf(b, "%send\n", indent)
}

func writeFishOptionCandidate(b *strings.Builder, o completionOption, spelling string) {
	line := escapeFishSingle(spelling)
	if o.repeatable {
		writeFishCandidate(b, "                ", spelling, line)
		return
	}
	fmt.Fprintf(b, "                if not string match -q -- '* %s *' \"$used\"\n", o.long)
	writeFishCandidate(b, "                    ", spelling, line)
	b.WriteString("                end\n")
}

// renderPowerShellCompletion renders the PowerShell script. All data lives
// inside the registered ScriptBlock so no outer scope is required.
func renderPowerShellCompletion(root string, nodes []completionNode) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# powershell completion for %s, generated by the shellsmith command package.\n", root)
	b.WriteString("# Dot-source this file to register completion for the command.\n")
	fmt.Fprintf(&b, "Register-ArgumentCompleter -Native -CommandName %s -ScriptBlock {\n", escapePowerShellSingle(root))
	b.WriteString("    param($wordToComplete, $commandAst, $cursorPosition)\n")
	b.WriteString("    $children = @{\n")
	for id, nd := range nodes {
		if len(nd.groups) == 0 {
			continue
		}
		fmt.Fprintf(&b, "        %d = @(\n", id)
		for _, g := range nd.groups {
			for _, name := range g.names {
				fmt.Fprintf(&b, "            ,@(%s, %s, %d)\n",
					escapePowerShellSingle(name), escapePowerShellSingle(g.desc), g.target)
			}
		}
		b.WriteString("        )\n")
	}
	b.WriteString("    }\n")
	b.WriteString("    $options = @{\n")
	for id, nd := range nodes {
		if len(nd.options) == 0 {
			continue
		}
		fmt.Fprintf(&b, "        %d = @(\n", id)
		for _, o := range nd.options {
			fmt.Fprintf(&b, "            ,@(%s, %s, $%s, $%s)\n",
				escapePowerShellSingle(o.long), escapePowerShellSingle(o.short),
				powerShellBool(o.isBool), powerShellBool(o.repeatable))
		}
		b.WriteString("        )\n")
	}
	b.WriteString("    }\n")
	b.WriteString(powerShellLogic)
	b.WriteString("}\n")
	return b.String()
}

func powerShellBool(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// powerShellLogic is the fixed runtime of the PowerShell completer. It
// mirrors the walking and candidate rules of the other shells.
const powerShellLogic = `    $cur = [string]$wordToComplete
    $prev = @()
    $elements = $commandAst.CommandElements
    for ($i = 1; $i -lt $elements.Count; $i++) {
        $prev += @([string]$elements[$i].Extent.Text)
    }
    if (($cur -ne '') -and ($prev.Count -gt 0)) {
        if ($prev.Count -eq 1) {
            $prev = @()
        } else {
            $prev = @($prev[0..($prev.Count - 2)])
        }
    }
    $node = 0
    $expectValue = $false
    $afterDdash = $false
    $dead = $false
    $used = @{}
    foreach ($word in $prev) {
        if ($expectValue) {
            $expectValue = $false
            continue
        }
        if ($afterDdash -or $dead) {
            continue
        }
        if ($children.ContainsKey($node)) {
            $next = -1
            foreach ($group in $children[$node]) {
                if ($group[0] -ceq $word) {
                    $next = $group[2]
                    break
                }
            }
            if ($next -lt 0) {
                $dead = $true
            } else {
                $node = $next
            }
            continue
        }
        if (-not $options.ContainsKey($node)) {
            continue
        }
        $opts = $options[$node]
        if ($word -ceq '--') {
            $afterDdash = $true
            continue
        }
        if ($word.StartsWith('--')) {
            $lname = $word
            $eq = $word.IndexOf('=')
            if ($eq -ge 0) {
                $lname = $word.Substring(0, $eq)
            }
            $lname = $lname.Substring(2)
            foreach ($o in $opts) {
                if ($o[0] -ceq $lname) {
                    if (-not $o[3]) {
                        $used[$o[0]] = $true
                    }
                    if ((-not $o[2]) -and ($eq -lt 0)) {
                        $expectValue = $true
                    }
                    break
                }
            }
            continue
        }
        if (($word.Length -gt 1) -and $word.StartsWith('-')) {
            $stop = $false
            for ($j = 1; ($j -lt $word.Length) -and (-not $stop); $j++) {
                $ch = $word.Substring($j, 1)
                $known = $false
                foreach ($o in $opts) {
                    if (($o[1] -ne '') -and ($o[1] -ceq $ch)) {
                        $known = $true
                        if (-not $o[3]) {
                            $used[$o[0]] = $true
                        }
                        if (-not $o[2]) {
                            if ($j -eq ($word.Length - 1)) {
                                $expectValue = $true
                            }
                            $stop = $true
                        }
                        break
                    }
                }
                if (-not $known) {
                    break
                }
            }
        }
    }
    $results = @()
    if ((-not $dead) -and (-not $expectValue) -and (-not $afterDdash)) {
        if ($children.ContainsKey($node)) {
            foreach ($group in $children[$node]) {
                if ($group[0].StartsWith($cur, [System.StringComparison]::Ordinal)) {
                    $results += [System.Management.Automation.CompletionResult]::new($group[0], $group[0], 'ParameterValue', $group[1])
                }
            }
        } elseif ($options.ContainsKey($node)) {
            foreach ($o in $options[$node]) {
                if ($o[3] -or (-not $used.ContainsKey($o[0]))) {
                    $long = '--' + $o[0]
                    if ($long.StartsWith($cur, [System.StringComparison]::Ordinal)) {
                        $results += [System.Management.Automation.CompletionResult]::new($long, $long, 'ParameterName', $long)
                    }
                    if ($o[1] -ne '') {
                        $short = '-' + $o[1]
                        if ($short.StartsWith($cur, [System.StringComparison]::Ordinal)) {
                            $results += [System.Management.Automation.CompletionResult]::new($short, $short, 'ParameterName', $short)
                        }
                    }
                }
            }
        }
    }
    return $results
`

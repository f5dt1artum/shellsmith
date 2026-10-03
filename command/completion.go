package command

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// This file adds shell completion script generation to the command tree.
// Generation is a pure function of the frozen inputs: it never runs
// handlers, never starts a shell or the target program, and never reads
// the environment, the network, or the file system. The tree, the
// parsers, the parser map, and every caller-owned value are only read,
// so concurrent calls against the same frozen inputs are safe.

// ErrUnsupportedShell indicates a shell name outside the supported set
// ("bash", "zsh", "fish", "powershell"). It is wrapped by *ShellError
// carrying the offending value.
var ErrUnsupportedShell = errors.New("command: unsupported shell")

// ShellError wraps ErrUnsupportedShell and reports the shell name that
// was rejected.
type ShellError struct {
	// Shell is the unsupported shell name as supplied by the caller.
	Shell string
}

func (e *ShellError) Error() string {
	return fmt.Sprintf("command: unsupported shell %q", e.Shell)
}

// Unwrap exposes ErrUnsupportedShell for errors.Is and errors.As.
func (e *ShellError) Unwrap() error { return ErrUnsupportedShell }

// WriteCompletion writes a shell completion script for the command tree
// rooted at n to w. shell must be one of "bash", "zsh", "fish" or
// "powershell" (exact, lowercase); any other value yields a *ShellError
// wrapping ErrUnsupportedShell and no byte reaches w. If the root's
// primary name violates the command name rules, a *NameError wrapping
// ErrInvalidName is returned and no byte reaches w.
//
// parsers associates nodes of the tree with their argument parsers; it
// may be nil or empty. Entries keyed by nodes that do not belong to the
// tree are ignored. Nodes without an associated parser still complete
// their subcommands.
//
// The generated script registers completion for the root's primary name.
// When loaded by the corresponding shell it resolves the words before
// the cursor level by level: primary names and aliases both descend into
// subcommands, and candidates are each child's primary name and aliases
// in Add order, carrying the child's description where the shell can
// display one (all but bash). Matching is case-sensitive and restricted
// to candidates carrying the current prefix. An unknown intermediate
// command word stops framework completion instead of skipping deeper or
// borrowing a sibling's parser.
//
// Only once a leaf node is reached does the script offer the long and
// short options of that node's parser. A non-repeatable option that
// already appears on the line is not suggested again; repeatable options
// always remain available. Boolean options do not consume the next word;
// other options are recognized with a separated value, a "--name=value"
// value, an attached short value, and inside short option clusters.
// While an option value is expected, no command or option candidates are
// produced. A standalone "--" stops framework completion, and positional
// arguments (fixed or variadic) never produce invented values — in all
// these cases the shell's own default completion applies.
//
// The scripts are native to each shell and need no third-party tools.
// All visible text from the tree is escaped for the target shell, so
// quotes, backslashes, newlines, and shell metacharacters in names or
// descriptions cannot change the script's meaning. For the same frozen
// tree, parser map, and shell the output is byte-stable, uses LF line
// endings only, and ends with a single LF. Validation and rendering
// complete before anything is written; a write failure is returned
// unchanged. WriteCompletion never modifies the tree, the parsers, the
// map, or any caller data.
func (n *Node) WriteCompletion(shell string, parsers map[*Node]*Parser, w io.Writer) error {
	var render func(*completionModel) string
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
	script := render(buildCompletionModel(n, parsers))
	_, err := io.WriteString(w, script)
	return err
}

// completionOption is the completion-relevant projection of one Option.
type completionOption struct {
	long       string
	short      string
	boolean    bool
	repeatable bool
}

// completionChild describes one child edge of a node.
type completionChild struct {
	names  []string // primary name followed by aliases, registration order
	desc   string
	target int // index of the child in completionModel.nodes
}

// completionNode is the completion-relevant projection of one Node.
type completionNode struct {
	children []completionChild  // Add order; empty for leaves
	options  []completionOption // declaration order; leaves with a parser only
}

// completionModel is an immutable, index-addressed snapshot of the tree.
type completionModel struct {
	root  string
	ident string // root name sanitized for shell identifiers
	nodes []completionNode
}

// buildCompletionModel snapshots the tree in preorder. The root is
// index 0. Parsers are consulted only for leaf nodes; map entries for
// foreign nodes are never touched.
func buildCompletionModel(root *Node, parsers map[*Node]*Parser) *completionModel {
	m := &completionModel{
		root:  root.name,
		ident: "_" + strings.ReplaceAll(root.name, "-", "_"),
	}
	var walk func(n *Node) int
	walk = func(n *Node) int {
		idx := len(m.nodes)
		m.nodes = append(m.nodes, completionNode{})
		for _, child := range n.children {
			target := walk(child)
			names := make([]string, 0, 1+len(child.aliases))
			names = append(names, child.name)
			names = append(names, child.aliases...)
			m.nodes[idx].children = append(m.nodes[idx].children, completionChild{
				names:  names,
				desc:   child.desc,
				target: target,
			})
		}
		if len(n.children) == 0 {
			if p := parsers[n]; p != nil {
				for i := range p.options {
					o := &p.options[i]
					m.nodes[idx].options = append(m.nodes[idx].options, completionOption{
						long:       o.Long,
						short:      o.Short,
						boolean:    o.Type == TypeBool,
						repeatable: o.Repeatable,
					})
				}
			}
		}
		return idx
	}
	walk(root)
	return m
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// kindLetter is the option-kind marker used in the generated data: "b"
// for boolean options, "v" for value-taking options.
func kindLetter(boolean bool) string {
	if boolean {
		return "b"
	}
	return "v"
}

// longNames collects the long names of the options of one kind, in
// declaration order.
func longNames(opts []completionOption, boolean bool) []string {
	var names []string
	for _, o := range opts {
		if o.boolean == boolean {
			names = append(names, o.long)
		}
	}
	return names
}

// escapeSingleQuoted escapes s for a single-quoted string in bash and
// zsh, where a single quote is written as '\” .
func escapeSingleQuoted(s string) string {
	return strings.ReplaceAll(s, "'", `'\''`)
}

// escapeFishSingle escapes s for a fish single-quoted string, where
// backslash and single quote are the only escaped characters.
func escapeFishSingle(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `'`, `\'`)
}

// escapePowerShellSingle escapes s for a PowerShell single-quoted
// string, where a single quote is doubled.
func escapePowerShellSingle(s string) string {
	return strings.ReplaceAll(s, `'`, `''`)
}

// replaceEngineTokens substitutes the __ID__ and __ROOT__ placeholders
// in a fixed engine template.
func (m *completionModel) replaceEngineTokens(engine string) string {
	return strings.NewReplacer("__ID__", m.ident, "__ROOT__", m.root).Replace(engine)
}

// writeBourneData emits the lookup functions shared by the bash and zsh
// scripts. Command and option names use the restricted name character
// set, so they are safe inside case patterns and single-quoted strings
// without further escaping.
func writeBourneData(b *strings.Builder, m *completionModel) {
	id := m.ident

	// _id_has_children reports via exit status whether a node has
	// children, i.e. whether the next word selects a subcommand.
	b.WriteString(id + "_has_children() {\n    case \"$1\" in\n")
	var inner []string
	for i := range m.nodes {
		if len(m.nodes[i].children) > 0 {
			inner = append(inner, strconv.Itoa(i))
		}
	}
	if len(inner) > 0 {
		fmt.Fprintf(b, "        %s) return 0 ;;\n", strings.Join(inner, "|"))
	}
	b.WriteString("        *) return 1 ;;\n    esac\n}\n\n")

	// _id_next prints the child index selected by a word, or nothing.
	b.WriteString(id + "_next() {\n    case \"$1\" in\n")
	for i := range m.nodes {
		nd := &m.nodes[i]
		if len(nd.children) == 0 {
			continue
		}
		fmt.Fprintf(b, "        %d)\n            case \"$2\" in\n", i)
		for _, ch := range nd.children {
			fmt.Fprintf(b, "                %s) printf '%%s\\n' %d ;;\n", strings.Join(ch.names, "|"), ch.target)
		}
		b.WriteString("            esac\n            ;;\n")
	}
	b.WriteString("    esac\n}\n\n")

	// _id_options prints one "long|short|bool|repeatable" spec per line.
	b.WriteString(id + "_options() {\n    case \"$1\" in\n")
	for i := range m.nodes {
		nd := &m.nodes[i]
		if len(nd.options) == 0 {
			continue
		}
		fmt.Fprintf(b, "        %d) printf '%%s\\n'", i)
		for _, o := range nd.options {
			fmt.Fprintf(b, " '%s|%s|%d|%d'", o.long, o.short, boolInt(o.boolean), boolInt(o.repeatable))
		}
		b.WriteString(" ;;\n")
	}
	b.WriteString("    esac\n}\n\n")

	// _id_long_kind prints b, v, or - for a long option name.
	b.WriteString(id + "_long_kind() {\n    case \"$1\" in\n")
	for i := range m.nodes {
		nd := &m.nodes[i]
		if len(nd.options) == 0 {
			continue
		}
		fmt.Fprintf(b, "        %d)\n            case \"$2\" in\n", i)
		if names := longNames(nd.options, true); len(names) > 0 {
			fmt.Fprintf(b, "                %s) printf '%%s\\n' b ;;\n", strings.Join(names, "|"))
		}
		if names := longNames(nd.options, false); len(names) > 0 {
			fmt.Fprintf(b, "                %s) printf '%%s\\n' v ;;\n", strings.Join(names, "|"))
		}
		b.WriteString("                *) printf '%s\\n' - ;;\n")
		b.WriteString("            esac\n            ;;\n")
	}
	b.WriteString("        *) printf '%s\\n' - ;;\n    esac\n}\n\n")

	// _id_short_info prints "long|b" or "long|v" for a short flag, or -.
	b.WriteString(id + "_short_info() {\n    case \"$1\" in\n")
	for i := range m.nodes {
		nd := &m.nodes[i]
		var shorts []completionOption
		for _, o := range nd.options {
			if o.short != "" {
				shorts = append(shorts, o)
			}
		}
		if len(shorts) == 0 {
			continue
		}
		fmt.Fprintf(b, "        %d)\n            case \"$2\" in\n", i)
		for _, o := range shorts {
			fmt.Fprintf(b, "                %s) printf '%%s\\n' '%s|%s' ;;\n", o.short, o.long, kindLetter(o.boolean))
		}
		b.WriteString("                *) printf '%s\\n' - ;;\n")
		b.WriteString("            esac\n            ;;\n")
	}
	b.WriteString("        *) printf '%s\\n' - ;;\n    esac\n}\n")
}

// renderBashCompletion renders the bash script. bash has no native
// mechanism to display candidate descriptions, so they are omitted.
func renderBashCompletion(m *completionModel) string {
	var b strings.Builder
	id := m.ident
	fmt.Fprintf(&b, "# bash completion for %s — generated by github.com/f5dt1artum/shellsmith command package.\n", m.root)
	b.WriteString("# Source this file or copy it into a bash_completion.d directory.\n\n")
	writeBourneData(&b, m)
	b.WriteString("\n")

	// _id_commands prints the candidate names of one node, one per line.
	b.WriteString(id + "_commands() {\n    case \"$1\" in\n")
	for i := range m.nodes {
		nd := &m.nodes[i]
		if len(nd.children) == 0 {
			continue
		}
		fmt.Fprintf(&b, "        %d) printf '%%s\\n'", i)
		for _, ch := range nd.children {
			for _, name := range ch.names {
				fmt.Fprintf(&b, " '%s'", name)
			}
		}
		b.WriteString(" ;;\n")
	}
	b.WriteString("    esac\n}\n\n")

	b.WriteString(m.replaceEngineTokens(bashCompletionEngine))
	return b.String()
}

// renderZshCompletion renders the zsh script.
func renderZshCompletion(m *completionModel) string {
	var b strings.Builder
	id := m.ident
	fmt.Fprintf(&b, "#compdef %s\n", m.root)
	fmt.Fprintf(&b, "# zsh completion for %s — generated by github.com/f5dt1artum/shellsmith command package.\n\n", m.root)
	writeBourneData(&b, m)
	b.WriteString("\n")

	// Candidate names and descriptions per node with children, aligned
	// by position; compadd -d displays the descriptions.
	for i := range m.nodes {
		nd := &m.nodes[i]
		if len(nd.children) == 0 {
			continue
		}
		var names, descs []string
		for _, ch := range nd.children {
			for _, name := range ch.names {
				names = append(names, "'"+escapeSingleQuoted(name)+"'")
				descs = append(descs, "'"+escapeSingleQuoted(ch.desc)+"'")
			}
		}
		fmt.Fprintf(&b, "%s_cmds_%d=(%s)\n", id, i, strings.Join(names, " "))
		fmt.Fprintf(&b, "%s_desc_%d=(%s)\n", id, i, strings.Join(descs, " "))
	}
	b.WriteString("\n")

	b.WriteString(m.replaceEngineTokens(zshCompletionEngine))
	return b.String()
}

// renderFishCompletion renders the fish script.
func renderFishCompletion(m *completionModel) string {
	var b strings.Builder
	id := m.ident
	fmt.Fprintf(&b, "# fish completion for %s — generated by github.com/f5dt1artum/shellsmith command package.\n\n", m.root)

	// Descriptions per node with children, aligned with the candidate
	// names printed by _id_commands. They live in variables because
	// descriptions may contain newlines, which cannot travel through
	// line-based command substitution.
	for i := range m.nodes {
		nd := &m.nodes[i]
		if len(nd.children) == 0 {
			continue
		}
		fmt.Fprintf(&b, "set -g %s_desc_%d", id, i)
		for _, ch := range nd.children {
			for range ch.names {
				fmt.Fprintf(&b, " '%s'", escapeFishSingle(ch.desc))
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")

	// _id_has_children reports via exit status whether a node has
	// children.
	b.WriteString("function " + id + "_has_children\n    switch $argv[1]\n")
	var inner []string
	for i := range m.nodes {
		if len(m.nodes[i].children) > 0 {
			inner = append(inner, strconv.Itoa(i))
		}
	}
	if len(inner) > 0 {
		fmt.Fprintf(&b, "        case %s\n            return 0\n", strings.Join(inner, " "))
	}
	b.WriteString("    end\n    return 1\nend\n\n")

	// _id_next prints the child index selected by a word, or nothing.
	b.WriteString("function " + id + "_next\n    switch $argv[1]\n")
	for i := range m.nodes {
		nd := &m.nodes[i]
		if len(nd.children) == 0 {
			continue
		}
		fmt.Fprintf(&b, "        case %d\n", i)
		for j, ch := range nd.children {
			keyword := "if"
			if j > 0 {
				keyword = "else if"
			}
			b.WriteString("            " + keyword + " contains -- $argv[2]")
			for _, name := range ch.names {
				fmt.Fprintf(&b, " '%s'", escapeFishSingle(name))
			}
			fmt.Fprintf(&b, "\n                printf '%%s\\n' %d\n", ch.target)
		}
		b.WriteString("            end\n")
	}
	b.WriteString("    end\nend\n\n")

	// _id_commands prints the candidate names of one node, one per line.
	b.WriteString("function " + id + "_commands\n    switch $argv[1]\n")
	for i := range m.nodes {
		nd := &m.nodes[i]
		if len(nd.children) == 0 {
			continue
		}
		fmt.Fprintf(&b, "        case %d\n            printf '%%s\\n'", i)
		for _, ch := range nd.children {
			for _, name := range ch.names {
				fmt.Fprintf(&b, " '%s'", escapeFishSingle(name))
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("    end\nend\n\n")

	// _id_options prints one "long|short|bool|repeatable" spec per line.
	b.WriteString("function " + id + "_options\n    switch $argv[1]\n")
	for i := range m.nodes {
		nd := &m.nodes[i]
		if len(nd.options) == 0 {
			continue
		}
		fmt.Fprintf(&b, "        case %d\n            printf '%%s\\n'", i)
		for _, o := range nd.options {
			fmt.Fprintf(&b, " '%s|%s|%d|%d'", o.long, o.short, boolInt(o.boolean), boolInt(o.repeatable))
		}
		b.WriteString("\n")
	}
	b.WriteString("    end\nend\n\n")

	// _id_long_kind prints b or v for a long option name, or nothing when
	// the name is unknown.
	b.WriteString("function " + id + "_long_kind\n    switch $argv[1]\n")
	for i := range m.nodes {
		nd := &m.nodes[i]
		if len(nd.options) == 0 {
			continue
		}
		fmt.Fprintf(&b, "        case %d\n", i)
		if names := longNames(nd.options, true); len(names) > 0 {
			b.WriteString("            if contains -- $argv[2]")
			for _, name := range names {
				fmt.Fprintf(&b, " '%s'", escapeFishSingle(name))
			}
			b.WriteString("\n                printf '%s\\n' b\n")
		}
		keyword := "if"
		if names := longNames(nd.options, false); len(names) > 0 {
			if len(longNames(nd.options, true)) > 0 {
				keyword = "else if"
			}
			b.WriteString("            " + keyword + " contains -- $argv[2]")
			for _, name := range names {
				fmt.Fprintf(&b, " '%s'", escapeFishSingle(name))
			}
			b.WriteString("\n                printf '%s\\n' v\n")
		}
		b.WriteString("            end\n")
	}
	b.WriteString("    end\nend\n\n")

	// _id_short_info prints "long|b" or "long|v" for a short flag, or
	// nothing when the flag is unknown.
	b.WriteString("function " + id + "_short_info\n    switch $argv[1]\n")
	for i := range m.nodes {
		nd := &m.nodes[i]
		var shorts []completionOption
		for _, o := range nd.options {
			if o.short != "" {
				shorts = append(shorts, o)
			}
		}
		if len(shorts) == 0 {
			continue
		}
		fmt.Fprintf(&b, "        case %d\n            switch $argv[2]\n", i)
		for _, o := range shorts {
			fmt.Fprintf(&b, "                case '%s'\n                    printf '%%s\\n' '%s|%s'\n", escapeFishSingle(o.short), escapeFishSingle(o.long), kindLetter(o.boolean))
		}
		b.WriteString("            end\n")
	}
	b.WriteString("    end\nend\n\n")

	b.WriteString(m.replaceEngineTokens(fishCompletionEngine))
	return b.String()
}

// renderPowerShellCompletion renders the PowerShell script.
func renderPowerShellCompletion(m *completionModel) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# powershell completion for %s — generated by github.com/f5dt1artum/shellsmith command package.\n", m.root)
	fmt.Fprintf(&b, "Register-ArgumentCompleter -Native -CommandName '%s' -ScriptBlock {\n", escapePowerShellSingle(m.root))
	b.WriteString("    param($wordToComplete, $commandAst, $cursorPosition)\n\n")

	quote := func(s string) string { return "'" + escapePowerShellSingle(s) + "'" }
	writeMap := func(name string, entry func(i int) string) {
		var lines []string
		for i := range m.nodes {
			if s := entry(i); s != "" {
				lines = append(lines, fmt.Sprintf("        '%d' = %s", i, s))
			}
		}
		if len(lines) == 0 {
			fmt.Fprintf(&b, "    $%s = @{}\n", name)
			return
		}
		fmt.Fprintf(&b, "    $%s = @{\n%s\n    }\n", name, strings.Join(lines, "\n"))
	}

	// Candidate names, child indices, and descriptions per node with
	// children, aligned by position.
	writeMap("cmdNames", func(i int) string {
		nd := &m.nodes[i]
		if len(nd.children) == 0 {
			return ""
		}
		var names []string
		for _, ch := range nd.children {
			for _, nm := range ch.names {
				names = append(names, quote(nm))
			}
		}
		return "@(" + strings.Join(names, ", ") + ")"
	})
	writeMap("resTargs", func(i int) string {
		nd := &m.nodes[i]
		if len(nd.children) == 0 {
			return ""
		}
		var targets []string
		for _, ch := range nd.children {
			for range ch.names {
				targets = append(targets, strconv.Itoa(ch.target))
			}
		}
		return "@(" + strings.Join(targets, ", ") + ")"
	})
	writeMap("cmdDescs", func(i int) string {
		nd := &m.nodes[i]
		if len(nd.children) == 0 {
			return ""
		}
		var descs []string
		for _, ch := range nd.children {
			for range ch.names {
				descs = append(descs, quote(ch.desc))
			}
		}
		return "@(" + strings.Join(descs, ", ") + ")"
	})

	// Option specs per leaf with a parser: (long, short, bool,
	// repeatable) tuples in declaration order. A single-element outer
	// array needs the comma operator to keep the tuple nested.
	writeMap("optSpecs", func(i int) string {
		nd := &m.nodes[i]
		if len(nd.options) == 0 {
			return ""
		}
		var tuples []string
		for _, o := range nd.options {
			tuples = append(tuples, fmt.Sprintf("@(%s, %s, $%s, $%s)",
				quote(o.long), quote(o.short), psBool(o.boolean), psBool(o.repeatable)))
		}
		if len(tuples) == 1 {
			return "@(, " + tuples[0] + ")"
		}
		return "@(" + strings.Join(tuples, ", ") + ")"
	})
	b.WriteString("\n")

	b.WriteString(powerShellCompletionEngine)
	b.WriteString("}\n")
	return b.String()
}

func psBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

const bashCompletionEngine = `__ID___scan_cluster() {
    local token="$2" j info long
    for (( j = 1; j < ${#token}; j++ )); do
        info="$(__ID___short_info "$1" "${token:j:1}")"
        case "$info" in
            - | '') return 0 ;;
        esac
        long="${info%%|*}"
        seen+=("$long")
        case "$info" in
            *'|v')
                if (( j + 1 == ${#token} )); then
                    want_value=1
                fi
                return 0
                ;;
        esac
    done
}

__ID___seen() {
    local s
    for s in ${seen[@]+"${seen[@]}"}; do
        [[ "$s" == "$1" ]] && return 0
    done
    return 1
}

__ID___offer_long() {
    local long short kind rep
    while IFS='|' read -r long short kind rep; do
        [[ -z "$long" ]] && continue
        if [[ "$rep" != 1 ]] && __ID___seen "$long"; then
            continue
        fi
        [[ "$long" == "$2"* ]] && COMPREPLY+=("--$long")
    done < <(__ID___options "$1")
}

__ID___offer_short() {
    local long short kind rep
    while IFS='|' read -r long short kind rep; do
        [[ -z "$short" ]] && continue
        if [[ "$rep" != 1 ]] && __ID___seen "$long"; then
            continue
        fi
        [[ "$short" == "$2"* ]] && COMPREPLY+=("-$short")
    done < <(__ID___options "$1")
}

__ID___complete() {
    local cur word node next i
    local stopped=0 want_value=0
    local -a seen
    COMPREPLY=()
    cur="${COMP_WORDS[COMP_CWORD]}"
    node=0
    seen=()

    for (( i = 1; i < COMP_CWORD; i++ )); do
        word="${COMP_WORDS[i]}"
        if (( want_value )); then
            want_value=0
            continue
        fi
        if (( stopped )); then
            continue
        fi
        if __ID___has_children "$node"; then
            next="$(__ID___next "$node" "$word")"
            if [[ -n "$next" ]]; then
                node="$next"
            else
                stopped=1
            fi
            continue
        fi
        case "$word" in
            --)
                stopped=1
                ;;
            --*=*)
                word="${word#--}"
                seen+=("${word%%=*}")
                ;;
            --*)
                if [[ "$(__ID___long_kind "$node" "${word#--}")" == v ]]; then
                    want_value=1
                fi
                seen+=("${word#--}")
                ;;
            -?*)
                __ID___scan_cluster "$node" "$word"
                ;;
        esac
    done

    if (( stopped || want_value )); then
        return 0
    fi
    case "$cur" in
        --*=*)
            return 0
            ;;
    esac

    if __ID___has_children "$node"; then
        local name
        while IFS= read -r name; do
            [[ "$name" == "$cur"* ]] && COMPREPLY+=("$name")
        done < <(__ID___commands "$node")
        return 0
    fi

    case "$cur" in
        --*)
            __ID___offer_long "$node" "${cur:2}"
            ;;
        -*)
            __ID___offer_short "$node" "${cur:1}"
            __ID___offer_long "$node" "${cur:1}"
            ;;
    esac
    return 0
}

complete -o default -F __ID___complete __ROOT__
`

const zshCompletionEngine = `__ID___scan_cluster() {
    local token="$2" info long
    local j len=${#token}
    for (( j = 2; j <= len; j++ )); do
        info="$(__ID___short_info "$1" "${token[j]}")"
        case "$info" in
            - | '') return 0 ;;
        esac
        long="${info%%|*}"
        seen+=("$long")
        case "$info" in
            *'|v')
                if (( j == len )); then
                    want_value=1
                fi
                return 0
                ;;
        esac
    done
}

__ID___collect_long() {
    local long short kind rep
    while IFS='|' read -r long short kind rep; do
        [[ -z "$long" ]] && continue
        if [[ "$rep" != 1 ]] && (( ${seen[(Ie)$long]} )); then
            continue
        fi
        [[ "$long" == "$2"* ]] && matches+=("--$long")
    done < <(__ID___options "$1")
}

__ID___collect_short() {
    local long short kind rep
    while IFS='|' read -r long short kind rep; do
        [[ -z "$short" ]] && continue
        if [[ "$rep" != 1 ]] && (( ${seen[(Ie)$long]} )); then
            continue
        fi
        [[ "$short" == "$2"* ]] && matches+=("-$short")
    done < <(__ID___options "$1")
}

__ID___complete() {
    local cur word next
    local node=0 stopped=0 want_value=0
    local -a seen matches mdesc
    local i k
    cur="$PREFIX"
    seen=()
    matches=()
    mdesc=()

    for (( i = 2; i < CURRENT; i++ )); do
        word="${words[i]}"
        if (( want_value )); then
            want_value=0
            continue
        fi
        if (( stopped )); then
            continue
        fi
        if __ID___has_children "$node"; then
            next="$(__ID___next "$node" "$word")"
            if [[ -n "$next" ]]; then
                node="$next"
            else
                stopped=1
            fi
            continue
        fi
        case "$word" in
            --)
                stopped=1
                ;;
            --*=*)
                word="${word#--}"
                seen+=("${word%%=*}")
                ;;
            --*)
                if [[ "$(__ID___long_kind "$node" "${word#--}")" == v ]]; then
                    want_value=1
                fi
                seen+=("${word#--}")
                ;;
            -?*)
                __ID___scan_cluster "$node" "$word"
                ;;
        esac
    done

    if (( stopped || want_value )); then
        _files
        return
    fi
    case "$cur" in
        --*=*)
            _files
            return
            ;;
    esac

    if __ID___has_children "$node"; then
        local names_var="__ID___cmds_$node" desc_var="__ID___desc_$node"
        local -a names descs
        names=("${(@P)names_var}")
        descs=("${(@P)desc_var}")
        for (( k = 1; k <= ${#names}; k++ )); do
            if [[ "${names[k]}" == "$cur"* ]]; then
                matches+=("${names[k]}")
                mdesc+=("${descs[k]}")
            fi
        done
        if (( ${#matches} )); then
            compadd -V commands -d mdesc -a matches
            return
        fi
        _files
        return
    fi

    case "$cur" in
        --*)
            __ID___collect_long "$node" "${cur:2}"
            ;;
        -*)
            __ID___collect_short "$node" "${cur:1}"
            __ID___collect_long "$node" "${cur:1}"
            ;;
        *)
            _files
            return
            ;;
    esac
    if (( ${#matches} )); then
        compadd -V options -- "${matches[@]}"
        return
    fi
    _files
}

(( $+functions[compdef] )) && compdef __ID___complete __ROOT__
`

const fishCompletionEngine = `function __ID___has_prefix
    set -l plen (string length -- $argv[2])
    if test $plen -eq 0
        return 0
    end
    test (string sub -l $plen -- $argv[1]) = "$argv[2]"
end

function __ID___scan_cluster
    set -l token $argv[2]
    set -l len (string length -- $token)
    set -l j 2
    while test $j -le $len
        set -l ch (string sub -s $j -l 1 -- $token)
        set -l info (__ID___short_info $argv[1] $ch)
        if test "$info" = '-'; or test "$info" = ''
            return
        end
        set -l parts (string split '|' -- $info)
        set -a seen $parts[1]
        if test "$parts[2]" = v
            if test $j -eq $len
                set want_value 1
            end
            return
        end
        set j (math $j + 1)
    end
end

function __ID___offer_long
    for spec in (__ID___options $argv[1])
        set -l parts (string split '|' -- $spec)
        if test "$parts[4]" != 1
            if contains -- $parts[1] $seen
                continue
            end
        end
        if __ID___has_prefix $parts[1] $argv[2]
            printf '%s\n' "--$parts[1]"
        end
    end
end

function __ID___offer_short
    for spec in (__ID___options $argv[1])
        set -l parts (string split '|' -- $spec)
        if test -z "$parts[2]"
            continue
        end
        if test "$parts[4]" != 1
            if contains -- $parts[1] $seen
                continue
            end
        end
        if __ID___has_prefix $parts[2] $argv[2]
            printf '%s\n' "-$parts[2]"
        end
    end
end

function __ID___complete
    set -l tokens (commandline -opc)
    if test (count $tokens) -eq 0
        return
    end
    set -l before (commandline -cp)
    if not string match -qr '\s$' -- "$before"
        set -e tokens[-1]
    end
    if test (count $tokens) -eq 0
        return
    end
    set -l prev
    if test (count $tokens) -gt 1
        set prev $tokens[2..-1]
    end
    set -l cur ''
    set -l ct (commandline -ct)
    if set -q ct[1]
        set cur $ct[1]
    end

    set -l node 0
    set -l stopped 0
    set -l want_value 0
    set -l seen

    for word in $prev
        if test $want_value -eq 1
            set want_value 0
            continue
        end
        if test $stopped -eq 1
            continue
        end
        if __ID___has_children $node
            set -l next (__ID___next $node $word)
            if set -q next[1]
                set node $next[1]
            else
                set stopped 1
            end
            continue
        end
        if test "$word" = '--'
            set stopped 1
        else if string match -q -- '--*=*' $word
            set -l kv (string split -m 1 '=' -- (string sub -s 3 -- $word))
            set -a seen $kv[1]
        else if string match -q -- '--*' $word
            set -l lname (string sub -s 3 -- $word)
            set -a seen $lname
            set -l kind (__ID___long_kind $node $lname)
            if test "$kind" = v
                set want_value 1
            end
        else if string match -q -- '-?*' $word
            __ID___scan_cluster $node $word
        end
    end

    if test $stopped -eq 1; or test $want_value -eq 1
        return
    end
    if string match -q -- '--*=*' $cur
        return
    end

    if __ID___has_children $node
        set -l dvar __ID___desc_$node
        set -l descs $$dvar
        set -l k 0
        for name in (__ID___commands $node)
            set k (math $k + 1)
            if __ID___has_prefix $name $cur
                set -l d ''
                if test $k -le (count $descs)
                    set d (string replace -ar '[\r\n\t]' ' ' -- $descs[$k])
                end
                printf '%s\t%s\n' $name $d
            end
        end
        return
    end

    if string match -q -- '--*' $cur
        set -l pfx (string sub -s 3 -- $cur)
        set -q pfx[1]; or set pfx ''
        __ID___offer_long $node "$pfx"
    else if string match -q -- '-*' $cur
        set -l pfx (string sub -s 2 -- $cur)
        set -q pfx[1]; or set pfx ''
        __ID___offer_short $node "$pfx"
        __ID___offer_long $node "$pfx"
    end
end

complete -c __ROOT__ -a '(__ID___complete)'
`

const powerShellCompletionEngine = `    if ($null -eq $wordToComplete) { $wordToComplete = '' }
    if (($null -eq $commandAst) -or ($null -eq $commandAst.CommandElements)) { return }

    $prev = @()
    $elements = $commandAst.CommandElements
    $last = $elements.Count - 1
    for ($i = 1; $i -le $last; $i++) {
        if (($i -eq $last) -and ($wordToComplete -ne '')) { break }
        $prev += $elements[$i].Extent.Text
    }

    $node = '0'
    $stopped = $false
    $wantValue = $false
    $seen = @()

    foreach ($word in $prev) {
        if ($wantValue) { $wantValue = $false; continue }
        if ($stopped) { continue }
        $words = $cmdNames[$node]
        if ($null -ne $words) {
            $target = $null
            for ($k = 0; $k -lt $words.Count; $k++) {
                if ($words[$k] -ceq $word) { $target = $resTargs[$node][$k]; break }
            }
            if ($null -ne $target) { $node = [string]$target } else { $stopped = $true }
            continue
        }
        $opts = $optSpecs[$node]
        if ($word -ceq '--') { $stopped = $true; continue }
        if ($word.StartsWith('--')) {
            $body = $word.Substring(2)
            $eq = $body.IndexOf('=')
            if ($eq -ge 0) {
                $seen += $body.Substring(0, $eq)
            } else {
                $seen += $body
                if ($null -ne $opts) {
                    foreach ($o in $opts) {
                        if (($o[0] -ceq $body) -and (-not $o[2])) { $wantValue = $true; break }
                    }
                }
            }
            continue
        }
        if (($word.Length -gt 1) -and $word.StartsWith('-')) {
            for ($j = 1; $j -lt $word.Length; $j++) {
                $ch = $word.Substring($j, 1)
                $found = $null
                if ($null -ne $opts) {
                    foreach ($o in $opts) {
                        if (($o[1].Length -gt 0) -and ($o[1] -ceq $ch)) { $found = $o; break }
                    }
                }
                if ($null -eq $found) { break }
                $seen += $found[0]
                if (-not $found[2]) {
                    if ($j -eq ($word.Length - 1)) { $wantValue = $true }
                    break
                }
            }
            continue
        }
    }

    if ($stopped -or $wantValue) { return }
    if ($wordToComplete.StartsWith('--') -and $wordToComplete.Contains('=')) { return }

    $names = $cmdNames[$node]
    if ($null -ne $names) {
        $descs = $cmdDescs[$node]
        for ($k = 0; $k -lt $names.Count; $k++) {
            $name = $names[$k]
            if ($name.StartsWith($wordToComplete, [System.StringComparison]::Ordinal)) {
                [System.Management.Automation.CompletionResult]::new($name, $name, [System.Management.Automation.CompletionResultType]::ParameterValue, $descs[$k])
            }
        }
        return
    }

    $opts = $optSpecs[$node]
    if ($null -eq $opts) { return }
    if ($wordToComplete.StartsWith('--')) {
        $prefix = $wordToComplete.Substring(2)
        foreach ($o in $opts) {
            if ((-not $o[3]) -and ($seen -ccontains $o[0])) { continue }
            if ($o[0].StartsWith($prefix, [System.StringComparison]::Ordinal)) {
                $text = '--' + $o[0]
                [System.Management.Automation.CompletionResult]::new($text, $text, [System.Management.Automation.CompletionResultType]::ParameterName, $text)
            }
        }
        return
    }
    if ($wordToComplete.StartsWith('-')) {
        $prefix = $wordToComplete.Substring(1)
        foreach ($o in $opts) {
            if ((-not $o[3]) -and ($seen -ccontains $o[0])) { continue }
            if (($o[1].Length -gt 0) -and $o[1].StartsWith($prefix, [System.StringComparison]::Ordinal)) {
                $text = '-' + $o[1]
                [System.Management.Automation.CompletionResult]::new($text, $text, [System.Management.Automation.CompletionResultType]::ParameterName, $text)
            }
            if ($o[0].StartsWith($prefix, [System.StringComparison]::Ordinal)) {
                $text = '--' + $o[0]
                [System.Management.Automation.CompletionResult]::new($text, $text, [System.Management.Automation.CompletionResultType]::ParameterName, $text)
            }
        }
    }
`

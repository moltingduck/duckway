package main

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/hackerduck/duckway/internal/client"
)

// completionNode describes only public Duckway syntax. Empty flag values are
// free-form arguments, so completing them must not suggest command names.
type completionNode struct {
	children   string
	flags      map[string]string
	positional string
}

var completionNodes = map[string]completionNode{
	"":                     {children: "init sync env proxy status doctor config logs install-ca update mcp cc session ducklion projects git start stop restart integrate version help completion plugin --help -h --version -v"},
	"restart":              {flags: map[string]string{"-f": "!"}},
	"completion":           {children: "bash zsh fish"},
	"config":               {children: "get set"},
	"config get":           {children: "pty_log_retention_days"},
	"config set":           {children: "pty_log_retention_days"},
	"git":                  {children: "list ls setup clone"},
	"git clone":            {positional: "repos"},
	"mcp":                  {children: "serve"},
	"mcp serve":            {flags: map[string]string{"--config-dir": ""}},
	"integrate":            {children: "ducklion"},
	"projects":             {children: "add list remove rm clear"},
	"projects add":         {flags: map[string]string{"--name": "", "-n": "", "--config-dir": "", "--help": "!", "-h": "!"}},
	"cc":                   {children: "watch bind projects"},
	"cc watch":             {children: "stop restart status", flags: map[string]string{"--daemon": "!", "-d": "!", "--no-tmux": "!", "--tmux": "!", "--debug": "!", "-D": "!", "--help": "!", "-h": "!", "--config-dir": ""}},
	"cc bind":              {flags: map[string]string{"--session": "", "-s": "", "--cwd": "", "--config-dir": "", "-h": "!", "--help": "!"}},
	"session":              {children: "list start attach send read stop"},
	"session start":        {flags: map[string]string{"--name": "", "-n": "", "--kind": "terminal", "--agent": "shell claude codex", "--agent-type": "shell claude codex", "--cwd": "", "-C": "", "--tmux": "!", "--pty": "!", "--": "!"}},
	"session read":         {flags: map[string]string{"--lines": "", "-n": ""}},
	"proxy":                {children: "stop status restart exec hosts", flags: map[string]string{"--daemon": "!", "-d": "!", "--debug": "!", "-D": "!", "--port": ""}},
	"env":                  {flags: map[string]string{"--proxy": "!", "-p": "!"}},
	"update":               {flags: map[string]string{"--server": "", "--restart": "!"}},
	"logs":                 {children: "ducklion proxy cc cc-watch all", flags: map[string]string{"--follow": "!", "-f": "!", "--lines": "", "-n": ""}},
	"plugin":               {children: "install install-local list installed update run"},
	"plugin install-local": {flags: map[string]string{"--dir": ""}},
}

// completionCandidates accepts words after the executable, including the current
// (possibly empty) word. It never executes the requested command.
func completionCandidates(configDir string, words []string) []string {
	if len(words) == 0 {
		words = []string{""}
	}
	prefix := words[len(words)-1]
	prior := words[:len(words)-1]
	path := ""
	node := completionNodes[path]
	positionals := 0
	used := map[string]bool{}
	for i := 0; i < len(prior); i++ {
		word := prior[i]
		if word == "--" {
			return nil
		}
		if value, ok := node.flags[word]; ok {
			used[word] = true
			if value != "!" {
				if i+1 == len(prior) {
					return completionFilter(strings.Fields(value), prefix)
				}
				i++
			}
			continue
		}
		next := strings.TrimSpace(path + " " + word)
		if strings.Contains(" "+node.children+" ", " "+word+" ") {
			path = next
			if path == "cc projects" {
				path = "projects"
			}
			nextNode := completionNodes[path]
			if path == "proxy stop" || path == "proxy status" || path == "proxy restart" || strings.HasPrefix(path, "logs ") || strings.HasPrefix(path, "cc watch ") {
				nextNode.flags = node.flags
			}
			node = nextNode
			positionals = 0
		} else {
			if path == "" {
				return nil
			}
			positionals++
		}
	}
	var choices []string
	if positionals == 0 {
		choices = append(choices, strings.Fields(node.children)...)
	}
	for flag := range node.flags {
		if !used[flag] {
			choices = append(choices, flag)
		}
	}
	if node.positional == "repos" && positionals == 0 {
		choices = append(choices, client.LocalGitCompletionRepos(configDir)...)
	}
	return completionFilter(choices, prefix)
}

func completionFilter(choices []string, prefix string) []string {
	seen := map[string]bool{}
	var out []string
	for _, choice := range choices {
		if strings.HasPrefix(choice, prefix) && !seen[choice] {
			seen[choice] = true
			out = append(out, choice)
		}
	}
	sort.Strings(out)
	return out
}

func writeCompletion(out io.Writer, shell string) error {
	var script string
	switch shell {
	case "bash":
		script = `# Duckway Bash completion
_duckway_complete() {
    local word
    COMPREPLY=()
    while IFS= read -r word; do
        COMPREPLY+=("$word")
    done < <(command duckway __complete "${COMP_WORDS[@]:1:COMP_CWORD}")
}
complete -F _duckway_complete duckway
`
	case "zsh":
		script = `#compdef duckway
_duckway_complete() {
    local -a candidates
    candidates=("${(@f)$(command duckway __complete "${words[@]:1:$((CURRENT-1))}")}")
    (( ${#candidates} )) && compadd -- "${candidates[@]}"
}
compdef _duckway_complete duckway
`
	case "fish":
		script = `# Duckway Fish completion
function __duckway_complete
    set -l words (commandline -opc)
    set -e words[1]
    set -l current (commandline -ct)
    command duckway __complete $words "$current"
end
complete -c duckway -f -a '(__duckway_complete)'
`
	default:
		return fmt.Errorf("usage: duckway completion <bash|zsh|fish>")
	}
	_, err := io.WriteString(out, script)
	return err
}

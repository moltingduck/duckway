package client

import "strings"

// tomlStatement spans a complete logical statement, including its final newline.
// Values can cross physical lines inside strings, arrays and inline tables.
// This lexer only locates edit boundaries; it does not parse or reformat TOML.
type tomlStatement struct {
	start, end int
	raw        string
	header     string
	assignment int
}

func tomlStatements(input string) []tomlStatement {
	var out []tomlStatement
	start, depth, assignment := 0, 0, -1
	var quote byte
	multiline, comment := false, false
	emit := func(end int) {
		raw := input[start:end]
		header := ""
		if assignment < 0 {
			header = normalizeTOMLSectionHeader(raw)
		}
		out = append(out, tomlStatement{start, end, raw, header, assignment})
		start, assignment = end, -1
	}
	for i := 0; i < len(input); i++ {
		c := input[i]
		if comment {
			if c != '\n' {
				continue
			}
			comment = false
		} else if quote != 0 {
			if quote == '"' && c == '\\' {
				// A basic string escape also covers a multiline continuation.
				if i+1 < len(input) {
					i++
				}
				continue
			}
			if c == quote {
				if !multiline {
					quote = 0
				} else if i+2 < len(input) && input[i+1] == quote && input[i+2] == quote {
					// TOML allows one or two quotes immediately before the
					// closing triple delimiter (runs of four or five).
					for i+1 < len(input) && input[i+1] == quote {
						i++
					}
					quote = 0
				}
			}
			continue
		} else {
			switch c {
			case '#':
				comment = true
			case '\'', '"':
				quote, multiline = c, false
				if i+2 < len(input) && input[i+1] == c && input[i+2] == c {
					multiline = true
					i += 2
				}
			case '[', '{':
				depth++
			case ']', '}':
				if depth > 0 {
					depth--
				}
			case '=':
				if depth == 0 && assignment < 0 {
					assignment = i - start
				}
			}
		}
		if c == '\n' && quote == 0 && depth == 0 {
			emit(i + 1)
		}
	}
	if start < len(input) {
		emit(len(input))
	}
	return out
}

func (s tomlStatement) isKey(key string) bool {
	if s.assignment < 0 {
		return false
	}
	name := strings.TrimSpace(s.raw[:s.assignment])
	return name == key || name == tomlQuote(key) || name == "'"+key+"'"
}

func (s tomlStatement) stringIs(value string) bool {
	if s.assignment < 0 {
		return false
	}
	rhs := strings.TrimSpace(s.raw[s.assignment+1:])
	for _, quoted := range []string{tomlQuote(value), "'" + value + "'"} {
		if rest, ok := strings.CutPrefix(rhs, quoted); ok {
			rest = strings.TrimSpace(rest)
			if rest == "" || strings.HasPrefix(rest, "#") {
				return true
			}
		}
	}
	return false
}

func removeTopLevelTOMLStringValue(input, key, value string) string {
	var out strings.Builder
	inTable := false
	for _, stmt := range tomlStatements(input) {
		inTable = inTable || stmt.header != ""
		if !inTable && stmt.isKey(key) && stmt.stringIs(value) {
			continue
		}
		out.WriteString(stmt.raw)
	}
	return out.String()
}

func replaceTopLevelTOMLString(input, key, value string) string {
	replacement := key + " = " + tomlQuote(value) + "\n"
	for _, stmt := range tomlStatements(input) {
		if stmt.header != "" {
			return input[:stmt.start] + replacement + input[stmt.start:]
		}
		if stmt.isKey(key) {
			if !strings.HasSuffix(stmt.raw, "\n") {
				replacement = strings.TrimSuffix(replacement, "\n")
			}
			return input[:stmt.start] + replacement + input[stmt.end:]
		}
	}
	if input != "" && !strings.HasSuffix(input, "\n") {
		input += "\n"
	}
	return input + replacement
}

func removeTOMLSection(input, section string) string {
	return editTOMLSection(input, section, "")
}

func replaceTOMLSection(input, section, replacement string) string {
	return editTOMLSection(input, section, replacement)
}

func editTOMLSection(input, section, replacement string) string {
	var out strings.Builder
	found, skipping := false, false
	for _, stmt := range tomlStatements(input) {
		if stmt.header != "" {
			skipping = stmt.header == section
			if skipping && !found {
				out.WriteString(replacement)
				if replacement != "" && !strings.HasSuffix(replacement, "\n") {
					out.WriteByte('\n')
				}
				found = true
			}
		}
		if !skipping {
			out.WriteString(stmt.raw)
		}
	}
	if !found && replacement != "" {
		if out.Len() > 0 {
			out.WriteString("\n")
			if !strings.HasSuffix(input, "\n") {
				out.WriteByte('\n')
			}
		}
		out.WriteString(strings.TrimRight(replacement, "\n"))
		out.WriteByte('\n')
	}
	return out.String()
}

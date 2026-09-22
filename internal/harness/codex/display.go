package codex

import "strings"

// splitDisplay splits quoted display arguments without evaluating shell syntax.
// Comments, variables and substitutions are data. Native argv, when available,
// always takes precedence; malformed displays remain strings in Normalize.
func splitDisplay(s string) ([]string, bool) {
	words := []string{}
	var word strings.Builder
	var quote byte
	started := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '\'':
			if c == quote {
				quote = 0
			} else {
				word.WriteByte(c)
			}
		case c == '\\':
			i++
			if i == len(s) {
				return nil, false
			}
			if quote == '"' && s[i] != '"' && s[i] != '\\' {
				word.WriteByte('\\')
			}
			word.WriteByte(s[i])
			started = true
		case quote == '"':
			if c == quote {
				quote = 0
			} else {
				word.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, started = c, true
		case strings.ContainsRune(" \t\r\n", rune(c)):
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
		default:
			word.WriteByte(c)
			started = true
		}
	}
	if quote != 0 {
		return nil, false
	}
	if started {
		words = append(words, word.String())
	}
	return words, true
}

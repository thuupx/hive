package storage

import (
	"errors"
	"strings"
)

// splitStatements splits a SQL script into individual statements.
//
// libSQL executes one statement per call, so migration scripts are split
// before execution. Semicolons inside single-quoted string literals, line
// comments, and block comments are not treated as separators. Splitting
// explicitly keeps migration failures attributable to one statement and
// avoids relying on undocumented multi-statement behaviour.
func splitStatements(script string) ([]string, error) {
	var (
		out []string
		b   strings.Builder
	)

	flush := func() {
		if s := strings.TrimSpace(b.String()); s != "" {
			out = append(out, s)
		}
		b.Reset()
	}

	for i := 0; i < len(script); i++ {
		switch c := script[i]; {
		case c == '\'':
			b.WriteByte(c)
			i++
			for {
				if i >= len(script) {
					return nil, errors.New("unterminated string literal")
				}
				b.WriteByte(script[i])
				if script[i] != '\'' {
					i++
					continue
				}
				// A doubled quote is an escaped quote inside the literal.
				if i+1 < len(script) && script[i+1] == '\'' {
					b.WriteByte(script[i+1])
					i += 2
					continue
				}
				break
			}

		case c == '-' && i+1 < len(script) && script[i+1] == '-':
			for i < len(script) && script[i] != '\n' {
				i++
			}
			if i < len(script) {
				b.WriteByte('\n')
			}

		case c == '/' && i+1 < len(script) && script[i+1] == '*':
			end := strings.Index(script[i+2:], "*/")
			if end < 0 {
				return nil, errors.New("unterminated block comment")
			}
			i += 2 + end + 1

		case c == ';':
			flush()

		default:
			b.WriteByte(c)
		}
	}

	flush()
	return out, nil
}

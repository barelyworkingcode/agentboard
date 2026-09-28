package ghparse

import "strings"

// opaque stands in for an expansion ($(...), `...`, $VAR, ${...}) whose value
// the lexer never reads. A word holding it is never a usable value.
const opaque = '\x00'

// lexer splits a shell command line into simple commands of unquoted words.
// It covers what gh invocations need, not the whole shell grammar.
type lexer struct {
	s       string
	i       int
	pending []heredoc
}

type heredoc struct {
	delim string
	tabs  bool // <<- strips leading tabs
}

// commands splits l.s into simple commands at && || ; | & ( ) and newlines.
// With sub set it stops after the ) closing a $( it was called for.
func (l *lexer) commands(sub bool) [][]string {
	var (
		cmds  [][]string
		cur   []string
		depth int
	)
	end := func() {
		if len(cur) > 0 {
			cmds = append(cmds, cur)
			cur = nil
		}
	}
	for l.i < len(l.s) {
		c := l.s[l.i]
		switch {
		case c == ' ' || c == '\t':
			l.i++
		case c == '\n':
			l.i++
			l.heredocs()
			end()
		case c == '#':
			for l.i < len(l.s) && l.s[l.i] != '\n' {
				l.i++
			}
		case c == '<' || c == '>' || c == '&' && l.peek(1) == '>':
			l.redirect()
		case c == ';' || c == '&' || c == '|':
			l.i++
			if strings.IndexByte(";&|", l.peek(0)) >= 0 {
				l.i++
			}
			end()
		case c == '(':
			l.i++
			depth++
			end()
		case c == ')':
			l.i++
			end()
			if depth > 0 {
				depth--
			} else if sub {
				return cmds
			}
		default:
			w, plain, ok := l.word()
			if plain && isDigits(w) && (l.peek(0) == '<' || l.peek(0) == '>') {
				continue // a file descriptor, as in 2>&1
			}
			if ok {
				cur = append(cur, w)
			}
		}
	}
	end()
	return cmds
}

// peek returns the byte n past l.i, or 0 past the end.
func (l *lexer) peek(n int) byte {
	if l.i+n < len(l.s) {
		return l.s[l.i+n]
	}
	return 0
}

// word reads one word, removing quotes and escapes. plain reports that it had
// no quoting or expansion; ok that it had any content at all, where an
// empty quoted string counts.
func (l *lexer) word() (w string, plain, ok bool) {
	var b strings.Builder
	plain = true
	for l.i < len(l.s) && !isBreak(l.s[l.i]) {
		c := l.s[l.i]
		switch c {
		case '\\':
			l.i++
			if l.i < len(l.s) {
				if l.s[l.i] != '\n' {
					b.WriteByte(l.s[l.i])
					ok = true
				}
				l.i++
			}
			plain = false
		case '\'':
			l.i++
			j := strings.IndexByte(l.s[l.i:], '\'')
			if j < 0 {
				j = len(l.s) - l.i
			}
			b.WriteString(l.s[l.i : l.i+j])
			l.i = min(l.i+j+1, len(l.s))
			ok, plain = true, false
		case '"':
			l.i++
			l.dquote(&b)
			ok, plain = true, false
		case '$', '`':
			if l.expand() {
				b.WriteByte(opaque)
				plain = false
			} else {
				b.WriteByte(c)
				l.i++
			}
			ok = true
		default:
			b.WriteByte(c)
			l.i++
			ok = true
		}
	}
	return b.String(), plain, ok
}

// dquote reads the rest of a double-quoted string into b.
func (l *lexer) dquote(b *strings.Builder) {
	for l.i < len(l.s) {
		c := l.s[l.i]
		switch {
		case c == '"':
			l.i++
			return
		case c == '\\' && strings.IndexByte("$`\"\\\n", l.peek(1)) >= 0 && l.peek(1) != 0:
			if l.peek(1) != '\n' {
				b.WriteByte(l.peek(1))
			}
			l.i += 2
		case (c == '$' || c == '`') && l.expand():
			b.WriteByte(opaque)
		default:
			b.WriteByte(c)
			l.i++
		}
	}
}

// expand skips the expansion at l.i and reports true, or leaves l.i alone and
// reports false when the $ is literal.
func (l *lexer) expand() bool {
	if l.s[l.i] == '`' {
		for l.i++; l.i < len(l.s) && l.s[l.i] != '`'; l.i++ {
			if l.s[l.i] == '\\' {
				l.i++
			}
		}
		l.i = min(l.i+1, len(l.s))
		return true
	}
	c := l.peek(1)
	switch {
	case c == '(':
		l.i += 2
		l.commands(true)
	case c == '{':
		depth := 0
		for l.i += 2; l.i < len(l.s); l.i++ {
			if l.s[l.i] == '{' {
				depth++
			} else if l.s[l.i] == '}' {
				if depth == 0 {
					break
				}
				depth--
			}
		}
		l.i = min(l.i+1, len(l.s))
	case isNameByte(c) && !isDigit(c):
		for l.i++; l.i < len(l.s) && isNameByte(l.s[l.i]); l.i++ {
		}
	case c != 0 && strings.IndexByte("0123456789?$#@*!-", c) >= 0:
		l.i += 2
	default:
		return false
	}
	return true
}

// redirect skips a redirection operator and its target. A << target is a
// heredoc delimiter whose body is skipped at the next newline.
func (l *lexer) redirect() {
	start := l.i
	for l.i < len(l.s) && l.i-start < 3 && strings.IndexByte("<>&", l.s[l.i]) >= 0 {
		l.i++
	}
	op := l.s[start:l.i]
	if op == "<<" && l.peek(0) == '-' || strings.HasSuffix(op, ">") && l.peek(0) == '|' {
		op += string(l.s[l.i])
		l.i++
	}
	for l.peek(0) == ' ' || l.peek(0) == '\t' {
		l.i++
	}
	if l.i >= len(l.s) || isBreak(l.s[l.i]) {
		return
	}
	target, _, _ := l.word()
	if op == "<<" || op == "<<-" {
		l.pending = append(l.pending, heredoc{delim: target, tabs: op == "<<-"})
	}
}

// heredocs skips the bodies of pending heredocs, which start at l.i.
func (l *lexer) heredocs() {
	for _, h := range l.pending {
		for l.i < len(l.s) {
			line := l.s[l.i:]
			if j := strings.IndexByte(line, '\n'); j >= 0 {
				line = line[:j]
			}
			l.i = min(l.i+len(line)+1, len(l.s))
			if h.tabs {
				line = strings.TrimLeft(line, "\t")
			}
			if line == h.delim {
				break
			}
		}
	}
	l.pending = nil
}

func isBreak(c byte) bool { return strings.IndexByte(" \t\n;&|()<>", c) >= 0 }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isNameByte(c byte) bool {
	return c == '_' || isDigit(c) || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isDigits(s string) bool {
	for i := range len(s) {
		if !isDigit(s[i]) {
			return false
		}
	}
	return s != ""
}

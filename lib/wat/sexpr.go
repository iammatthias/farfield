package wat

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// node is one S-expression: an atom (keyword, $id, number, key=value), a
// string literal, or a parenthesized list.
type node struct {
	atom   string
	str    []byte // decoded bytes of a string literal
	isStr  bool
	list   []node
	isList bool
	pos    pos
}

type pos struct {
	file string
	line int
}

func (p pos) String() string { return fmt.Sprintf("%s:%d", p.file, p.line) }

// head is the first atom of a list — its keyword — or "".
func (n node) head() string {
	if n.isList && len(n.list) > 0 && !n.list[0].isList && !n.list[0].isStr {
		return n.list[0].atom
	}
	return ""
}

// parseSexprs reads every top-level S-expression in src.
func parseSexprs(file, src string) ([]node, error) {
	p := &reader{file: file, src: src, line: 1}
	var out []node
	for {
		if err := p.skip(); err != nil {
			return nil, err
		}
		if p.i >= len(p.src) {
			return out, nil
		}
		n, err := p.read()
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
}

type reader struct {
	file string
	src  string
	i    int
	line int
}

func (p *reader) errf(format string, a ...any) error {
	return fmt.Errorf("%s:%d: %s", p.file, p.line, fmt.Sprintf(format, a...))
}

// skip advances past whitespace, ;; line comments, and (; nested ;) block
// comments.
func (p *reader) skip() error {
	for p.i < len(p.src) {
		c := p.src[p.i]
		switch {
		case c == '\n':
			p.line++
			p.i++
		case c == ' ' || c == '\t' || c == '\r':
			p.i++
		case strings.HasPrefix(p.src[p.i:], ";;"):
			for p.i < len(p.src) && p.src[p.i] != '\n' {
				p.i++
			}
		case strings.HasPrefix(p.src[p.i:], "(;"):
			depth := 0
			for p.i < len(p.src) {
				switch {
				case strings.HasPrefix(p.src[p.i:], "(;"):
					depth++
					p.i += 2
				case strings.HasPrefix(p.src[p.i:], ";)"):
					depth--
					p.i += 2
				default:
					if p.src[p.i] == '\n' {
						p.line++
					}
					p.i++
				}
				if depth == 0 {
					break
				}
			}
			if depth != 0 {
				return p.errf("unterminated block comment")
			}
		default:
			return nil
		}
	}
	return nil
}

func (p *reader) read() (node, error) {
	here := pos{p.file, p.line}
	c := p.src[p.i]
	switch c {
	case '(':
		p.i++
		n := node{isList: true, pos: here}
		for {
			if err := p.skip(); err != nil {
				return node{}, err
			}
			if p.i >= len(p.src) {
				return node{}, fmt.Errorf("%s: unclosed '('", here)
			}
			if p.src[p.i] == ')' {
				p.i++
				return n, nil
			}
			child, err := p.read()
			if err != nil {
				return node{}, err
			}
			n.list = append(n.list, child)
		}
	case ')':
		return node{}, p.errf("unexpected ')'")
	case '"':
		b, err := p.readString()
		if err != nil {
			return node{}, err
		}
		return node{str: b, isStr: true, pos: here}, nil
	default:
		start := p.i
		for p.i < len(p.src) {
			c := p.src[p.i]
			if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '(' || c == ')' || c == '"' || c == ';' {
				break
			}
			p.i++
		}
		return node{atom: p.src[start:p.i], pos: here}, nil
	}
}

// readString decodes a WAT string literal: \n \t \r \\ \" \' \hh and
// \u{hex}. The result is raw bytes, so data segments can hold anything.
func (p *reader) readString() ([]byte, error) {
	p.i++ // opening quote
	var out []byte
	for {
		if p.i >= len(p.src) {
			return nil, p.errf("unterminated string")
		}
		c := p.src[p.i]
		switch c {
		case '"':
			p.i++
			return out, nil
		case '\n':
			return nil, p.errf("newline in string")
		case '\\':
			if p.i+1 >= len(p.src) {
				return nil, p.errf("dangling escape")
			}
			e := p.src[p.i+1]
			switch e {
			case 'n':
				out = append(out, '\n')
				p.i += 2
			case 't':
				out = append(out, '\t')
				p.i += 2
			case 'r':
				out = append(out, '\r')
				p.i += 2
			case '\\', '"', '\'':
				out = append(out, e)
				p.i += 2
			case 'u':
				end := strings.IndexByte(p.src[p.i:], '}')
				if !strings.HasPrefix(p.src[p.i:], `\u{`) || end < 0 {
					return nil, p.errf(`bad \u escape`)
				}
				v, err := strconv.ParseUint(p.src[p.i+3:p.i+end], 16, 32)
				if err != nil {
					return nil, p.errf(`bad \u escape: %v`, err)
				}
				out = utf8.AppendRune(out, rune(v))
				p.i += end + 1
			default:
				if p.i+2 >= len(p.src) {
					return nil, p.errf("bad escape")
				}
				v, err := strconv.ParseUint(p.src[p.i+1:p.i+3], 16, 8)
				if err != nil {
					return nil, p.errf("bad escape \\%s", p.src[p.i+1:p.i+3])
				}
				out = append(out, byte(v))
				p.i += 3
			}
		default:
			out = append(out, c)
			p.i++
		}
	}
}

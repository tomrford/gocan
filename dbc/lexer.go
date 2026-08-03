package dbc

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

type tokenKind uint8

const (
	tokenEOF tokenKind = iota
	tokenNewline
	tokenAtom
	tokenNumber
	tokenString
	tokenColon
	tokenSemicolon
	tokenPipe
	tokenAt
	tokenLeftParen
	tokenRightParen
	tokenLeftBracket
	tokenRightBracket
	tokenComma
	tokenPlus
	tokenMinus
)

type token struct {
	kind tokenKind
	text string
	pos  Position
}

type lexer struct {
	source string
	input  string
	offset int
	line   int
	column int
}

func lex(source, input string) ([]token, error) {
	if !utf8.ValidString(input) {
		return nil, &Error{
			Position: Position{Source: source, Line: 1, Column: 1},
			Message:  "input is not valid UTF-8",
		}
	}

	l := lexer{source: source, input: input, line: 1, column: 1}
	var tokens []token
	for {
		tok, err := l.next()
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, tok)
		if tok.kind == tokenEOF {
			return tokens, nil
		}
	}
}

func (l *lexer) next() (token, error) {
	for l.offset < len(l.input) {
		switch l.input[l.offset] {
		case ' ', '\t', '\r':
			l.advanceByte()
		case '/':
			if l.offset+1 < len(l.input) && l.input[l.offset+1] == '/' {
				for l.offset < len(l.input) && l.input[l.offset] != '\n' {
					l.advanceByte()
				}
				continue
			}
			return l.atom()
		default:
			goto ready
		}
	}

ready:
	pos := l.position()
	if l.offset >= len(l.input) {
		return token{kind: tokenEOF, pos: pos}, nil
	}

	character := l.input[l.offset]
	switch character {
	case '\n':
		l.offset++
		l.line++
		l.column = 1
		return token{kind: tokenNewline, text: "\n", pos: pos}, nil
	case '"':
		return l.quoted()
	case ':':
		l.advanceByte()
		return token{kind: tokenColon, text: ":", pos: pos}, nil
	case ';':
		l.advanceByte()
		return token{kind: tokenSemicolon, text: ";", pos: pos}, nil
	case '|':
		l.advanceByte()
		return token{kind: tokenPipe, text: "|", pos: pos}, nil
	case '@':
		l.advanceByte()
		return token{kind: tokenAt, text: "@", pos: pos}, nil
	case '(':
		l.advanceByte()
		return token{kind: tokenLeftParen, text: "(", pos: pos}, nil
	case ')':
		l.advanceByte()
		return token{kind: tokenRightParen, text: ")", pos: pos}, nil
	case '[':
		l.advanceByte()
		return token{kind: tokenLeftBracket, text: "[", pos: pos}, nil
	case ']':
		l.advanceByte()
		return token{kind: tokenRightBracket, text: "]", pos: pos}, nil
	case ',':
		l.advanceByte()
		return token{kind: tokenComma, text: ",", pos: pos}, nil
	case '+':
		l.advanceByte()
		return token{kind: tokenPlus, text: "+", pos: pos}, nil
	case '-':
		l.advanceByte()
		return token{kind: tokenMinus, text: "-", pos: pos}, nil
	default:
		if isDigit(character) || (character == '.' && l.offset+1 < len(l.input) && isDigit(l.input[l.offset+1])) {
			return l.number()
		}
		return l.atom()
	}
}

func (l *lexer) number() (token, error) {
	start := l.offset
	pos := l.position()
	seenDot := false
	seenExponent := false

	for l.offset < len(l.input) {
		character := l.input[l.offset]
		switch {
		case isDigit(character):
			l.advanceByte()
		case character == '.' && !seenDot && !seenExponent:
			seenDot = true
			l.advanceByte()
		case (character == 'e' || character == 'E') && !seenExponent:
			seenExponent = true
			l.advanceByte()
			if l.offset < len(l.input) && (l.input[l.offset] == '+' || l.input[l.offset] == '-') {
				l.advanceByte()
			}
		default:
			return token{kind: tokenNumber, text: l.input[start:l.offset], pos: pos}, nil
		}
	}

	return token{kind: tokenNumber, text: l.input[start:l.offset], pos: pos}, nil
}

func (l *lexer) atom() (token, error) {
	start := l.offset
	pos := l.position()
	for l.offset < len(l.input) && !isDelimiter(l.input[l.offset]) {
		l.advanceByte()
	}
	if start == l.offset {
		return token{}, &Error{Position: pos, Message: fmt.Sprintf("unexpected character %q", l.input[l.offset])}
	}
	return token{kind: tokenAtom, text: l.input[start:l.offset], pos: pos}, nil
}

func (l *lexer) quoted() (token, error) {
	pos := l.position()
	l.advanceByte()
	var value strings.Builder
	for l.offset < len(l.input) {
		character := l.input[l.offset]
		if character == '"' {
			l.advanceByte()
			return token{kind: tokenString, text: value.String(), pos: pos}, nil
		}
		if character == '\n' {
			value.WriteByte(character)
			l.offset++
			l.line++
			l.column = 1
			continue
		}
		if character != '\\' {
			value.WriteByte(character)
			l.advanceByte()
			continue
		}

		l.advanceByte()
		if l.offset >= len(l.input) {
			break
		}
		escaped := l.input[l.offset]
		switch escaped {
		case 'n':
			value.WriteByte('\n')
		case 'r':
			value.WriteByte('\r')
		case 't':
			value.WriteByte('\t')
		case '"', '\\':
			value.WriteByte(escaped)
		default:
			value.WriteByte('\\')
			value.WriteByte(escaped)
		}
		l.advanceByte()
	}

	return token{}, &Error{Position: pos, Message: "unterminated quoted string"}
}

func (l *lexer) position() Position {
	return Position{Source: l.source, Line: l.line, Column: l.column}
}

func (l *lexer) advanceByte() {
	l.offset++
	l.column++
}

func isDelimiter(character byte) bool {
	switch character {
	case ' ', '\t', '\r', '\n', '"', ':', ';', '|', '@', '(', ')', '[', ']', ',', '+', '-':
		return true
	default:
		return false
	}
}

func isDigit(character byte) bool {
	return character >= '0' && character <= '9'
}

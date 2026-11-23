package cdd

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/text/encoding/charmap"
)

type element struct {
	name     string
	attrs    []xml.Attr
	children []*element
	text     strings.Builder
	inText   bool
}

func decodeXML(name string, r io.Reader) (*element, error) {
	input := &recordingReader{reader: r}
	decoder := xml.NewDecoder(input)
	// encoding/xml reports charset failures as plain text, so the
	// unsupported encoding is kept to classify the decode error.
	var charsetErr error
	decoder.CharsetReader = func(label string, input io.Reader) (io.Reader, error) {
		switch strings.ToLower(strings.TrimSpace(label)) {
		case "iso-8859-1", "iso8859-1", "latin1":
			return charmap.ISO8859_1.NewDecoder().Reader(input), nil
		case "windows-1252", "cp1252":
			return charmap.Windows1252.NewDecoder().Reader(input), nil
		default:
			charsetErr = unsupportedf("unsupported XML encoding %q", label)
			return nil, charsetErr
		}
	}

	var root *element
	var stack []*element
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if charsetErr != nil {
			return nil, sourceError(name, charsetErr)
		}
		if err != nil {
			return nil, sourceError(name, xmlError(err, input.err))
		}
		switch token := token.(type) {
		case xml.StartElement:
			// The decoder allocates a fresh attribute slice for every start
			// element, so the element can keep it.
			node := &element{name: token.Name.Local, attrs: token.Attr}
			node.inText = node.name == "TUV" || len(stack) > 0 && stack[len(stack)-1].inText
			if len(stack) == 0 {
				if root != nil {
					return nil, sourceError(name, invalidf("XML contains more than one root element"))
				}
				root = node
			} else {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, node)
			}
			stack = append(stack, node)
		case xml.CharData:
			if len(stack) != 0 {
				stack[len(stack)-1].text.Write(token)
			}
		case xml.EndElement:
			node := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if len(stack) > 0 && stack[len(stack)-1].inText {
				parent := stack[len(stack)-1]
				if node.name == "PARA" {
					parent.text.WriteString("\n\n")
				}
				parent.text.WriteString(node.text.String())
				if node.name == "PARA" || node.name == "BR" {
					parent.text.WriteString("\n\n")
				}
			}
		}
	}
	if root == nil {
		return nil, sourceError(name, invalidf("XML contains no root element"))
	}
	return root, nil
}

// xmlError classifies a decoder failure. A reader failure says nothing about
// the document, even when the decoder reports it as a syntax error, such as
// a character cut short. Otherwise malformed XML makes the document invalid,
// and the decoder's remaining errors reject an XML version or encoding it
// does not implement.
func xmlError(err, readErr error) error {
	if readErr != nil {
		return fmt.Errorf("read XML: %w", readErr)
	}
	var syntaxErr *xml.SyntaxError
	if errors.As(err, &syntaxErr) {
		return invalidf("decode XML: %w", err)
	}
	return unsupportedf("decode XML: %w", err)
}

// recordingReader keeps the first reader failure, which the decoder may
// report only as malformed input.
type recordingReader struct {
	reader io.Reader
	err    error
}

func (r *recordingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err != nil && err != io.EOF && r.err == nil {
		r.err = err
	}
	return n, err
}

func (node *element) attr(name string) string {
	value, _ := node.lookup(name)
	return value
}

// lookup returns the attribute with local name. Namespaces are ignored, and the
// last attribute wins when several share a local name.
func (node *element) lookup(name string) (string, bool) {
	if node == nil {
		return "", false
	}
	for index := len(node.attrs) - 1; index >= 0; index-- {
		if node.attrs[index].Name.Local == name {
			return node.attrs[index].Value, true
		}
	}
	return "", false
}

func (node *element) child(name string) *element {
	if node == nil {
		return nil
	}
	for _, child := range node.children {
		if child.name == name {
			return child
		}
	}
	return nil
}

func (node *element) childrenNamed(name string) []*element {
	if node == nil {
		return nil
	}
	var children []*element
	for _, child := range node.children {
		if child.name == name {
			children = append(children, child)
		}
	}
	return children
}

func (node *element) childText(name string) string {
	child := node.child(name)
	if child == nil {
		return ""
	}
	return strings.TrimSpace(child.text.String())
}

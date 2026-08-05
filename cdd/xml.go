package cdd

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"

	"golang.org/x/text/encoding/charmap"
)

type element struct {
	name     string
	attrs    map[string]string
	children []*element
	text     strings.Builder
}

func decodeXML(name string, source []byte) (*element, error) {
	decoder := xml.NewDecoder(strings.NewReader(string(source)))
	decoder.CharsetReader = func(label string, input io.Reader) (io.Reader, error) {
		switch strings.ToLower(strings.TrimSpace(label)) {
		case "iso-8859-1", "iso8859-1", "latin1":
			return charmap.ISO8859_1.NewDecoder().Reader(input), nil
		default:
			return nil, fmt.Errorf("unsupported XML encoding %q", label)
		}
	}

	var root *element
	var stack []*element
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, sourceError(name, "decode XML: %v", err)
		}
		switch token := token.(type) {
		case xml.StartElement:
			node := &element{name: token.Name.Local, attrs: make(map[string]string, len(token.Attr))}
			for _, attr := range token.Attr {
				node.attrs[attr.Name.Local] = attr.Value
			}
			if len(stack) == 0 {
				if root != nil {
					return nil, sourceError(name, "XML contains more than one root element")
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
			stack = stack[:len(stack)-1]
		}
	}
	if root == nil {
		return nil, sourceError(name, "XML contains no root element")
	}
	return root, nil
}

func (node *element) attr(name string) string {
	if node == nil {
		return ""
	}
	return node.attrs[name]
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
	children := make([]*element, 0)
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

func (node *element) firstText(path ...string) string {
	for _, name := range path {
		node = node.child(name)
		if node == nil {
			return ""
		}
	}
	return strings.TrimSpace(node.text.String())
}

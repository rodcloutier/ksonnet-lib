package ksonnet

import (
	"bytes"
	"fmt"
	"strings"
)

// indentWriter abstracts the task of writing out indented text to a
// buffer. Different components can call `indent` and `dedent` as
// appropriate to specify how indentation needs to change, rather than
// to keep track of the current indentation.
//
// For example, if one component is responsible for writing an array,
// and an element in that array is a function, the component
// responsible for the array need only know to call `indent` after the
// '[' character and `dedent` before the ']' character, while the
// routine responsible for writing out the function can handle its own
// indentation independently.
type indentedLine struct {
	depth int
	text  string
}

type indentWriter struct {
	depth int
	lines []indentedLine
}

func (i *indentWriter) writeLine(text string) {
	i.lines = append(i.lines, indentedLine{i.depth, text})
}

func (i *indentWriter) insert(o indentWriter) {
	for _, l := range o.lines {
		i.lines = append(i.lines, indentedLine{l.depth + i.depth, l.text})
	}
}

func (i *indentWriter) write(buffer *bytes.Buffer) error {

	for _, l := range i.lines {
		prefix := strings.Repeat("  ", l.depth)
		line := fmt.Sprintf("%s%s\n", prefix, l.text)
		_, err := buffer.WriteString(line)
		if err != nil {
			return err
		}
	}

	return nil
}

func (i *indentWriter) indent() {
	i.depth++
}

func (i *indentWriter) dedent() {
	i.depth--
}

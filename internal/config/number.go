package config

import (
	"fmt"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Numeric setting kept in the whole or fractional form it was written in, written back as the same text
type Number struct {
	tag   string
	value string
	style yaml.Style
}

// Builds a number written as a whole number
func IntNumber(v int) Number {
	return Number{tag: tagInt, value: strconv.Itoa(v)}
}

// Builds a number written with a fraction
func FloatNumber(v float64) Number {
	return Number{tag: tagFloat, value: floatText(v)}
}

// Reads a number under the default coercion
func (n *Number) UnmarshalYAML(node *yaml.Node) error {
	return n.decodeCoerced(node, coerceDefault)
}

// Keeps a scalar the field coercion reads as a number, leaving the receiver as it was for anything else
func (n *Number) decodeCoerced(node *yaml.Node, c coercion) error {
	node, err := resolved(node)
	if err != nil {
		return rootError(err)
	}
	if node == nil || node.Kind != yaml.ScalarNode {
		return shapeError(node, yaml.ScalarNode)
	}
	if _, ok := floatOf(node, node.ShortTag(), c); !ok {
		return rootError(fmt.Errorf("%w: %s is not a number", ErrScalar, kindName(node)))
	}
	*n = Number{tag: node.ShortTag(), value: node.Value, style: chosenStyle(node)}
	return nil
}

// Writes the number in the form it was read or built in
func (n Number) MarshalYAML() (any, error) {
	if n.tag == "" {
		return IntNumber(0).MarshalYAML()
	}
	return emitted(&yaml.Node{Kind: yaml.ScalarNode, Tag: n.tag, Value: n.value, Style: n.style}), nil
}

// Value as a float, zero for the zero Number
func (n Number) Float() float64 {
	if n.tag == "" {
		return 0
	}
	f, _ := floatOf(&yaml.Node{Kind: yaml.ScalarNode, Tag: n.tag, Value: n.value}, n.tag, coerceDefault)
	return f
}

// Text of the number in the form it was written, a whole number without a fraction
func (n Number) String() string {
	if n.tag == "" {
		return "0"
	}
	return displayText(&yaml.Node{Kind: yaml.ScalarNode, Tag: n.tag, Value: n.value})
}

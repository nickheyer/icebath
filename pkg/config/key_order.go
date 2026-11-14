package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

// Keys of the mapping at a key path of a YAML file in document order, nil when the file or the mapping is absent
func MappingKeyOrder(path string, keys ...string) []string {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	doc, err := parseSingleYAMLDocument(src)
	if err != nil || doc == nil {
		return nil
	}
	node := derefAlias(doc)
	if node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
		node = derefAlias(node.Content[0])
	}
	for _, key := range keys {
		if node = mappingChild(node, key); node == nil {
			return nil
		}
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	seen := map[string]bool{}
	var order []string
	for i := 0; i+1 < len(node.Content); i += 2 {
		name := decodedKeyString(node.Content[i])
		if !seen[name] {
			seen[name] = true
			order = append(order, name)
		}
	}
	return order
}

// Value node a mapping holds under a key, the last occurrence winning, nil when it has none
func mappingChild(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	var found *yaml.Node
	for i := 0; i+1 < len(node.Content); i += 2 {
		if decodedKeyString(node.Content[i]) == key {
			found = derefAlias(node.Content[i+1])
		}
	}
	return found
}

package openapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	maximumOpenAPIDocumentBytes = 16 << 20
	maximumYAMLBytes            = maximumOpenAPIDocumentBytes
	maximumYAMLDepth            = 128
	maximumYAMLNodes            = 100_000
)

// decodeYAMLObject parses one bounded YAML document without invoking custom
// constructors or expanding aliases. The explicit conversion keeps the
// resulting value model identical to JSON's map[string]any representation.
func decodeYAMLObject(data []byte, document *map[string]any) error {
	if len(data) > maximumYAMLBytes {
		return fmt.Errorf("document exceeds %d-byte safety limit", maximumYAMLBytes)
	}
	if err := validateYAMLPreflight(data); err != nil {
		return err
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var root yaml.Node
	if err := decoder.Decode(&root); err != nil {
		return err
	}
	if len(root.Content) != 1 {
		return errors.New("document is empty")
	}

	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err == nil {
		return errors.New("multiple YAML documents")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("invalid trailing data: %w", err)
	}

	converter := yamlConverter{}
	value, err := converter.convert(root.Content[0], 1)
	if err != nil {
		return err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return errors.New("document root must be a mapping")
	}
	*document = object
	return nil
}

type yamlConverter struct {
	nodes int
}

func (converter *yamlConverter) convert(node *yaml.Node, depth int) (any, error) {
	if node == nil {
		return nil, errors.New("encountered an empty YAML node")
	}
	converter.nodes++
	if converter.nodes > maximumYAMLNodes {
		return nil, fmt.Errorf("document exceeds %d-node safety limit", maximumYAMLNodes)
	}
	if depth > maximumYAMLDepth {
		return nil, fmt.Errorf("document exceeds %d-level nesting limit", maximumYAMLDepth)
	}
	if node.Style&yaml.TaggedStyle != 0 {
		return nil, converter.nodeError(node, "explicit YAML tags are not supported")
	}

	switch node.Kind {
	case yaml.MappingNode:
		return converter.convertMapping(node, depth)
	case yaml.SequenceNode:
		if len(node.Content) > maximumYAMLNodes-converter.nodes {
			return nil, converter.nodeError(node, "sequence exceeds the YAML node safety limit")
		}
		result := make([]any, 0, len(node.Content))
		for _, child := range node.Content {
			value, err := converter.convert(child, depth+1)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
		return result, nil
	case yaml.ScalarNode:
		return converter.convertScalar(node)
	case yaml.AliasNode:
		return nil, converter.nodeError(node, "YAML aliases are not supported")
	default:
		return nil, converter.nodeError(node, "unsupported YAML node")
	}
}

func (converter *yamlConverter) convertMapping(node *yaml.Node, depth int) (map[string]any, error) {
	if len(node.Content)%2 != 0 {
		return nil, converter.nodeError(node, "mapping has an unmatched key")
	}
	if len(node.Content) > 2*(maximumYAMLNodes-converter.nodes) {
		return nil, converter.nodeError(node, "mapping exceeds the YAML node safety limit")
	}
	result := make(map[string]any, len(node.Content)/2)
	for index := 0; index < len(node.Content); index += 2 {
		keyNode := node.Content[index]
		converter.nodes++
		if converter.nodes > maximumYAMLNodes {
			return nil, converter.nodeError(keyNode, "document exceeds the YAML node safety limit")
		}
		if keyNode.Kind != yaml.ScalarNode || keyNode.ShortTag() != "!!str" {
			return nil, converter.nodeError(keyNode, "mapping keys must be strings")
		}
		if keyNode.Style&yaml.TaggedStyle != 0 {
			return nil, converter.nodeError(keyNode, "explicit YAML tags are not supported")
		}
		key := keyNode.Value
		if _, duplicate := result[key]; duplicate {
			return nil, converter.nodeError(keyNode, fmt.Sprintf("duplicate mapping key %q", key))
		}
		value, err := converter.convert(node.Content[index+1], depth+1)
		if err != nil {
			return nil, err
		}
		result[key] = value
	}
	return result, nil
}

func (converter *yamlConverter) convertScalar(node *yaml.Node) (any, error) {
	switch node.ShortTag() {
	case "!!str":
		return node.Value, nil
	case "!!null":
		return nil, nil
	case "!!bool":
		value, err := strconv.ParseBool(strings.ToLower(node.Value))
		if err != nil {
			return nil, converter.nodeError(node, "invalid boolean scalar")
		}
		return value, nil
	case "!!int":
		value, err := canonicalYAMLInteger(node.Value)
		if err != nil {
			return nil, converter.nodeError(node, "invalid integer scalar")
		}
		return json.Number(value), nil
	case "!!float":
		value, err := canonicalYAMLFloat(node.Value)
		if err != nil {
			return nil, converter.nodeError(node, err.Error())
		}
		return json.Number(value), nil
	case "!!timestamp":
		// OpenAPI's data model is JSON-compatible and has no timestamp scalar
		// type. YAML timestamps therefore retain their exact source spelling as
		// strings, matching JSON documents that express the same example.
		return node.Value, nil
	default:
		return nil, converter.nodeError(node, fmt.Sprintf("unsupported scalar tag %q", node.ShortTag()))
	}
}

func canonicalYAMLInteger(raw string) (string, error) {
	value := strings.ReplaceAll(strings.TrimSpace(raw), "_", "")
	sign := 1
	switch {
	case strings.HasPrefix(value, "-"):
		sign = -1
		value = strings.TrimPrefix(value, "-")
	case strings.HasPrefix(value, "+"):
		value = strings.TrimPrefix(value, "+")
	}
	base := 10
	switch {
	case strings.HasPrefix(value, "0x"), strings.HasPrefix(value, "0X"):
		base, value = 16, value[2:]
	case strings.HasPrefix(value, "0o"), strings.HasPrefix(value, "0O"):
		base, value = 8, value[2:]
	case strings.HasPrefix(value, "0b"), strings.HasPrefix(value, "0B"):
		base, value = 2, value[2:]
	}
	if value == "" {
		return "", errors.New("integer scalar is empty")
	}
	parsed, ok := new(big.Int).SetString(value, base)
	if !ok {
		return "", errors.New("integer scalar is invalid")
	}
	if sign < 0 {
		parsed.Neg(parsed)
	}
	return parsed.String(), nil
}

// validateYAMLPreflight rejects pathological flow/indent nesting and documents
// whose structural token count would exceed the post-parse node budget. This
// runs before yaml.v3 builds its node tree, reducing the parser's own maximum
// depth from 10,000 to RKC's governed 128-level ceiling.
func validateYAMLPreflight(data []byte) error {
	lines := bytes.Split(data, []byte{'\n'})
	indentLevels := []int{-1}
	flowDepth := 0
	structural := 0
	for lineIndex, line := range lines {
		indent := 0
		for indent < len(line) && line[indent] == ' ' {
			indent++
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || trimmed[0] == '#' {
			continue
		}
		for len(indentLevels) > 1 && indent < indentLevels[len(indentLevels)-1] {
			indentLevels = indentLevels[:len(indentLevels)-1]
		}
		if indent > indentLevels[len(indentLevels)-1] {
			indentLevels = append(indentLevels, indent)
			if len(indentLevels)-1 > maximumYAMLDepth {
				return fmt.Errorf(
					"line %d: document exceeds %d-level nesting limit",
					lineIndex+1,
					maximumYAMLDepth,
				)
			}
		}
		structural++
		singleQuoted := false
		doubleQuoted := false
		escaped := false
		tokenStart := true
		for index := indent; index < len(line); index++ {
			character := line[index]
			if doubleQuoted {
				if escaped {
					escaped = false
					continue
				}
				if character == '\\' {
					escaped = true
				} else if character == '"' {
					doubleQuoted = false
				}
				continue
			}
			if singleQuoted {
				if character == '\'' {
					if index+1 < len(line) && line[index+1] == '\'' {
						index++
					} else {
						singleQuoted = false
					}
				}
				continue
			}
			switch character {
			case '#':
				if tokenStart || index == 0 || line[index-1] == ' ' || line[index-1] == '\t' {
					index = len(line)
					continue
				}
			case '"':
				doubleQuoted = true
				tokenStart = false
				continue
			case '\'':
				singleQuoted = true
				tokenStart = false
				continue
			case '[', '{':
				flowDepth++
				structural++
				if flowDepth > maximumYAMLDepth {
					return fmt.Errorf(
						"line %d: document exceeds %d-level flow nesting limit",
						lineIndex+1,
						maximumYAMLDepth,
					)
				}
			case ']', '}':
				if flowDepth > 0 {
					flowDepth--
				}
			case ':', ',':
				structural++
			case '!':
				if tokenStart && index+1 < len(line) &&
					(line[index+1] == ' ' || line[index+1] == '\t') {
					return fmt.Errorf(
						"line %d, column %d: explicit YAML tags are not supported",
						lineIndex+1,
						index+1,
					)
				}
			}
			if character == ' ' || character == '\t' ||
				character == '-' || character == '?' || character == ':' ||
				character == ',' || character == '[' || character == '{' {
				tokenStart = true
			} else {
				tokenStart = false
			}
			if structural > maximumYAMLNodes {
				return fmt.Errorf(
					"line %d: document exceeds %d structural-token safety limit",
					lineIndex+1,
					maximumYAMLNodes,
				)
			}
		}
	}
	if flowDepth != 0 {
		// The parser will provide the detailed syntax diagnostic, but returning
		// here prevents it from receiving a deeply unterminated flow document.
		return errors.New("unterminated YAML flow collection")
	}
	return nil
}

func canonicalYAMLFloat(raw string) (string, error) {
	value := strings.ReplaceAll(strings.TrimSpace(raw), "_", "")
	lower := strings.ToLower(value)
	switch lower {
	case ".nan", "+.nan", "-.nan", ".inf", "+.inf", "-.inf":
		return "", errors.New("non-finite floating-point scalars are not supported")
	}
	if strings.HasPrefix(value, "+") {
		value = strings.TrimPrefix(value, "+")
	}
	if strings.HasPrefix(value, ".") {
		value = "0" + value
	} else if strings.HasPrefix(value, "-.") {
		value = "-0" + strings.TrimPrefix(value, "-")
	}
	if strings.HasSuffix(value, ".") {
		value += "0"
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsInf(parsed, 0) || math.IsNaN(parsed) {
		return "", errors.New("invalid floating-point scalar")
	}
	if !json.Valid([]byte(value)) {
		return "", errors.New("floating-point scalar is not JSON-compatible")
	}
	return value, nil
}

func (converter *yamlConverter) nodeError(node *yaml.Node, message string) error {
	if node.Line > 0 {
		return fmt.Errorf("line %d, column %d: %s", node.Line, node.Column, message)
	}
	return errors.New(message)
}

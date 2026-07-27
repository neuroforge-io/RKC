package scipindex

import (
	"errors"
	"fmt"
)

type metadata struct {
	toolName     string
	toolVersion  string
	projectRoot  string
	textEncoding int32
}

type document struct {
	path             string
	language         string
	text             string
	positionEncoding int32
	occurrences      []occurrence
	symbols          []symbolInformation
}

type occurrence struct {
	rangeValues           []int32
	typedRange            *sourcePosition
	symbol                string
	roles                 int32
	overrideDocumentation []string
	syntaxKind            int32
	diagnostics           []compilerDiagnostic
	enclosingRange        []int32
	typedEnclosingRange   *sourcePosition
}

type sourcePosition struct {
	startLine      int32
	startCharacter int32
	endLine        int32
	endCharacter   int32
}

type symbolInformation struct {
	symbol          string
	documentation   []string
	relationships   []relationship
	kind            int32
	displayName     string
	signature       string
	signatureLang   string
	enclosingSymbol string
}

type relationship struct {
	symbol           string
	isReference      bool
	isImplementation bool
	isTypeDefinition bool
	isDefinition     bool
}

type compilerDiagnostic struct {
	severity int32
	code     string
	message  string
	source   string
}

func parseMetadata(data []byte) (metadata, error) {
	var result metadata
	reader := newMessageReader(data)
	for {
		field, wire, done, err := reader.next()
		if err != nil || done {
			return result, err
		}
		switch field {
		case 2:
			if err := requireWire(field, wire, 2); err != nil {
				return result, err
			}
			message, err := reader.bytes(maximumMessageBytes)
			if err != nil {
				return result, err
			}
			result.toolName, result.toolVersion, err = parseToolInfo(message)
			if err != nil {
				return result, err
			}
		case 3:
			if err := requireWire(field, wire, 2); err != nil {
				return result, err
			}
			result.projectRoot, err = reader.string()
			if err != nil {
				return result, err
			}
		case 4:
			if err := requireWire(field, wire, 0); err != nil {
				return result, err
			}
			value, err := reader.varint()
			if err != nil {
				return result, err
			}
			result.textEncoding = int32(value)
		default:
			if err := reader.skip(wire); err != nil {
				return result, err
			}
		}
	}
}

func parseToolInfo(data []byte) (name, version string, err error) {
	reader := newMessageReader(data)
	for {
		field, wire, done, nextErr := reader.next()
		if nextErr != nil || done {
			return name, version, nextErr
		}
		switch field {
		case 1:
			if err = requireWire(field, wire, 2); err == nil {
				name, err = reader.string()
			}
		case 2:
			if err = requireWire(field, wire, 2); err == nil {
				version, err = reader.string()
			}
		default:
			err = reader.skip(wire)
		}
		if err != nil {
			return name, version, err
		}
	}
}

func parseDocument(data []byte) (document, error) {
	var result document
	reader := newMessageReader(data)
	for {
		field, wire, done, err := reader.next()
		if err != nil || done {
			return result, err
		}
		switch field {
		case 1, 4, 5:
			if err := requireWire(field, wire, 2); err != nil {
				return result, err
			}
			value, err := reader.string()
			if err != nil {
				return result, err
			}
			switch field {
			case 1:
				result.path = value
			case 4:
				result.language = value
			case 5:
				result.text = value
			}
		case 2:
			if err := requireWire(field, wire, 2); err != nil {
				return result, err
			}
			message, err := reader.bytes(maximumMessageBytes)
			if err != nil {
				return result, err
			}
			value, err := parseOccurrence(message)
			if err != nil {
				return result, err
			}
			result.occurrences = append(result.occurrences, value)
		case 3:
			if err := requireWire(field, wire, 2); err != nil {
				return result, err
			}
			message, err := reader.bytes(maximumMessageBytes)
			if err != nil {
				return result, err
			}
			value, err := parseSymbolInformation(message)
			if err != nil {
				return result, err
			}
			result.symbols = append(result.symbols, value)
		case 6:
			if err := requireWire(field, wire, 0); err != nil {
				return result, err
			}
			value, err := reader.varint()
			if err != nil {
				return result, err
			}
			result.positionEncoding = int32(value)
		default:
			if err := reader.skip(wire); err != nil {
				return result, err
			}
		}
	}
}

func parseOccurrence(data []byte) (occurrence, error) {
	var result occurrence
	reader := newMessageReader(data)
	for {
		field, wire, done, err := reader.next()
		if err != nil || done {
			return result, err
		}
		switch field {
		case 1, 7:
			var values []int32
			switch wire {
			case 0:
				value, err := reader.varint()
				if err != nil {
					return result, err
				}
				values = []int32{int32(value)}
			case 2:
				packed, err := reader.bytes(64)
				if err != nil {
					return result, err
				}
				values, err = parsePackedInt32(packed)
				if err != nil {
					return result, err
				}
			default:
				return result, requireWire(field, wire, 2)
			}
			if field == 1 {
				result.rangeValues = append(result.rangeValues, values...)
				if len(result.rangeValues) > maximumRangeValues {
					return result, errors.New("SCIP range has more than four values")
				}
			} else {
				result.enclosingRange = append(result.enclosingRange, values...)
				if len(result.enclosingRange) > maximumRangeValues {
					return result, errors.New("SCIP enclosing range has more than four values")
				}
			}
		case 2:
			if err := requireWire(field, wire, 2); err != nil {
				return result, err
			}
			result.symbol, err = reader.string()
			if err != nil {
				return result, err
			}
		case 3, 5:
			if err := requireWire(field, wire, 0); err != nil {
				return result, err
			}
			value, err := reader.varint()
			if err != nil {
				return result, err
			}
			if field == 3 {
				result.roles = int32(value)
			} else {
				result.syntaxKind = int32(value)
			}
		case 4:
			if err := requireWire(field, wire, 2); err != nil {
				return result, err
			}
			value, err := reader.string()
			if err != nil {
				return result, err
			}
			result.overrideDocumentation = append(result.overrideDocumentation, value)
		case 6:
			if err := requireWire(field, wire, 2); err != nil {
				return result, err
			}
			message, err := reader.bytes(maximumMessageBytes)
			if err != nil {
				return result, err
			}
			value, err := parseCompilerDiagnostic(message)
			if err != nil {
				return result, err
			}
			result.diagnostics = append(result.diagnostics, value)
		case 8, 9, 10, 11:
			if err := requireWire(field, wire, 2); err != nil {
				return result, err
			}
			message, err := reader.bytes(128)
			if err != nil {
				return result, err
			}
			var value sourcePosition
			if field == 8 || field == 10 {
				value, err = parseSingleLineRange(message)
			} else {
				value, err = parseMultiLineRange(message)
			}
			if err != nil {
				return result, err
			}
			if field == 8 || field == 9 {
				result.typedRange = &value
			} else {
				result.typedEnclosingRange = &value
			}
		default:
			if err := reader.skip(wire); err != nil {
				return result, err
			}
		}
	}
}

func parseSingleLineRange(data []byte) (sourcePosition, error) {
	values, err := parseNumericMessage(data, 3)
	return sourcePosition{
		startLine: values[0], startCharacter: values[1],
		endLine: values[0], endCharacter: values[2],
	}, err
}

func parseMultiLineRange(data []byte) (sourcePosition, error) {
	values, err := parseNumericMessage(data, 4)
	return sourcePosition{
		startLine: values[0], startCharacter: values[1],
		endLine: values[2], endCharacter: values[3],
	}, err
}

func parseNumericMessage(data []byte, fields int) ([]int32, error) {
	values := make([]int32, fields)
	reader := newMessageReader(data)
	for {
		field, wire, done, err := reader.next()
		if err != nil || done {
			return values, err
		}
		if field >= 1 && field <= fields {
			if err := requireWire(field, wire, 0); err != nil {
				return values, err
			}
			value, err := reader.varint()
			if err != nil {
				return values, err
			}
			values[field-1] = int32(value)
		} else if err := reader.skip(wire); err != nil {
			return values, err
		}
	}
}

func parseSymbolInformation(data []byte) (symbolInformation, error) {
	var result symbolInformation
	reader := newMessageReader(data)
	for {
		field, wire, done, err := reader.next()
		if err != nil || done {
			return result, err
		}
		switch field {
		case 1, 3, 6, 8:
			if err := requireWire(field, wire, 2); err != nil {
				return result, err
			}
			value, err := reader.string()
			if err != nil {
				return result, err
			}
			switch field {
			case 1:
				result.symbol = value
			case 3:
				result.documentation = append(result.documentation, value)
			case 6:
				result.displayName = value
			case 8:
				result.enclosingSymbol = value
			}
		case 4:
			if err := requireWire(field, wire, 2); err != nil {
				return result, err
			}
			message, err := reader.bytes(maximumMessageBytes)
			if err != nil {
				return result, err
			}
			value, err := parseRelationship(message)
			if err != nil {
				return result, err
			}
			result.relationships = append(result.relationships, value)
		case 5:
			if err := requireWire(field, wire, 0); err != nil {
				return result, err
			}
			value, err := reader.varint()
			if err != nil {
				return result, err
			}
			result.kind = int32(value)
		case 7:
			if err := requireWire(field, wire, 2); err != nil {
				return result, err
			}
			message, err := reader.bytes(maximumMessageBytes)
			if err != nil {
				return result, err
			}
			result.signatureLang, result.signature, err = parseSignature(message)
			if err != nil {
				return result, err
			}
		default:
			if err := reader.skip(wire); err != nil {
				return result, err
			}
		}
	}
}

func parseRelationship(data []byte) (relationship, error) {
	var result relationship
	reader := newMessageReader(data)
	for {
		field, wire, done, err := reader.next()
		if err != nil || done {
			return result, err
		}
		switch field {
		case 1:
			if err := requireWire(field, wire, 2); err != nil {
				return result, err
			}
			result.symbol, err = reader.string()
			if err != nil {
				return result, err
			}
		case 2, 3, 4, 5:
			if err := requireWire(field, wire, 0); err != nil {
				return result, err
			}
			value, err := reader.varint()
			if err != nil {
				return result, err
			}
			switch field {
			case 2:
				result.isReference = value != 0
			case 3:
				result.isImplementation = value != 0
			case 4:
				result.isTypeDefinition = value != 0
			case 5:
				result.isDefinition = value != 0
			}
		default:
			if err := reader.skip(wire); err != nil {
				return result, err
			}
		}
	}
}

func parseSignature(data []byte) (language, text string, err error) {
	reader := newMessageReader(data)
	for {
		field, wire, done, nextErr := reader.next()
		if nextErr != nil || done {
			return language, text, nextErr
		}
		switch field {
		case 4, 5:
			if err = requireWire(field, wire, 2); err == nil {
				var value string
				value, err = reader.string()
				if field == 4 {
					language = value
				} else {
					text = value
				}
			}
		default:
			err = reader.skip(wire)
		}
		if err != nil {
			return language, text, err
		}
	}
}

func parseCompilerDiagnostic(data []byte) (compilerDiagnostic, error) {
	var result compilerDiagnostic
	reader := newMessageReader(data)
	for {
		field, wire, done, err := reader.next()
		if err != nil || done {
			return result, err
		}
		switch field {
		case 1:
			if err := requireWire(field, wire, 0); err != nil {
				return result, err
			}
			value, err := reader.varint()
			if err != nil {
				return result, err
			}
			result.severity = int32(value)
		case 2, 3, 4:
			if err := requireWire(field, wire, 2); err != nil {
				return result, err
			}
			value, err := reader.string()
			if err != nil {
				return result, err
			}
			switch field {
			case 2:
				result.code = value
			case 3:
				result.message = value
			case 4:
				result.source = value
			}
		default:
			if err := reader.skip(wire); err != nil {
				return result, err
			}
		}
	}
}

func occurrenceRange(value occurrence) (sourcePosition, bool, error) {
	if value.typedRange != nil {
		if err := validatePosition(*value.typedRange); err != nil {
			return sourcePosition{}, false, err
		}
		return *value.typedRange, true, nil
	}
	return legacyRange(value.rangeValues)
}

func occurrenceEnclosingRange(value occurrence) (sourcePosition, bool, error) {
	if value.typedEnclosingRange != nil {
		if err := validatePosition(*value.typedEnclosingRange); err != nil {
			return sourcePosition{}, false, err
		}
		return *value.typedEnclosingRange, true, nil
	}
	return legacyRange(value.enclosingRange)
}

func legacyRange(values []int32) (sourcePosition, bool, error) {
	if len(values) == 0 {
		return sourcePosition{}, false, nil
	}
	var result sourcePosition
	switch len(values) {
	case 3:
		result = sourcePosition{
			startLine: values[0], startCharacter: values[1],
			endLine: values[0], endCharacter: values[2],
		}
	case 4:
		result = sourcePosition{
			startLine: values[0], startCharacter: values[1],
			endLine: values[2], endCharacter: values[3],
		}
	default:
		return sourcePosition{}, false, fmt.Errorf("SCIP range has %d values; want 3 or 4", len(values))
	}
	if err := validatePosition(result); err != nil {
		return sourcePosition{}, false, err
	}
	return result, true, nil
}

func validatePosition(value sourcePosition) error {
	if value.startLine < 0 || value.startCharacter < 0 ||
		value.endLine < 0 || value.endCharacter < 0 {
		return errors.New("SCIP range contains a negative position")
	}
	if value.endLine < value.startLine ||
		value.endLine == value.startLine && value.endCharacter < value.startCharacter {
		return errors.New("SCIP range end precedes its start")
	}
	return nil
}

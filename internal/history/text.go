package history

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// EscapeTerminalText returns text that cannot emit terminal control sequences.
// It is defense in depth for paths supplied on the command line; compiled
// history text is rejected earlier when it contains a control or format rune.
func EscapeTerminalText(value string) string {
	var result strings.Builder
	result.Grow(len(value))
	for len(value) > 0 {
		character, size := utf8.DecodeRuneInString(value)
		if character == utf8.RuneError && size == 1 {
			_, _ = fmt.Fprintf(&result, "\\x%02X", value[0])
			value = value[1:]
			continue
		}
		if unsafeTextRune(character) {
			if character <= 0xffff {
				_, _ = fmt.Fprintf(&result, "\\u%04X", character)
			} else {
				_, _ = fmt.Fprintf(&result, "\\U%08X", character)
			}
		} else {
			result.WriteRune(character)
		}
		value = value[size:]
	}
	return result.String()
}

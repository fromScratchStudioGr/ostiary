// Package jsonscan reads the top level of a JSON object without decoding or
// copying it, so a large request body can be inspected, and patched, at the
// cost of one pass over its bytes.
package jsonscan

import (
	"bytes"
	"encoding/json"
	"errors"
	"unicode/utf8"
)

var errMalformed = errors.New("jsonscan: malformed JSON object")

// TopLevel calls fn for each member of the object body holds, in order, with
// its key (unescaped) and the bounds of its raw value, body[start:end]. A key
// that appears more than once is reported each time.
//
// It only checks the structure it walks through, not every value: a body
// that isn't valid JSON may still be reported on, so callers that act on
// untrusted input should only use it on a body already known to be valid.
func TopLevel(body []byte, fn func(key []byte, start, end int)) error {
	i := skipSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return errMalformed
	}
	i = skipSpace(body, i+1)
	if i < len(body) && body[i] == '}' {
		return onlySpaceAfter(body, i+1)
	}

	for {
		if i >= len(body) || body[i] != '"' {
			return errMalformed
		}
		keyEnd, err := stringEnd(body, i)
		if err != nil {
			return err
		}
		key := body[i+1 : keyEnd]
		// Escapes are decoded, and invalid UTF-8 replaced, as a JSON decoder
		// would, so the key is the one a decoder sees.
		if bytes.IndexByte(key, '\\') >= 0 || !utf8.Valid(key) {
			var unescaped string
			if err := json.Unmarshal(body[i:keyEnd+1], &unescaped); err != nil {
				return errMalformed
			}
			key = []byte(unescaped)
		}

		i = skipSpace(body, keyEnd+1)
		if i >= len(body) || body[i] != ':' {
			return errMalformed
		}
		start := skipSpace(body, i+1)
		end, err := valueEnd(body, start)
		if err != nil {
			return err
		}
		fn(key, start, end)

		i = skipSpace(body, end)
		if i >= len(body) {
			return errMalformed
		}
		switch body[i] {
		case ',':
			i = skipSpace(body, i+1)
		case '}':
			return onlySpaceAfter(body, i+1)
		default:
			return errMalformed
		}
	}
}

// stringEnd returns the index of the quote closing the string that opens at
// body[start].
func stringEnd(body []byte, start int) (int, error) {
	from := start + 1
	for {
		n := bytes.IndexByte(body[from:], '"')
		if n < 0 {
			return 0, errMalformed
		}
		end := from + n
		// A quote preceded by an odd number of backslashes is escaped.
		backslashes := 0
		for k := end - 1; k > start && body[k] == '\\'; k-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			return end, nil
		}
		from = end + 1
	}
}

// valueEnd returns the index just past the value that starts at body[start].
func valueEnd(body []byte, start int) (int, error) {
	if start >= len(body) {
		return 0, errMalformed
	}
	switch body[start] {
	case '"':
		end, err := stringEnd(body, start)
		return end + 1, err
	case '{', '[':
		depth := 0
		for j := start; j < len(body); j++ {
			switch body[j] {
			case '"':
				end, err := stringEnd(body, j)
				if err != nil {
					return 0, err
				}
				j = end
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1, nil
				}
			}
		}
		return 0, errMalformed
	default:
		// A number, true, false or null runs up to the next delimiter.
		j := start
		for j < len(body) && !isDelimiter(body[j]) {
			j++
		}
		if j == start {
			return 0, errMalformed
		}
		return j, nil
	}
}

func isDelimiter(c byte) bool {
	switch c {
	case ',', '}', ']', ' ', '\t', '\n', '\r':
		return true
	}
	return false
}

func skipSpace(body []byte, i int) int {
	for i < len(body) {
		switch body[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

func onlySpaceAfter(body []byte, i int) error {
	if skipSpace(body, i) != len(body) {
		return errMalformed
	}
	return nil
}

package ctxwin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"
)

// How a shortened result tells the model where it can get the rest.
const (
	HowCommand  = "run the command again, narrowed, if you need them" // D40's words
	HowResource = "read the resource again if you need it"
)

// Shorten keeps the head (two thirds of limit) and the tail (a third) of an earlier tool result and says what was left
// out and how to get it again (D40). A text of at most limit bytes is returned as it is.
func Shorten(s string, limit int, how string) string {
	if len(s) <= limit {
		return s
	}
	head, tail := limit*2/3, limit/3
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	start := len(s) - tail
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[:head] + fmt.Sprintf("\n[… %d bytes of this earlier result are not shown; %s …]\n", start-head, how) + s[start:]
}

// argumentMarker is what a shortened argument value says. It is inside the JSON string, so that the arguments stay what
// every provider requires them to be (an object, valid JSON), and it says that the call ran with the whole value.
const argumentMarker = "\n[… %d bytes of this earlier argument value are not shown. The call was made with the whole value; only this view of it is shortened, so do not take what is left for what was sent …]\n"

// ShortenArguments shortens every string value of a tool call's arguments (a JSON object) that is longer than limit
// bytes, anywhere in it, to its head and tail with the marker above. Keys, other values and their order are kept, and the
// result is valid JSON whenever the input is. It returns the arguments unchanged, and false, when nothing is shortened,
// when the input is not valid JSON, or when the result would not be smaller.
func ShortenArguments(args string, limit int) (string, bool) {
	if len(args) <= limit {
		return args, false
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(args)))
	dec.UseNumber()
	var out bytes.Buffer
	changed := false
	if err := copyValue(dec, &out, limit, &changed); err != nil || !changed {
		return args, false
	}
	if _, err := dec.Token(); err != io.EOF { // trailing data: not what a provider sent
		return args, false
	}
	if out.Len() >= len(args) {
		return args, false
	}
	return out.String(), true
}

func encodeString(out *bytes.Buffer, s string) {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	out.Truncate(out.Len() - 1) // the newline Encode adds
}

func copyValue(dec *json.Decoder, out *bytes.Buffer, limit int, changed *bool) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch v := tok.(type) {
	case json.Delim:
		closer := byte('}')
		if v == '[' {
			closer = ']'
		}
		out.WriteByte(byte(v))
		first := true
		for dec.More() {
			if !first {
				out.WriteByte(',')
			}
			first = false
			if v == '{' {
				k, err := dec.Token()
				if err != nil {
					return err
				}
				key, _ := k.(string)
				encodeString(out, key)
				out.WriteByte(':')
			}
			if err := copyValue(dec, out, limit, changed); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
		out.WriteByte(closer)
	case string:
		if len(v) > limit {
			v = shortenValue(v, limit)
			*changed = true
		}
		encodeString(out, v)
	case json.Number:
		out.WriteString(v.String())
	case bool:
		fmt.Fprint(out, v)
	case nil:
		out.WriteString("null")
	}
	return nil
}

func shortenValue(s string, limit int) string {
	head, tail := limit*2/3, limit/3
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	start := len(s) - tail
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[:head] + fmt.Sprintf(argumentMarker, start-head) + s[start:]
}

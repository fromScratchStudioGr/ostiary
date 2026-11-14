package jsonscan

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"
)

type member struct{ key, value string }

// members runs TopLevel on body and returns what it reported.
func members(body string) ([]member, error) {
	var got []member
	err := TopLevel([]byte(body), func(key []byte, start, end int) {
		got = append(got, member{string(key), body[start:end]})
	})
	return got, err
}

func TestTopLevel(t *testing.T) {
	t.Run("reports each member in order, with its key and raw value", func(t *testing.T) {
		body := ` { "model" : "gpt-6-luna" ,
			"n":-1.5e+3, "stream":true, "store":false, "user":null,
			"metadata":{"model":"nested","a":[1,{"b":"}"}]},
			"messages":[{"role":"user","content":"say \"]}\" and \\"}],
			"empty":{}, "list":[] } `

		got, err := members(body)
		if err != nil {
			t.Fatalf("TopLevel: %v", err)
		}

		want := []member{
			{"model", `"gpt-6-luna"`},
			{"n", `-1.5e+3`},
			{"stream", `true`},
			{"store", `false`},
			{"user", `null`},
			{"metadata", `{"model":"nested","a":[1,{"b":"}"}]}`},
			{"messages", `[{"role":"user","content":"say \"]}\" and \\"}]`},
			{"empty", `{}`},
			{"list", `[]`},
		}
		if !slices.Equal(got, want) {
			t.Errorf("got  %q\nwant %q", got, want)
		}
	})

	t.Run("unescapes keys, as a JSON decoder would read them", func(t *testing.T) {
		got, err := members(`{"model":1,"a\"b":2,"c\\":3,"\\\"":4}`)
		if err != nil {
			t.Fatalf("TopLevel: %v", err)
		}

		want := []member{{"model", "1"}, {`a"b`, "2"}, {`c\`, "3"}, {`\"`, "4"}}
		if !slices.Equal(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("reports a repeated key each time", func(t *testing.T) {
		got, err := members(`{"model":"a","model":"b"}`)
		if err != nil {
			t.Fatalf("TopLevel: %v", err)
		}

		if want := []member{{"model", `"a"`}, {"model", `"b"`}}; !slices.Equal(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("reports nothing for an empty object", func(t *testing.T) {
		got, err := members(" {\n} ")
		if err != nil || len(got) != 0 {
			t.Errorf("got %q, %v; want nothing and no error", got, err)
		}
	})

	t.Run("returns an error for a body it can't walk", func(t *testing.T) {
		for name, body := range map[string]string{
			"nothing":                      "",
			"only whitespace":              "  ",
			"an array":                     `[{"model":"a"}]`,
			"a string":                     `"model"`,
			"an unclosed object":           `{"model":"a"`,
			"an unclosed nested value":     `{"messages":[{"a":1}`,
			"an unterminated key":          `{"model`,
			"an unterminated string":       `{"model":"a}`,
			"a key with a bad escape":      `{"mod\qel":"a"}`,
			"a key that isn't a string":    `{model:"a"}`,
			"a missing colon":              `{"model" "a"}`,
			"a missing value":              `{"model":}`,
			"a missing comma":              `{"model":"a" "n":1}`,
			"a trailing comma":             `{"model":"a",}`,
			"data after the object":        `{"model":"a"}{"model":"b"}`,
			"a value cut off at the end":   `{"model":`,
			"an escaped quote at the end":  `{"model":"a\"}`,
			"a closing brace and no value": `{"model"}`,
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := members(body); err == nil {
					t.Errorf("TopLevel(%q) succeeded, want an error", body)
				}
			})
		}
	})
}

// FuzzTopLevel checks that TopLevel never panics, and that on every valid
// JSON object it reports exactly the members a JSON decoder sees, in order.
func FuzzTopLevel(f *testing.F) {
	for _, seed := range []string{
		`{}`,
		`{"model":"a","stream":true,"stream_options":{"include_usage":false}}`,
		`{"model":"a","model":"b","Model":"c"}`,
		`{"model":"a","a\"b":[1,"]}",{"c":"\\"}],"n":-0.5e10}`,
		` { "x" : null , "y" : [ ] } `,
		`{"model":"a"`,
		`[1,2]`,
		`{"a":"\"}`,
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		var got []member
		err := TopLevel(body, func(key []byte, start, end int) {
			got = append(got, member{string(key), string(body[start:end])})
		})

		want, isObject := decoderMembers(body)
		if !isObject {
			return
		}
		if err != nil {
			t.Fatalf("TopLevel(%q): %v, but it is a valid JSON object", body, err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("TopLevel(%q)\ngot  %q\nwant %q", body, got, want)
		}
	})
}

// decoderMembers lists the top-level members of body as encoding/json reads
// them, and whether body is a valid JSON object at all.
func decoderMembers(body []byte) ([]member, bool) {
	if !json.Valid(body) {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if token, _ := dec.Token(); token != json.Delim('{') {
		return nil, false
	}
	var got []member
	for dec.More() {
		token, _ := dec.Token()
		var value json.RawMessage
		dec.Decode(&value)
		got = append(got, member{token.(string), string(value)})
	}
	return got, true
}

package jsonc

import (
	"encoding/json"
	"strings"
	"testing"
)

// get decodes doc and walks path, failing the test if doc isn't valid
// JSONC — every result of Set must stay readable by a real parser.
func get(t *testing.T, doc []byte, path ...string) any {
	t.Helper()
	var v any
	if err := Unmarshal(doc, &v); err != nil {
		t.Fatalf("result is not valid JSONC: %v\n%s", err, doc)
	}
	for _, k := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func mustSet(t *testing.T, doc string, value any, path ...string) string {
	t.Helper()
	out, err := Set([]byte(doc), path, value)
	if err != nil {
		t.Fatalf("Set(%v): %v\n%s", path, err, doc)
	}
	if got, _ := json.Marshal(get(t, out, path...)); string(got) != mustJSON(t, value) {
		t.Fatalf("after Set(%v), value = %s, want %s\n%s", path, got, mustJSON(t, value), out)
	}
	return string(out)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The point of the package: an edit leaves comments, trailing commas and
// unrelated layout exactly where they were.
func TestSetKeepsCommentsAndLayout(t *testing.T) {
	doc := `{
  "$schema": "https://opencode.ai/config.json",
  // Why these are off: they bill straight to the vendor.
  "disabled_providers": ["openai", "anthropic"], // trailing note
  /* block
     comment */
  "provider": {
    "gaia": {
      "npm": "@ai-sdk/openai-compatible", // local llama-swap
      "options": { "baseURL": "http://127.0.0.1:8080/v1" },
    },
  },
}
`
	out := mustSet(t, doc, "http://127.0.0.1:7331/v1", "provider", "prism-anthropic", "options", "baseURL")
	for _, keep := range []string{
		"// Why these are off: they bill straight to the vendor.",
		`"disabled_providers": ["openai", "anthropic"], // trailing note`,
		"/* block\n     comment */",
		`"npm": "@ai-sdk/openai-compatible", // local llama-swap`,
		`"options": { "baseURL": "http://127.0.0.1:8080/v1" },`,
	} {
		if !strings.Contains(out, keep) {
			t.Errorf("lost %q:\n%s", keep, out)
		}
	}
	want := `    },
    "prism-anthropic": {
      "options": {
        "baseURL": "http://127.0.0.1:7331/v1"
      }
    }
  },
}`
	if !strings.Contains(out, want) {
		t.Errorf("new member not laid out as expected; want\n%s\ngot\n%s", want, out)
	}
	if get(t, []byte(out), "provider", "gaia", "npm") != "@ai-sdk/openai-compatible" {
		t.Error("sibling provider changed")
	}
}

func TestSetReplacesExistingValue(t *testing.T) {
	doc := `{
  "provider": {
    "prism-anthropic": {
      "options": {
        "baseURL": "http://127.0.0.1:7000/v1", // old port
        "apiKey": "teleport"
      }
    }
  }
}`
	out := mustSet(t, doc, "http://127.0.0.1:7331/v1", "provider", "prism-anthropic", "options", "baseURL")
	if !strings.Contains(out, `"baseURL": "http://127.0.0.1:7331/v1", // old port`) {
		t.Errorf("value not replaced in place:\n%s", out)
	}
	if strings.Count(out, "\n") != strings.Count(doc, "\n") {
		t.Errorf("replacing a scalar changed the line count:\n%s", out)
	}
}

// A member added after a last member that carries a same-line comment
// goes after the comment, so the comment stays with its own line.
func TestSetAfterCommentedLastMember(t *testing.T) {
	doc := "{\n  \"a\": 1 // about a\n}\n"
	out := mustSet(t, doc, 2, "b")
	if want := "{\n  \"a\": 1, // about a\n  \"b\": 2\n}\n"; out != want {
		t.Errorf("got\n%q\nwant\n%q", out, want)
	}
}

func TestSetWithTrailingComma(t *testing.T) {
	doc := "{\n  \"a\": 1,\n}\n"
	out := mustSet(t, doc, 2, "b")
	if want := "{\n  \"a\": 1,\n  \"b\": 2\n}\n"; out != want {
		t.Errorf("got\n%q\nwant\n%q", out, want)
	}
}

func TestSetIntoEmptyAndOneLineObjects(t *testing.T) {
	cases := []struct{ name, doc, want string }{
		{"empty", `{}`, "{\n  \"a\": 1\n}"},
		{"spaced", "{\n}\n", "{\n  \"a\": 1\n}\n"},
		{"one line", `{"x": true}`, `{"x": true, "a": 1}`},
		{"one line trailing comma", `{"x": true,}`, `{"x": true, "a": 1}`},
		{"blank document", "", "{\n  \"a\": 1\n}\n"},
		{"comment only", "// nothing yet\n", "// nothing yet\n{\n  \"a\": 1\n}\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if out := mustSet(t, c.doc, 1, "a"); out != c.want {
				t.Errorf("got\n%q\nwant\n%q", out, c.want)
			}
		})
	}
}

// Objects are written one member per line at the right depth, so an
// --overwrite of a whole block reads like the rest of the file.
func TestSetObjectValueIsIndented(t *testing.T) {
	doc := "{\n\t\"provider\": {\n\t\t\"old\": {}\n\t}\n}\n"
	value := map[string]any{"prism": map[string]any{"npm": "@ai-sdk/anthropic"}}
	out := mustSet(t, doc, value, "provider")
	want := "{\n\t\"provider\": {\n\t\t\"prism\": {\n\t\t\t\"npm\": \"@ai-sdk/anthropic\"\n\t\t}\n\t}\n}\n"
	if out != want {
		t.Errorf("got\n%q\nwant\n%q", out, want)
	}
}

// A non-object in the way is replaced, since the path demands an object.
func TestSetReplacesNonObjectOnPath(t *testing.T) {
	out := mustSet(t, `{"provider": null}`, "x", "provider", "p", "npm")
	if get(t, []byte(out), "provider", "p", "npm") != "x" {
		t.Errorf("got %s", out)
	}
}

// The last of duplicate keys wins in JSON parsers, so that is the one
// to edit.
func TestSetEditsLastDuplicate(t *testing.T) {
	out := mustSet(t, `{"a": 1, "a": 2}`, 3, "a")
	if out != `{"a": 1, "a": 3}` {
		t.Errorf("got %s", out)
	}
}

func TestSetStringsWithCommentLookalikes(t *testing.T) {
	doc := `{"url": "http://x//y/*z*/", "s": "a\"b,}"}`
	out := mustSet(t, doc, 1, "n")
	if get(t, []byte(out), "url") != "http://x//y/*z*/" || get(t, []byte(out), "s") != `a"b,}` {
		t.Errorf("strings damaged: %s", out)
	}
}

func TestSetRejectsInvalidDocuments(t *testing.T) {
	for _, doc := range []string{`[1]`, `{"a": }`, `{"a": 1`, `{"a" 1}`, `{"a": 1} x`, `{"a": 1 "b": 2}`} {
		if _, err := Set([]byte(doc), []string{"a"}, 1); err == nil {
			t.Errorf("Set accepted %q", doc)
		}
	}
}

func TestStandardize(t *testing.T) {
	doc := "{\n  // c\n  \"a\": [1, 2,], /* d */\n  \"b\": \"//not a comment\",\n}"
	var v map[string]any
	if err := Unmarshal([]byte(doc), &v); err != nil {
		t.Fatal(err)
	}
	if v["b"] != "//not a comment" || len(v["a"].([]any)) != 2 {
		t.Errorf("got %v", v)
	}
	if len(Standardize([]byte(doc))) != len(doc) {
		t.Error("Standardize changed the length, so offsets no longer line up")
	}
}

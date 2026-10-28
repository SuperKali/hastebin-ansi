package mcpserver

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// skipValue takes any JSON value and keeps none of it, so the names of the
// arguments can be read without copying a large content
type skipValue struct{}

func (*skipValue) UnmarshalJSON([]byte) error { return nil }

// argNames returns the JSON names of the fields of T, in field order: the
// properties of the schema the SDK builds from T
func argNames[T any]() []string {
	var names []string
	t := reflect.TypeFor[T]()
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		names = append(names, name)
	}
	return names
}

// decodeArgs decodes the arguments of a tool that reads them itself into v,
// as strictly as the SDK checks a typed tool against its schema, where
// additionalProperties is false: each name must be a property of T, spelled
// exactly. It returns the names that are not, sorted, and leaves v alone then.
func decodeArgs[T any](raw json.RawMessage, v *T) (unknown []string, err error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var fields map[string]skipValue
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	known := argNames[T]()
	for name := range fields {
		if !slices.Contains(known, name) {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return unknown, nil
	}
	return nil, json.Unmarshal(raw, v)
}

// ownerOnlyArgs are the upload_log arguments that only an owned paste has:
// those of a registered user's upload_log that the anonymous one lacks
var ownerOnlyArgs = func() []string {
	var out []string
	anonymous := argNames[UploadInput]()
	for _, name := range argNames[UserUploadInput]() {
		if !slices.Contains(anonymous, name) {
			out = append(out, name)
		}
	}
	return out
}()

// argsError is the tool error for arguments the schema of tool does not
// name. takes lists the arguments it does; stores tells whether the tool
// stores something, so the error can say that nothing was.
func argsError(tool string, unknown, takes []string, stores bool) *mcp.CallToolResult {
	quoted := make([]string, len(unknown))
	for i, name := range unknown {
		quoted[i] = `"` + name + `"`
	}
	noun := "argument"
	if len(unknown) > 1 {
		noun = "arguments"
	}
	what := ""
	if stores {
		what = ", so nothing was stored"
	}
	return toolError("unknown %s %s%s: %s takes only %s", noun, joinWords(quoted), what, tool, joinWords(takes))
}

// anonymousArgsError is the tool error of the anonymous upload_log for
// arguments its schema does not name. An argument only an owned paste has
// says that it needs a token: without one, nothing may fall back to a
// public paste.
func anonymousArgsError(unknown []string) *mcp.CallToolResult {
	var owner []string
	for _, name := range unknown {
		if slices.Contains(ownerOnlyArgs, name) {
			owner = append(owner, name)
		}
	}
	if len(owner) == 0 {
		return argsError("upload_log", unknown, argNames[UploadInput](), true)
	}
	verb, pronoun := "needs", "it"
	if len(owner) > 1 {
		verb, pronoun = "need", "them"
	}
	return toolError("%s %s a personal token, sent in an Authorization: Bearer header, so nothing was stored. "+
		"Without a token every paste is public: leave %s out to upload a public paste.", joinWords(owner), verb, pronoun)
}

// joinWords joins words as "a", "a and b" or "a, b and c"
func joinWords(words []string) string {
	if len(words) <= 1 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

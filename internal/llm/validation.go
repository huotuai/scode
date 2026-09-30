package llm

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Argument validation against a tool's declared JSON Schema — the
// behavioral port of pi's validateToolArguments (utils/validation.ts):
// normalizeOptionalNulls, then schema-driven coercion (Value.Convert's
// role), then a full check. The agent loop validates BEFORE execution
// and turns a failure into an error tool result the model can
// self-correct from; on success the COERCED arguments are what the
// tool executes with (pi returns the converted value too).
//
// Schema surface (pi's JsonSchemaObject plus the common constraint
// keywords): type (string or list), properties, required,
// additionalProperties (bool or schema), items (schema or tuple),
// allOf/anyOf/oneOf, enum, const, pattern, minLength/maxLength,
// minimum/maximum/exclusiveMinimum/exclusiveMaximum, multipleOf,
// minItems/maxItems.

// ValidateArguments checks raw against the JSON Schema document params
// and returns the (possibly coerced) arguments to execute with. A nil
// schema accepts any JSON object.
func ValidateArguments(params json.RawMessage, args json.RawMessage) (json.RawMessage, error) {
	var v any
	if len(args) == 0 {
		v = map[string]any{} // tools may take no arguments
	} else if err := json.Unmarshal(args, &v); err != nil {
		return nil, fmt.Errorf("arguments must be a JSON object: %w", err)
	}
	if len(params) == 0 {
		if _, isObj := v.(map[string]any); !isObj {
			return nil, fmt.Errorf("arguments must be a JSON object")
		}
		return args, nil
	}
	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		return args, nil // unparseable schema: let execution judge
	}

	normalizeOptionalNulls(v, schema)
	v = coerceWithSchema(v, schema)
	if err := validateValue(v, schema, "arguments"); err != nil {
		return nil, err
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- validation ---

func validateValue(v any, schema map[string]any, path string) error {
	for _, sub := range schemasOf(schema["allOf"]) {
		if err := validateValue(v, sub, path); err != nil {
			return err
		}
	}
	if subs := schemasOf(schema["anyOf"]); len(subs) > 0 {
		if !matchesAny(v, subs, path) {
			return fmt.Errorf("%s: does not match any anyOf schema", path)
		}
	}
	if subs := schemasOf(schema["oneOf"]); len(subs) > 0 {
		matches := 0
		for _, sub := range subs {
			if validateValue(v, sub, path) == nil {
				matches++
			}
		}
		if matches != 1 {
			return fmt.Errorf("%s: matches %d oneOf schemas, want exactly 1", path, matches)
		}
	}
	if c, ok := schema["const"]; ok && !jsonEqual(v, c) {
		return fmt.Errorf("%s: not equal to const", path)
	}
	if enum, ok := schema["enum"].([]any); ok {
		matched := false
		for _, e := range enum {
			if jsonEqual(v, e) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s: value not in enum", path)
		}
	}

	types := schemaTypes(schema)
	if len(types) > 0 && !matchesAnyType(v, types) {
		return fmt.Errorf("%s: expected %s", path, strings.Join(types, " or "))
	}

	switch v := v.(type) {
	case map[string]any:
		for _, req := range requiredOf(schema) {
			if _, present := v[req]; !present {
				return fmt.Errorf("%s: missing required property %q", path, req)
			}
		}
		props, _ := schema["properties"].(map[string]any)
		for name, rawSub := range props {
			sub, isMap := rawSub.(map[string]any)
			if !isMap {
				continue
			}
			if val, present := v[name]; present {
				if err := validateValue(val, sub, path+"."+name); err != nil {
					return err
				}
			}
		}
		switch ap := schema["additionalProperties"].(type) {
		case bool:
			if !ap {
				for name := range v {
					if _, declared := props[name]; !declared {
						return fmt.Errorf("%s: additional property %q not allowed", path, name)
					}
				}
			}
		case map[string]any:
			for name, val := range v {
				if _, declared := props[name]; declared {
					continue
				}
				if err := validateValue(val, ap, path+"."+name); err != nil {
					return err
				}
			}
		}
	case []any:
		if n, ok := numberOf(schema["minItems"]); ok && len(v) < int(n) {
			return fmt.Errorf("%s: has %d items, minimum is %d", path, len(v), int(n))
		}
		if n, ok := numberOf(schema["maxItems"]); ok && len(v) > int(n) {
			return fmt.Errorf("%s: has %d items, maximum is %d", path, len(v), int(n))
		}
		if tuple, ok := schema["items"].([]any); ok {
			for i, el := range v {
				if i >= len(tuple) {
					break
				}
				if sub, isMap := tuple[i].(map[string]any); isMap {
					if err := validateValue(el, sub, fmt.Sprintf("%s[%d]", path, i)); err != nil {
						return err
					}
				}
			}
		} else if items, ok := schema["items"].(map[string]any); ok {
			for i, el := range v {
				if err := validateValue(el, items, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	case string:
		if n, ok := numberOf(schema["minLength"]); ok && len(v) < int(n) {
			return fmt.Errorf("%s: shorter than minLength %d", path, int(n))
		}
		if n, ok := numberOf(schema["maxLength"]); ok && len(v) > int(n) {
			return fmt.Errorf("%s: longer than maxLength %d", path, int(n))
		}
		if p, ok := schema["pattern"].(string); ok {
			if re, err := regexp.Compile(p); err == nil && !re.MatchString(v) {
				return fmt.Errorf("%s: does not match pattern %q", path, p)
			}
		}
	case float64:
		if err := checkNumberConstraints(v, schema, path); err != nil {
			return err
		}
	}
	return nil
}

func matchesAny(v any, subs []map[string]any, path string) bool {
	for _, sub := range subs {
		if validateValue(v, sub, path) == nil {
			return true
		}
	}
	return false
}

func checkNumberConstraints(v float64, schema map[string]any, path string) error {
	if n, ok := numberOf(schema["minimum"]); ok && v < n {
		return fmt.Errorf("%s: below minimum %v", path, n)
	}
	if n, ok := numberOf(schema["maximum"]); ok && v > n {
		return fmt.Errorf("%s: above maximum %v", path, n)
	}
	if n, ok := numberOf(schema["exclusiveMinimum"]); ok && v <= n {
		return fmt.Errorf("%s: not above exclusiveMinimum %v", path, n)
	}
	if n, ok := numberOf(schema["exclusiveMaximum"]); ok && v >= n {
		return fmt.Errorf("%s: not below exclusiveMaximum %v", path, n)
	}
	if n, ok := numberOf(schema["multipleOf"]); ok && n != 0 {
		q := v / n
		if q != float64(int64(q)) {
			return fmt.Errorf("%s: not a multiple of %v", path, n)
		}
	}
	return nil
}

// --- coercion (pi's Value.Convert role) ---

func normalizeOptionalNulls(v any, schema map[string]any) {
	obj, isObj := v.(map[string]any)
	if !isObj {
		if arr, isArr := v.([]any); isArr {
			if items, ok := schema["items"].(map[string]any); ok {
				for _, el := range arr {
					normalizeOptionalNulls(el, items)
				}
			}
		}
		return
	}
	required := map[string]bool{}
	for _, r := range requiredOf(schema) {
		required[r] = true
	}
	props, _ := schema["properties"].(map[string]any)
	for name, rawSub := range props {
		sub, isMap := rawSub.(map[string]any)
		if !isMap {
			continue
		}
		val, present := obj[name]
		if !present {
			continue
		}
		// A null on an optional property whose schema rejects null is
		// deleted, not coerced (pi's normalizeOptionalNulls).
		if val == nil && !required[name] && validateValue(nil, sub, "x") != nil {
			delete(obj, name)
			continue
		}
		normalizeOptionalNulls(val, sub)
	}
}

func coerceWithSchema(v any, schema map[string]any) any {
	next := v
	for _, sub := range schemasOf(schema["allOf"]) {
		next = coerceWithSchema(next, sub)
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		if subs := schemasOf(schema[key]); len(subs) > 0 {
			next = coerceWithUnion(next, subs)
		}
	}
	types := schemaTypes(schema)
	if len(types) > 0 && !matchesAnyType(next, types) {
		for _, typ := range types {
			if coerced := coercePrimitive(next, typ); coerced != next {
				next = coerced
				break
			}
		}
	}
	if obj, isObj := next.(map[string]any); isObj && containsType(types, "object") {
		props, _ := schema["properties"].(map[string]any)
		for name, rawSub := range props {
			sub, isMap := rawSub.(map[string]any)
			if !isMap {
				continue
			}
			if val, present := obj[name]; present {
				obj[name] = coerceWithSchema(val, sub)
			}
		}
		if ap, ok := schema["additionalProperties"].(map[string]any); ok {
			for name, val := range obj {
				if _, declared := props[name]; declared {
					continue
				}
				obj[name] = coerceWithSchema(val, ap)
			}
		}
	}
	if arr, isArr := next.([]any); isArr && containsType(types, "array") {
		if tuple, ok := schema["items"].([]any); ok {
			for i := range arr {
				if i >= len(tuple) {
					break
				}
				if sub, isMap := tuple[i].(map[string]any); isMap {
					arr[i] = coerceWithSchema(arr[i], sub)
				}
			}
		} else if items, ok := schema["items"].(map[string]any); ok {
			for i := range arr {
				arr[i] = coerceWithSchema(arr[i], items)
			}
		}
	}
	return next
}

func coerceWithUnion(v any, subs []map[string]any) any {
	for _, sub := range subs {
		if validateValue(v, sub, "x") == nil {
			return v
		}
	}
	for _, sub := range subs {
		candidate := coerceWithSchema(v, sub)
		if validateValue(candidate, sub, "x") == nil {
			return candidate
		}
	}
	return v
}

// coercePrimitive is pi's coercePrimitiveByType.
func coercePrimitive(v any, typ string) any {
	switch typ {
	case "number":
		switch x := v.(type) {
		case nil:
			return float64(0)
		case string:
			if s := strings.TrimSpace(x); s != "" {
				var f float64
				if _, err := fmt.Sscanf(s, "%g", &f); err == nil {
					return f
				}
			}
		case bool:
			if x {
				return float64(1)
			}
			return float64(0)
		}
	case "integer":
		switch x := v.(type) {
		case nil:
			return float64(0)
		case string:
			if s := strings.TrimSpace(x); s != "" {
				var f float64
				if _, err := fmt.Sscanf(s, "%g", &f); err == nil && f == float64(int64(f)) {
					return f
				}
			}
		case bool:
			if x {
				return float64(1)
			}
			return float64(0)
		}
	case "boolean":
		switch x := v.(type) {
		case nil:
			return false
		case string:
			if x == "true" {
				return true
			}
			if x == "false" {
				return false
			}
		case float64:
			if x == 1 {
				return true
			}
			if x == 0 {
				return false
			}
		}
	case "string":
		switch x := v.(type) {
		case nil:
			return ""
		case float64:
			return fmt.Sprintf("%v", x)
		case bool:
			return fmt.Sprintf("%v", x)
		default:
			_ = x
		}
	case "null":
		switch v {
		case "", float64(0), false:
			return nil
		}
	}
	return v
}

// --- schema helpers ---

func schemaTypes(schema map[string]any) []string {
	switch t := schema["type"].(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func schemasOf(v any) []map[string]any {
	xs, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []map[string]any
	for _, x := range xs {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func requiredOf(schema map[string]any) []string {
	xs, ok := schema["required"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func matchesAnyType(v any, types []string) bool {
	for _, typ := range types {
		if matchesType(v, typ) {
			return true
		}
	}
	return false
}

func containsType(types []string, want string) bool {
	for _, t := range types {
		if t == want {
			return true
		}
	}
	return false
}

func matchesType(v any, typ string) bool {
	switch typ {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "integer":
		f, isNum := v.(float64)
		return isNum && f == float64(int64(f))
	case "number":
		_, ok := v.(float64)
		return ok
	case "null":
		return v == nil
	}
	return false
}

func numberOf(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

func jsonEqual(a, b any) bool {
	ba, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ba) == string(bb)
}

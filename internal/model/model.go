package model

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type Object = map[string]any

func String(v any) string {
	switch value := v.(type) {
	case string:
		return value
	case json.Number:
		return value.String()
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	case nil:
		return ""
	default:
		return fmt.Sprint(value)
	}
}

func Int(v any) int {
	switch value := v.(type) {
	case int:
		return value
	case float64:
		return int(value)
	case json.Number:
		n, _ := value.Int64()
		return int(n)
	case string:
		n, _ := strconv.Atoi(value)
		return n
	default:
		return 0
	}
}

func Bool(v any) bool {
	value, _ := v.(bool)
	return value
}

func Obj(v any) Object {
	value, _ := v.(map[string]any)
	return value
}

func Slice(v any) []any {
	value, _ := v.([]any)
	return value
}

func Objects(v any) []Object {
	raw := Slice(v)
	result := make([]Object, 0, len(raw))
	for _, item := range raw {
		if object := Obj(item); object != nil {
			result = append(result, object)
		}
	}
	return result
}

func DisplayName(message Object) string {
	if nick := String(Obj(message["member"])["nick"]); nick != "" {
		return nick
	}
	author := Obj(message["author"])
	if name := String(author["global_name"]); name != "" {
		return name
	}
	if name := String(author["username"]); name != "" {
		return name
	}
	return "Unknown User"
}

func CloneObjects(objects []Object) ([]Object, error) {
	raw, err := json.Marshal(objects)
	if err != nil {
		return nil, err
	}
	var cloned []Object
	err = json.Unmarshal(raw, &cloned)
	return cloned, err
}

// NormalizeMessageMembers applies the newest guild-specific member snapshot to
// older and referenced messages, matching Discord's newest-first REST history.
func NormalizeMessageMembers(messages []Object) []Object {
	cloned, err := CloneObjects(messages)
	if err != nil {
		return messages
	}
	directory := map[string]Object{}
	remember := func(message Object) {
		id := String(Obj(message["author"])["id"])
		member := Obj(message["member"])
		if id == "" || member == nil {
			return
		}
		known := directory[id]
		merged := Object{}
		for key, value := range member {
			merged[key] = value
		}
		for key, value := range known {
			merged[key] = value
		}
		if known == nil || known["nick"] == nil {
			merged["nick"] = member["nick"]
		}
		if known == nil || known["roles"] == nil {
			merged["roles"] = member["roles"]
		}
		directory[id] = merged
	}
	for _, message := range cloned {
		remember(message)
		remember(Obj(message["referenced_message"]))
	}
	apply := func(message Object) {
		id := String(Obj(message["author"])["id"])
		known := directory[id]
		if message == nil || known == nil {
			return
		}
		merged := Object{}
		for key, value := range Obj(message["member"]) {
			merged[key] = value
		}
		for key, value := range known {
			merged[key] = value
		}
		message["member"] = merged
	}
	for _, message := range cloned {
		apply(message)
		apply(Obj(message["referenced_message"]))
	}
	return cloned
}

func SafeBaseName(value string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		valid := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-'
		if valid {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
		if b.Len() >= 80 {
			break
		}
	}
	result := strings.Trim(b.String(), "-")
	if result == "" {
		return "transcript"
	}
	return result
}

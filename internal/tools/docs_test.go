package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestClassDocs(t *testing.T) {
	call, _, cs := newSession(t, "Docs")
	docs := func(args map[string]any) (string, map[string]any) {
		t.Helper()
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "godot_class_docs", Arguments: args})
		must(t, err)
		if res.IsError {
			t.Fatalf("class_docs %v: %s", args, mustJSON(res.Content))
		}
		text := res.Content[0].(*mcp.TextContent).Text
		var out map[string]any
		raw, _ := json.Marshal(res.StructuredContent)
		must(t, json.Unmarshal(raw, &out))
		return text, out
	}

	text, out := docs(map[string]any{"name": "KinematicBody2D", "member": "move_and_slide"})
	if !strings.Contains(text, "in Godot 4 it is CharacterBody2D") || !strings.Contains(text, "move_and_slide() -> bool") || out["found"] != true {
		t.Errorf("rename lookup: %v\n%s", out, text)
	}
	if !strings.HasPrefix(out["version"].(string), "4.") {
		t.Errorf("version = %v", out["version"])
	}
	text, _ = docs(map[string]any{"search": "tween_prop"})
	if !strings.Contains(text, "Tween.tween_property(") {
		t.Errorf("search: %s", text)
	}

	// class_name проекта попадает в справку после импорта.
	if o, ok := call("godot_write_file", map[string]any{"path": "res://item.gd", "content": "class_name ItemData\nextends Resource\n"}); !ok {
		t.Fatal(o)
	}
	if o, ok := call("godot_import", nil); !ok {
		t.Fatal(o)
	}
	text, out = docs(map[string]any{"name": "ItemData"})
	if !strings.Contains(text, "class of this project (class_name in res://item.gd), extending Resource") || out["found"] != false {
		t.Errorf("project class: %v\n%s", out, text)
	}

	if o, ok := call("godot_class_docs", map[string]any{}); ok || !strings.Contains(mustJSON(o), "pass name") {
		t.Errorf("empty request must fail: %v", o)
	}
}

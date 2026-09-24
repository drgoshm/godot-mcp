package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"godot-mcp/internal/godot"
	"godot-mcp/internal/project"
)

// Сквозной тест через настоящий MCP-протокол (in-memory транспорт).
// Движковые шаги выполняются только с GODOT_BIN.
func TestEndToEnd(t *testing.T) {
	bin := os.Getenv("GODOT_BIN")
	if bin == "" {
		t.Skip("GODOT_BIN not set")
	}
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "project.godot"),
		[]byte("config_version=5\n\n[application]\n\nconfig/name=\"E2E\"\nrun/main_scene=\"res://main.tscn\"\n"), 0o644))

	sb, err := project.NewSandbox(dir)
	must(t, err)
	g := &godot.Godot{Bin: bin, ProjectDir: sb.Root()}
	runner := godot.NewRunner(g)
	defer runner.StopAll(context.Background())

	server := mcp.NewServer(&mcp.Implementation{Name: "godot-mcp-test", Version: "test"}, nil)
	Register(server, &Deps{Sandbox: sb, Godot: g, Runner: runner, Version: "test"})

	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	must(t, err)
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "test"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	must(t, err)
	defer cs.Close()

	call := func(name string, args map[string]any) (map[string]any, bool) {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		must(t, err)
		if res.IsError {
			var msg string
			for _, c := range res.Content {
				if tc, ok := c.(*mcp.TextContent); ok {
					msg += tc.Text
				}
			}
			return map[string]any{"error": msg}, false
		}
		var out map[string]any
		raw, _ := json.Marshal(res.StructuredContent)
		must(t, json.Unmarshal(raw, &out))
		return out, true
	}

	// Скрипт с ошибкой -> check_script находит строку -> правим -> проходит.
	if _, ok := call("godot_write_file", map[string]any{
		"path": "res://scripts/player.gd", "content": "extends Node2D\n\nfunc _ready() -> void:\n\tprint(\"player ready at \", position)\n\tundefined_call()\n",
	}); !ok {
		t.Fatal("write_file failed")
	}
	out, _ := call("godot_check_script", map[string]any{"paths": []string{"res://scripts/player.gd"}})
	if out["all_ok"] != false || !strings.Contains(mustJSON(out), `"line":5`) {
		t.Fatalf("check_script should report line 5: %s", mustJSON(out))
	}
	if _, ok := call("godot_edit_file", map[string]any{
		"path": "res://scripts/player.gd", "old_str": "\tundefined_call()\n", "new_str": "",
	}); !ok {
		t.Fatal("edit_file failed")
	}
	out, _ = call("godot_check_script", map[string]any{"paths": []string{"res://scripts/player.gd"}})
	if out["all_ok"] != true {
		t.Fatalf("check_script after fix: %s", mustJSON(out))
	}

	// Песочница.
	if out, ok := call("godot_read_file", map[string]any{"path": "../../etc/passwd"}); ok {
		t.Fatalf("sandbox escape succeeded: %v", out)
	}

	// Сцена через движок.
	out, ok := call("godot_create_scene", map[string]any{
		"path": "res://main.tscn",
		"root": map[string]any{
			"type": "Node2D", "name": "Player", "script": "res://scripts/player.gd",
			"properties": map[string]any{"position": "Vector2(10, 20)"},
			"children":   []any{map[string]any{"type": "Sprite2D", "name": "Sprite"}},
		},
	})
	if !ok || !strings.HasPrefix(out["uid"].(string), "uid://") || out["nodes"].(float64) != 2 {
		t.Fatalf("create_scene: %v", out)
	}
	if out, ok := call("godot_create_scene", map[string]any{
		"path": "res://bad.tscn", "root": map[string]any{"type": "NoSuchNode"},
	}); ok || !strings.Contains(out["error"].(string), "NoSuchNode") {
		t.Fatalf("create_scene should reject unknown types: %v", out)
	}

	// Запуск главной сцены.
	out, ok = call("godot_run_project", map[string]any{"headless": true, "quit_after": 10, "wait_seconds": 20})
	if !ok || !strings.Contains(mustJSON(out), "player ready at (10.0, 20.0)") {
		t.Fatalf("run_project: %s", mustJSON(out))
	}

	info, _ := call("godot_project_info", nil)
	if info["project"].(map[string]any)["name"] != "E2E" {
		t.Errorf("project_info: %v", info)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

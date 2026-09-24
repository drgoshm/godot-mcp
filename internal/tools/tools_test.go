package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/drgoshm/godot-mcp/internal/godot"
	"github.com/drgoshm/godot-mcp/internal/project"
)

// Сквозной тест через настоящий MCP-протокол (in-memory транспорт).
// Движковые шаги выполняются только с GODOT_BIN.
func TestEndToEnd(t *testing.T) {
	call, _ := newSession(t, "E2E")

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

// callFunc вызывает инструмент; при isError возвращает {"error": текст}, false.
type callFunc func(name string, args map[string]any) (map[string]any, bool)

// newSession поднимает сервер на свежем проекте и подключает к нему клиента
// через in-memory транспорт. Без GODOT_BIN тест пропускается.
func newSession(t *testing.T, name string) (callFunc, string) {
	t.Helper()
	bin := os.Getenv("GODOT_BIN")
	if bin == "" {
		t.Skip("GODOT_BIN not set")
	}
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "project.godot"),
		[]byte("config_version=5\n\n[application]\n\nconfig/name=\""+name+"\"\nrun/main_scene=\"res://main.tscn\"\n"), 0o644))

	sb, err := project.NewSandbox(dir)
	must(t, err)
	g := &godot.Godot{Bin: bin, ProjectDir: sb.Root()}
	runner := godot.NewRunner(g)
	t.Cleanup(func() { runner.StopAll(context.Background()) })

	server := mcp.NewServer(&mcp.Implementation{Name: "godot-mcp-test", Version: "test"}, nil)
	Register(server, &Deps{Sandbox: sb, Godot: g, Runner: runner, Version: "test"})

	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	must(t, err)
	t.Cleanup(func() { ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "test"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	must(t, err)
	t.Cleanup(func() { cs.Close() })

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
	return call, dir
}

// Встроенные подресурсы: {"_type": ...} в свойствах узла сохраняются
// в .tscn как sub_resource, в том числе вложенные и пользовательские.
func TestCreateSceneSubResources(t *testing.T) {
	call, dir := newSession(t, "SubRes")

	write := func(path, content string) {
		t.Helper()
		if out, ok := call("godot_write_file", map[string]any{"path": path, "content": content}); !ok {
			t.Fatalf("write %s: %v", path, out)
		}
	}
	write("res://item.gd", "class_name ItemData\nextends Resource\n\n@export var title: String\n@export var power: int\n")
	write("res://holder.gd", "extends Node2D\n\n@export var main_item: ItemData\n@export var items: Array[ItemData]\n")
	if out, ok := call("godot_import", nil); !ok {
		t.Fatalf("import: %v", out)
	}

	out, ok := call("godot_create_scene", map[string]any{
		"path": "res://player.tscn",
		"root": map[string]any{
			"type": "CharacterBody2D", "name": "Player",
			"children": []any{
				map[string]any{"type": "CollisionShape2D", "name": "Shape", "properties": map[string]any{
					"shape": map[string]any{"_type": "RectangleShape2D", "size": "Vector2(32, 48)"},
				}},
				map[string]any{"type": "Sprite2D", "name": "Sprite", "properties": map[string]any{
					"texture": map[string]any{"_type": "GradientTexture2D", "width": 16, "height": 16,
						"gradient": map[string]any{"_type": "Gradient", "colors": "PackedColorArray(1, 0, 0, 1, 0, 0, 1, 1)"}},
				}},
				map[string]any{"type": "Node2D", "name": "Holder", "script": "res://holder.gd", "properties": map[string]any{
					"main_item": map[string]any{"_type": "ItemData", "title": "Sword", "power": 3.0},
					"items": []any{
						map[string]any{"_script": "res://item.gd", "title": "Shield"},
						map[string]any{"_type": "ItemData", "title": "Bow", "power": 2},
					},
				}},
			},
		},
	})
	if !ok {
		t.Fatalf("create_scene: %v", out)
	}

	tscn, err := os.ReadFile(filepath.Join(dir, "player.tscn"))
	must(t, err)
	for _, want := range []string{`[sub_resource type="RectangleShape2D"`, `[sub_resource type="Gradient"`, `[sub_resource type="GradientTexture2D"`} {
		if !strings.Contains(string(tscn), want) {
			t.Errorf("player.tscn has no %s:\n%s", want, tscn)
		}
	}

	// Сцена загружается движком, значения на месте.
	out, ok = call("godot_run_script", map[string]any{"code": `extends SceneTree
func _init() -> void:
	var p: Node = (load("res://player.tscn") as PackedScene).instantiate()
	var shape: RectangleShape2D = p.get_node("Shape").shape
	var tex: GradientTexture2D = p.get_node("Sprite").texture
	var h: Node = p.get_node("Holder")
	print("SIZE=", shape.size, " TEX=", tex.width, " COLOR=", tex.gradient.colors[0])
	print("MAIN=", h.main_item.title, ":", h.main_item.power, " ITEMS=", h.items.size(), ":", h.items[0].title, ",", h.items[1].title, ":", h.items[1].power)
	p.free()
	quit()
`})
	text := mustJSON(out)
	for _, want := range []string{"SIZE=(32.0, 48.0)", "TEX=16", "COLOR=(1.0, 0.0, 0.0, 1.0)", "MAIN=Sword:3", "ITEMS=2:Shield,Bow:2"} {
		if !ok || !strings.Contains(text, want) {
			t.Errorf("run_script output has no %q: %s", want, text)
		}
	}

	// Ошибки должны объяснять, что не так.
	bad := []struct {
		name  string
		prop  string
		value any
		want  string
	}{
		{"wrong class", "shape", map[string]any{"_type": "Gradient"}, "expects Shape2D"},
		{"abstract", "shape", map[string]any{"_type": "Shape2D"}, "abstract"},
		{"unknown type", "shape", map[string]any{"_type": "NoSuchShape"}, "unknown resource type 'NoSuchShape'"},
		{"not a resource", "shape", map[string]any{"_type": "Node2D"}, "not a Resource type"},
		{"bad sub-property", "shape", map[string]any{"_type": "RectangleShape2D", "radius": 3}, "has no property 'radius'"},
		{"resource for a Vector2", "position", map[string]any{"_type": "RectangleShape2D"}, "the property is Vector2"},
	}
	for _, tc := range bad {
		out, ok := call("godot_create_scene", map[string]any{
			"path": "res://bad.tscn", "overwrite": true,
			"root": map[string]any{"type": "CollisionShape2D", "properties": map[string]any{tc.prop: tc.value}},
		})
		if ok || !strings.Contains(fmt.Sprint(out["error"]), tc.want) {
			t.Errorf("%s: want error containing %q, got %v", tc.name, tc.want, out)
		}
	}
	out, ok = call("godot_create_scene", map[string]any{
		"path": "res://bad.tscn", "overwrite": true,
		"root": map[string]any{"type": "Node2D", "script": "res://holder.gd", "properties": map[string]any{
			"items": []any{map[string]any{"_type": "Gradient"}},
		}},
	})
	if ok || !strings.Contains(fmt.Sprint(out["error"]), "items[0]: Gradient does not fit the array element type ItemData") {
		t.Errorf("typed array: want error about items[0], got %v", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "bad.tscn")); err == nil {
		t.Error("bad.tscn must not be written when the spec has errors")
	}
}

// scene_builder.gd выполняется с настройками проекта. Если там все
// предупреждения GDScript превращены в ошибки, сборка сцены не должна ломаться.
func TestCreateSceneStrictWarnings(t *testing.T) {
	call, dir := newSession(t, "Strict")

	// Список предупреждений берём у движка, чтобы тест следил за новыми версиями.
	out, ok := call("godot_run_script", map[string]any{"code": `extends SceneTree
func _init() -> void:
	for p: Dictionary in ProjectSettings.get_property_list():
		var n: String = p["name"]
		if n.begins_with("debug/gdscript/warnings/") and p["type"] == TYPE_INT and p["hint"] == PROPERTY_HINT_ENUM:
			print("WARN=", n.trim_prefix("debug/"))
	quit()
`})
	if !ok {
		t.Fatalf("list warnings: %v", out)
	}
	var cfg strings.Builder
	cfg.WriteString("config_version=5\n\n[application]\n\nconfig/name=\"Strict\"\n\n[debug]\n\n")
	n := 0
	for _, line := range strings.Split(fmt.Sprint(out["output"]), "\n") {
		if name, found := strings.CutPrefix(strings.TrimSpace(line), "WARN="); found {
			fmt.Fprintf(&cfg, "%s=2\n", name)
			n++
		}
	}
	if n < 20 {
		t.Fatalf("expected dozens of warning settings, got %d: %v", n, out)
	}
	must(t, os.WriteFile(filepath.Join(dir, "project.godot"), []byte(cfg.String()), 0o644))

	out, ok = call("godot_create_scene", map[string]any{
		"path": "res://strict.tscn",
		"root": map[string]any{"type": "StaticBody2D", "name": "Wall", "groups": []any{"walls"}, "children": []any{
			map[string]any{"type": "CollisionShape2D", "name": "Shape", "properties": map[string]any{
				"shape":    map[string]any{"_type": "CircleShape2D", "radius": 8},
				"position": "Vector2(1, 2)",
			}},
		}},
	})
	if !ok || out["nodes"] != float64(2) {
		t.Fatalf("create_scene with %d warnings as errors: %v", n, out)
	}
}

// Соединения сигналов сохраняются в .tscn и срабатывают после загрузки;
// ошибки в них ловятся при сборке, а не при срабатывании сигнала.
func TestCreateSceneConnections(t *testing.T) {
	call, dir := newSession(t, "Signals")
	if out, ok := call("godot_write_file", map[string]any{"path": "res://menu.gd", "content": `extends Control

func _on_start_pressed() -> void:
	print("START")

func _on_sound_toggled(on: bool) -> void:
	print("SOUND=", on)

func _on_timeout(tag: String, times: int = 1) -> void:
	print("TIMEOUT=", tag, ":", times)
`}); !ok {
		t.Fatalf("write menu.gd: %v", out)
	}

	root := map[string]any{"type": "Control", "name": "Menu", "script": "res://menu.gd", "children": []any{
		map[string]any{"type": "VBoxContainer", "name": "UI", "children": []any{
			map[string]any{"type": "Button", "name": "Start"},
			map[string]any{"type": "CheckButton", "name": "Sound"},
		}},
		map[string]any{"type": "Timer", "name": "Timer"},
	}}
	out, ok := call("godot_create_scene", map[string]any{
		"path": "res://menu.tscn", "root": root,
		"connections": []any{
			map[string]any{"from": "UI/Start", "signal": "pressed", "to": ".", "method": "_on_start_pressed"},
			map[string]any{"from": "UI/Sound", "signal": "toggled", "to": ".", "method": "_on_sound_toggled"},
			map[string]any{"from": "Timer", "signal": "timeout", "to": ".", "method": "_on_timeout", "binds": []any{"tick"}, "flags": 1},
		},
	})
	if !ok || out["connections"] != float64(3) {
		t.Fatalf("create_scene: %v", out)
	}

	tscn, err := os.ReadFile(filepath.Join(dir, "menu.tscn"))
	must(t, err)
	for _, want := range []string{
		`[connection signal="pressed" from="UI/Start" to="." method="_on_start_pressed"]`,
		`[connection signal="toggled" from="UI/Sound" to="." method="_on_sound_toggled"]`,
		`[connection signal="timeout" from="Timer" to="." method="_on_timeout" flags=`,
		`binds= ["tick"]`,
	} {
		if !strings.Contains(string(tscn), want) {
			t.Errorf("menu.tscn has no %s:\n%s", want, tscn)
		}
	}

	// После загрузки сигналы вызывают обработчики. Timer соединён отложенно
	// (flags=1), поэтому его обработчик срабатывает на следующем кадре.
	out, ok = call("godot_run_script", map[string]any{"code": `extends SceneTree
func _init() -> void:
	var menu: Node = (load("res://menu.tscn") as PackedScene).instantiate()
	root.add_child(menu)
	(menu.get_node("UI/Start") as Button).pressed.emit()
	(menu.get_node("UI/Sound") as CheckButton).toggled.emit(true)
	(menu.get_node("Timer") as Timer).timeout.emit()
	print("EMITTED")

func _process(_delta: float) -> bool:
	return true
`})
	text := mustJSON(out)
	for _, want := range []string{"START", "SOUND=true", "EMITTED", "TIMEOUT=tick:1"} {
		if !ok || !strings.Contains(text, want) {
			t.Errorf("run_script output has no %q: %s", want, text)
		}
	}
	if i, j := strings.Index(text, "EMITTED"), strings.Index(text, "TIMEOUT="); i < 0 || j < i {
		t.Errorf("deferred timeout handler should run after EMITTED: %s", text)
	}

	bad := []struct {
		name string
		conn map[string]any
		want string
	}{
		{"no node", map[string]any{"from": "UI/Quit", "signal": "pressed", "to": ".", "method": "_on_start_pressed"}, "no node at 'UI/Quit'"},
		{"no signal", map[string]any{"from": "UI/Start", "signal": "clicked", "to": ".", "method": "_on_start_pressed"}, "Button has no signal 'clicked'"},
		{"no method", map[string]any{"from": "UI/Start", "signal": "pressed", "to": ".", "method": "_on_quit"}, "no method '_on_quit'"},
		{"too many args", map[string]any{"from": "UI/Sound", "signal": "toggled", "to": ".", "method": "_on_start_pressed"}, "takes 0 arguments, but the signal passes 1 (+0 binds)"},
		{"too few args", map[string]any{"from": "UI/Start", "signal": "pressed", "to": ".", "method": "_on_timeout"}, "takes 1..2 arguments, but the signal passes 0 (+0 binds)"},
		{"missing method name", map[string]any{"from": "UI/Start", "signal": "pressed", "to": "."}, "method"},
	}
	for _, tc := range bad {
		out, ok := call("godot_create_scene", map[string]any{
			"path": "res://bad.tscn", "root": root, "connections": []any{tc.conn},
		})
		if ok || !strings.Contains(fmt.Sprint(out["error"]), tc.want) {
			t.Errorf("%s: want error containing %q, got %v", tc.name, tc.want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "bad.tscn")); err == nil {
		t.Error("bad.tscn must not be written when a connection is invalid")
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

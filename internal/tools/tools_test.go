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

	"github.com/drgoshm/godot-mcp/internal/docs"
	"github.com/drgoshm/godot-mcp/internal/godot"
	"github.com/drgoshm/godot-mcp/internal/project"
)

// Сквозной тест через настоящий MCP-протокол (in-memory транспорт).
// Движковые шаги выполняются только с GODOT_BIN.
func TestEndToEnd(t *testing.T) {
	call, _, _ := newSession(t, "E2E")

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
func newSession(t *testing.T, name string) (callFunc, string, *mcp.ClientSession) {
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
	// Кеш справки — во временном каталоге теста, а не в каталоге пользователя.
	Register(server, &Deps{Sandbox: sb, Godot: g, Runner: runner, Version: "test",
		Docs: &docs.Loader{Bin: bin, Version: "test", CacheDir: t.TempDir()}})

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
	return call, dir, cs
}

// Встроенные подресурсы: {"_type": ...} в свойствах узла сохраняются
// в .tscn как sub_resource, в том числе вложенные и пользовательские.
func TestCreateSceneSubResources(t *testing.T) {
	call, dir, _ := newSession(t, "SubRes")

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
	call, dir, _ := newSession(t, "Strict")

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
	call, dir, _ := newSession(t, "Signals")
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

// godot_scene_tree возвращает сцену в формате godot_create_scene (с круговой
// проверкой), а godot_edit_scene правит её атомарно, сохраняя UID.
func TestSceneTreeAndEdit(t *testing.T) {
	call, dir, _ := newSession(t, "Edit")
	mustCall := func(name string, args map[string]any) map[string]any {
		t.Helper()
		out, ok := call(name, args)
		if !ok {
			t.Fatalf("%s: %v", name, out)
		}
		return out
	}
	mustCall("godot_write_file", map[string]any{"path": "res://player.gd", "content": "extends CharacterBody2D\n\nsignal hit\n\n@export var speed: float = 100.0\n\nfunc _on_hit() -> void:\n\tpass\n"})
	mustCall("godot_create_scene", map[string]any{"path": "res://enemy.tscn", "root": map[string]any{
		"type": "Area2D", "name": "Enemy", "children": []any{map[string]any{"type": "CollisionShape2D", "name": "Shape",
			"properties": map[string]any{"shape": map[string]any{"_type": "CircleShape2D", "radius": 8}}}},
	}})
	points := make([]string, 60)
	for i := range points {
		points[i] = fmt.Sprintf("%d, %d", i*10, i*i)
	}
	created := mustCall("godot_create_scene", map[string]any{
		"path": "res://level.tscn",
		"root": map[string]any{"type": "Node2D", "name": "Level", "children": []any{
			map[string]any{"type": "CharacterBody2D", "name": "Player", "script": "res://player.gd",
				"properties": map[string]any{"speed": 250, "position": "Vector2(10, 20)"},
				"children": []any{map[string]any{"type": "CollisionShape2D", "name": "Shape",
					"properties": map[string]any{"shape": map[string]any{"_type": "RectangleShape2D", "size": "Vector2(32, 48)"}}}}},
			map[string]any{"type": "CanvasLayer", "name": "UI", "children": []any{
				map[string]any{"type": "Label", "name": "Score", "properties": map[string]any{"text": "0"}}}},
			map[string]any{"scene": "res://enemy.tscn", "name": "Enemy1", "properties": map[string]any{"position": "Vector2(100, 0)"}},
			map[string]any{"type": "Line2D", "name": "Path", "properties": map[string]any{"points": "PackedVector2Array(" + strings.Join(points, ", ") + ")"}},
		}},
		"connections": []any{map[string]any{"from": "Player", "signal": "hit", "to": "Player", "method": "_on_hit"}},
	})
	uid := created["uid"].(string)

	// ---- чтение ----
	tree := mustCall("godot_scene_tree", map[string]any{"path": uid, "max_value_chars": 100})
	j := mustJSON(tree)
	for _, want := range []string{
		`"type":"Node2D"`, `"script":"res://player.gd"`, `"speed":250`, `"position":"Vector2(10, 20)"`,
		`"shape":{"_type":"RectangleShape2D","size":"Vector2(32, 48)"}`,
		`"scene":"res://enemy.tscn"`, `"path":"UI/Score"`, `"text":"0"`,
		`"points":{"_omitted":"PackedVector2Array`,
		`{"from":"Player","method":"_on_hit","signal":"hit","to":"Player"}`,
	} {
		if !strings.Contains(j, want) {
			t.Errorf("scene_tree has no %s:\n%s", want, j)
		}
	}
	if tree["path"] != "res://level.tscn" || tree["nodes"] != float64(7) {
		t.Errorf("scene_tree header: path=%v nodes=%v", tree["path"], tree["nodes"])
	}
	sub := mustCall("godot_scene_tree", map[string]any{"path": "res://level.tscn", "node": "UI"})
	if sub["root"].(map[string]any)["name"] != "UI" || sub["connections"] != nil {
		t.Errorf("subtree: %s", mustJSON(sub))
	}

	// ---- круговая проверка: дерево -> create_scene -> то же дерево ----
	root := tree["root"].(map[string]any)
	if out, ok := call("godot_create_scene", map[string]any{"path": "res://copy.tscn", "root": root, "connections": tree["connections"]}); ok ||
		!strings.Contains(fmt.Sprint(out["error"]), "omitted by godot_scene_tree") {
		t.Errorf("writing an omitted value back must fail: %v", out)
	}
	var kept []any
	for _, c := range root["children"].([]any) {
		if c.(map[string]any)["name"] != "Path" {
			kept = append(kept, c)
		}
	}
	root["children"] = kept
	mustCall("godot_create_scene", map[string]any{"path": "res://copy.tscn", "root": root, "connections": tree["connections"]})
	orig := mustCall("godot_scene_tree", map[string]any{"path": "res://level.tscn"})
	cp := mustCall("godot_scene_tree", map[string]any{"path": "res://copy.tscn"})
	origRoot := orig["root"].(map[string]any)
	var origKept []any
	for _, c := range origRoot["children"].([]any) {
		if c.(map[string]any)["name"] != "Path" {
			origKept = append(origKept, c)
		}
	}
	origRoot["children"] = origKept
	if a, b := mustJSON(origRoot)+mustJSON(orig["connections"]), mustJSON(cp["root"])+mustJSON(cp["connections"]); a != b {
		t.Errorf("round trip differs:\norig: %s\ncopy: %s", a, b)
	}

	// ---- правка ----
	edited := mustCall("godot_edit_scene", map[string]any{"path": "res://level.tscn", "operations": []any{
		map[string]any{"op": "set_properties", "path": "Player", "properties": map[string]any{"speed": 300}},
		map[string]any{"op": "set_properties", "path": "Player/Shape", "properties": map[string]any{"shape": map[string]any{"_type": "CircleShape2D", "radius": 12}}},
		map[string]any{"op": "add_node", "parent": "UI", "index": 0, "node": map[string]any{"type": "Label", "name": "Lives", "properties": map[string]any{"text": "3"}}},
		map[string]any{"op": "rename", "path": "UI/Score", "name": "Points"},
		map[string]any{"op": "add_node", "node": map[string]any{"type": "Node2D", "name": "Enemies"}},
		map[string]any{"op": "move", "path": "Enemy1", "parent": "Enemies"},
		map[string]any{"op": "groups", "path": "Player", "add": []any{"players"}},
		map[string]any{"op": "disconnect", "from": "Player", "signal": "hit", "to": "Player", "method": "_on_hit"},
		map[string]any{"op": "connect", "from": "Enemies/Enemy1", "signal": "body_entered", "to": ".", "method": "add_child"},
		map[string]any{"op": "remove_node", "path": "Path"},
	}})
	if edited["uid"] != uid || edited["applied"] != float64(10) || edited["connections"] != float64(1) {
		t.Errorf("edit_scene: %v (uid must stay %s)", edited, uid)
	}
	after := mustJSON(mustCall("godot_scene_tree", map[string]any{"path": "res://level.tscn"}))
	for _, want := range []string{
		`"speed":300`, `"shape":{"_type":"CircleShape2D","radius":12}`, `"groups":["players"]`,
		`"path":"UI/Lives"`, `"path":"UI/Points"`, `"path":"Enemies/Enemy1"`,
		`{"from":"Enemies/Enemy1","method":"add_child","signal":"body_entered","to":"."}`,
	} {
		if !strings.Contains(after, want) {
			t.Errorf("after edit, scene_tree has no %s:\n%s", want, after)
		}
	}
	for _, gone := range []string{`"name":"Path"`, `"method":"_on_hit"`, `"name":"Score"`} {
		if strings.Contains(after, gone) {
			t.Errorf("after edit, scene_tree still has %s:\n%s", gone, after)
		}
	}
	if i, j := strings.Index(after, `"name":"Lives"`), strings.Index(after, `"name":"Points"`); i > j {
		t.Errorf("Lives should be inserted before Points: %s", after)
	}
	out := mustCall("godot_run_script", map[string]any{"code": `extends SceneTree
func _init() -> void:
	var level: Node = (load("res://level.tscn") as PackedScene).instantiate()
	print("SPEED=", level.get_node("Player").speed, " ENEMY=", level.get_node("Enemies/Enemy1/Shape").shape.radius)
	level.free()
	quit()
`})
	if j := mustJSON(out); !strings.Contains(j, "SPEED=300") || !strings.Contains(j, "ENEMY=8") {
		t.Errorf("edited scene does not load as expected: %s", j)
	}

	// ---- ошибки: ничего не сохраняется ----
	before, err := os.ReadFile(filepath.Join(dir, "level.tscn"))
	must(t, err)
	bad := []struct {
		name string
		ops  []any
		want string
	}{
		{"atomic", []any{
			map[string]any{"op": "set_properties", "path": "Player", "properties": map[string]any{"speed": 1}},
			map[string]any{"op": "remove_node", "path": "Nope"},
		}, "operations[1] remove_node: no node at 'Nope'"},
		{"inside instance", []any{map[string]any{"op": "set_properties", "path": "Enemies/Enemy1/Shape", "properties": map[string]any{"disabled": true}}},
			"belongs to the instanced scene res://enemy.tscn"},
		{"remove root", []any{map[string]any{"op": "remove_node", "path": "."}}, "cannot remove the scene root"},
		{"rename clash", []any{map[string]any{"op": "rename", "path": "UI/Lives", "name": "Points"}}, "already has a child named 'Points'"},
		{"bad name", []any{map[string]any{"op": "rename", "path": "UI/Lives", "name": "a/b"}}, "not a valid node name"},
		{"into itself", []any{map[string]any{"op": "move", "path": "Player", "parent": "Player/Shape"}}, "into itself or its own child"},
		{"duplicate add", []any{map[string]any{"op": "add_node", "parent": "UI", "node": map[string]any{"type": "Label", "name": "Points"}}}, "operations[0] add_node: cannot name a node 'Points' under 'UI'"},
		{"unknown op", []any{map[string]any{"op": "explode"}}, "unknown op"},
		{"not connected", []any{map[string]any{"op": "disconnect", "from": "Player", "signal": "hit", "to": "Player", "method": "_on_hit"}}, "is not connected"},
	}
	for _, tc := range bad {
		out, ok := call("godot_edit_scene", map[string]any{"path": "res://level.tscn", "operations": tc.ops})
		msg := fmt.Sprint(out["error"])
		if ok || !strings.Contains(msg, tc.want) || !strings.Contains(msg, "no changes were saved") {
			t.Errorf("%s: want error containing %q, got %v", tc.name, tc.want, out)
		}
	}
	afterBad, err := os.ReadFile(filepath.Join(dir, "level.tscn"))
	must(t, err)
	if string(before) != string(afterBad) {
		t.Error("failed edits must not change the file")
	}
	if out, ok := call("godot_scene_tree", map[string]any{"path": "res://nope.tscn"}); ok || !strings.Contains(fmt.Sprint(out["error"]), "does not exist") {
		t.Errorf("missing scene: %v", out)
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

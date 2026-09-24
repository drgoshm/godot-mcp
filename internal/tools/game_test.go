package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const bridgeProject = `config_version=5

[application]

config/name="Bridge"
run/main_scene="res://main.tscn"

[input]

move_right={
"deadzone": 0.2,
"events": []
}
move_left={
"deadzone": 0.2,
"events": []
}
`

const bridgePlayer = `extends CharacterBody2D

@export var speed: float = 200.0
var health: int = 10
var typed: String = ""

func _physics_process(_delta: float) -> void:
	velocity.x = Input.get_axis("move_left", "move_right") * speed
	move_and_slide()

func take_damage(amount: int) -> int:
	health -= amount
	return health

func _unhandled_input(event: InputEvent) -> void:
	if event is InputEventKey and event.pressed and event.unicode > 0:
		typed += char(event.unicode)
`

// Мост к запущенной игре: дерево, выражения, свойства, ввод (headless).
func TestGameBridge(t *testing.T) {
	call, dir, _ := newSession(t, "Bridge")
	writeFiles(t, dir, map[string]string{"project.godot": bridgeProject, "player.gd": bridgePlayer})
	userCfg := "[display]\n\nwindow/size/viewport_width=320\n"
	writeFiles(t, dir, map[string]string{"override.cfg": userCfg})
	mustOK := func(name string, args map[string]any) map[string]any {
		t.Helper()
		out, ok := call(name, args)
		if !ok {
			logs, _ := call("godot_get_output", map[string]any{"max_lines": 60})
			t.Fatalf("%s %v: %v\ngame output: %s", name, args, out, mustJSON(logs))
		}
		return out
	}
	mustOK("godot_create_scene", map[string]any{"path": "res://main.tscn", "root": map[string]any{
		"type": "Node2D", "name": "Main", "children": []any{
			map[string]any{"type": "CharacterBody2D", "name": "Player", "script": "res://player.gd",
				"properties": map[string]any{"position": "Vector2(100, 50)"}},
			map[string]any{"type": "Label", "name": "Score", "properties": map[string]any{"text": "0"}},
		}}})

	run := mustOK("godot_run_project", map[string]any{"headless": true, "wait_seconds": 1})
	runID := run["run"].(map[string]any)["run_id"].(string)
	t.Cleanup(func() { call("godot_stop_project", map[string]any{"run_id": runID}) })

	// Дерево: ждёт подключения моста сам.
	tree := mustOK("godot_game_tree", map[string]any{"properties": []string{"position", "text"}})
	j := mustJSON(tree)
	for _, want := range []string{`"current_scene":"/root/Main"`, `"path":"/root/Main/Player"`, `"script":"res://player.gd"`,
		`"position":"Vector2(100, 50)"`, `"text":"0"`} {
		if !strings.Contains(j, want) {
			t.Errorf("tree has no %s:\n%s", want, j)
		}
	}
	if strings.Contains(j, "GodotMcpBridge") {
		t.Errorf("the bridge itself must be hidden from the tree")
	}
	// После подключения override.cfg пользователя возвращается как был.
	if data, _ := os.ReadFile(filepath.Join(dir, "override.cfg")); string(data) != userCfg {
		t.Errorf("override.cfg not restored while the game runs: %q", data)
	}

	eval := func(expr, node string) any {
		t.Helper()
		return mustOK("godot_game_eval", map[string]any{"expression": expr, "node": node})["value"]
	}
	x0 := eval("position.x", "Player").(float64)

	// Держим «вправо» полсекунды — персонаж уезжает вправо, потом останавливается.
	mustOK("godot_game_input", map[string]any{"steps": []any{
		map[string]any{"action": "move_right"},
		map[string]any{"wait": 0.5},
		map[string]any{"action": "move_right", "pressed": false},
	}})
	x1 := eval("position.x", "Player").(float64)
	if x1-x0 < 50 {
		t.Errorf("player should have moved right: %.1f -> %.1f", x0, x1)
	}
	if v := eval("velocity.x", "/root/Main/Player"); v != 0.0 {
		t.Errorf("after release velocity.x = %v", v)
	}

	// Вызов метода, изменение свойств, набор текста.
	if h := eval("take_damage(3)", "Player"); h != 7.0 {
		t.Errorf("take_damage(3) = %v", h)
	}
	set := mustOK("godot_game_set", map[string]any{"node": "Player", "properties": map[string]any{"speed": 50, "position:y": 300, "health": 99}})
	if mustJSON(set) != `{"health":99,"position:y":300,"speed":50}` {
		t.Errorf("set = %s", mustJSON(set))
	}
	mustOK("godot_game_input", map[string]any{"steps": []any{map[string]any{"text": "hi"}}})
	if s := eval("typed", "Player"); s != "hi" {
		t.Errorf("typed = %v", s)
	}
	if v := eval("tree.current_scene.name + ':' + str(scene.get_child_count())", ""); v != "Main:2" {
		t.Errorf("tree/scene variables: %v", v)
	}

	// Понятные ошибки.
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"godot_game_eval", map[string]any{"expression": "nope(", "node": "Player"}, "cannot parse expression"},
		{"godot_game_eval", map[string]any{"expression": "position", "node": "Ghost"}, "no node at 'Ghost'"},
		{"godot_game_set", map[string]any{"node": "Player", "properties": map[string]any{"mana": 1}}, "has no property 'mana'"},
		{"godot_game_input", map[string]any{"steps": []any{map[string]any{"action": "fly"}}}, "unknown input action 'fly'; defined: move_right, move_left"},
		{"godot_game_input", map[string]any{"steps": []any{map[string]any{"key": "NoSuchKey"}}}, "unknown key"},
		{"godot_game_screenshot", map[string]any{}, "runs headless"},
	} {
		out, ok := call(tc.tool, tc.args)
		if ok || !strings.Contains(fmt.Sprint(out["error"]), tc.want) {
			t.Errorf("%s %v: want error containing %q, got %v", tc.tool, tc.args, tc.want, out)
		}
	}

	runs := mustJSON(mustOK("godot_list_runs", nil))
	if !strings.Contains(runs, `"bridge":"connected"`) {
		t.Errorf("list_runs should show the bridge: %s", runs)
	}
	mustOK("godot_stop_project", map[string]any{"run_id": runID})
	if out, ok := call("godot_game_eval", map[string]any{"expression": "1"}); ok || !strings.Contains(fmt.Sprint(out["error"]), "has exited") {
		t.Errorf("after stop: %v", out)
	}

	// Без моста: override.cfg не трогается, инструменты объясняют, почему не работают.
	mustOK("godot_run_project", map[string]any{"headless": true, "no_bridge": true, "wait_seconds": 1})
	if out, ok := call("godot_game_tree", nil); ok || !strings.Contains(fmt.Sprint(out["error"]), "no_bridge") {
		t.Errorf("no_bridge run: %v", out)
	}
	call("godot_stop_project", nil)
	time.Sleep(100 * time.Millisecond)
	if data, _ := os.ReadFile(filepath.Join(dir, "override.cfg")); string(data) != userCfg {
		t.Errorf("override.cfg changed: %q", data)
	}
}

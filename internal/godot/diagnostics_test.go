package godot

import "testing"

func TestParseDiagnostics(t *testing.T) {
	out := []string{
		"hello from ready",
		"\x1b[1;31mSCRIPT ERROR: Invalid call. Nonexistent function 'foo' in base 'Nil'.\x1b[0m",
		"          at: _ready (res://good.gd:6)",
		"          GDScript backtrace (most recent call first):",
		"              [0] _ready (res://good.gd:6)",
		"              [1] _init (res://main.gd:3)",
		"SCRIPT ERROR: Parse Error: Identifier \"x\" not declared in the current scope.",
		"          at: GDScript::reload (res://bad.gd:4)",
		"ERROR: Failed to load script \"res://bad.gd\" with error \"Parse error\".",
		"   at: load (modules/gdscript/gdscript_resource_format.cpp:46)",
		"ERROR: Cannot open file 'res://missing.tscn'.",
		"   at: load (scene/resources/resource_format_text.cpp:1442)",
		"ERROR: boom from ready",
		"   at: push_error (core/variant/variant_utility.cpp:1024)",
		"   GDScript backtrace (most recent call first):",
		"       [0] _ready (res://main.gd:7)",
		"WARNING: 1 RID of type \"CanvasItem\" was leaked.",
		"   at: _free_rids (servers/rendering/renderer_canvas_cull.cpp:2733)",
	}
	ds := ParseDiagnostics(out)
	if len(ds) != 4 {
		t.Fatalf("got %d diagnostics: %+v", len(ds), ds)
	}
	if d := ds[0]; d.File != "res://good.gd" || d.Line != 6 || d.Function != "_ready" || len(d.Backtrace) != 2 || d.Kind != "script" {
		t.Errorf("runtime error parsed as %+v", d)
	}
	if d := ds[1]; d.File != "res://bad.gd" || d.Line != 4 {
		t.Errorf("parse error parsed as %+v", d)
	}
	if d := ds[2]; d.Kind != "engine" || d.Source != "scene/resources/resource_format_text.cpp:1442" {
		t.Errorf("engine error parsed as %+v", d)
	}
	// push_error(): "at:" указывает в C++, место в скрипте — верхний кадр backtrace.
	if d := ds[3]; d.Kind != "script" || d.File != "res://main.gd" || d.Line != 7 || d.Function != "_ready" || d.Source != "core/variant/variant_utility.cpp:1024" {
		t.Errorf("push_error parsed as %+v", d)
	}
}

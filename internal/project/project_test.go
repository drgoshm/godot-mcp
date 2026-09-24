package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestProject(t *testing.T) *Sandbox {
	t.Helper()
	dir := t.TempDir()
	write := func(p, s string) {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("project.godot", `; Engine configuration file.
config_version=5

[application]

config/name="Test \"Game\""
run/main_scene="uid://abc123"
config/features=PackedStringArray("4.7", "Forward Plus")

[autoload]

Global="*res://global.gd"

[input]

jump={
"deadzone": 0.2,
"events": [Object(InputEventKey,"keycode":32)]
}
move_left={
"deadzone": 0.2,
"events": []
}
`)
	write("main.tscn", "[gd_scene format=3 uid=\"uid://abc123\"]\n\n[node name=\"Main\" type=\"Node2D\"]\n")
	write("player.gd", "extends Node2D\n\tfunc _ready():\n\tpass\n")
	write("player.gd.uid", "uid://scriptuid\n")
	write(".godot/editor/x.cfg", "cache")
	sb, err := NewSandbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	return sb
}

func TestResolve(t *testing.T) {
	sb := newTestProject(t)
	ok := []string{"res://main.tscn", "main.tscn", "res://", "res://a/../main.tscn", "res://new/dir/file.gd"}
	for _, p := range ok {
		if _, err := sb.Resolve(p); err != nil {
			t.Errorf("Resolve(%q) unexpected error: %v", p, err)
		}
	}
	bad := []string{"res://../x", "../../etc/passwd", "/etc/passwd", "res://.godot/editor/x.cfg", "res://.git/config", "user://save.dat", "uid://abc"}
	for _, p := range bad {
		if _, err := sb.Resolve(p); err == nil {
			t.Errorf("Resolve(%q) should fail", p)
		}
	}
}

func TestSymlinkEscape(t *testing.T) {
	sb := newTestProject(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(sb.Root(), "link")); err != nil {
		t.Skip("symlinks unsupported:", err)
	}
	if _, err := sb.Resolve("res://link/evil.gd"); err == nil {
		t.Error("symlink escape was not detected")
	}
}

func TestLoadInfo(t *testing.T) {
	sb := newTestProject(t)
	info, err := sb.LoadInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != `Test "Game"` {
		t.Errorf("name = %q", info.Name)
	}
	if info.MainSceneFile != "res://main.tscn" {
		t.Errorf("main scene file = %q", info.MainSceneFile)
	}
	if strings.Join(info.Features, ",") != "4.7,Forward Plus" {
		t.Errorf("features = %v", info.Features)
	}
	if info.Autoloads["Global"] != "*res://global.gd" {
		t.Errorf("autoloads = %v", info.Autoloads)
	}
	if strings.Join(info.InputActions, ",") != "jump,move_left" {
		t.Errorf("input actions = %v", info.InputActions)
	}
	if p, err := sb.ResolveUID("uid://scriptuid"); err != nil || p != "res://player.gd" {
		t.Errorf("ResolveUID(script) = %q, %v", p, err)
	}
}

func TestListReadEdit(t *testing.T) {
	sb := newTestProject(t)
	entries, _, err := sb.List("res://", ListOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Path, "res://.godot") || strings.HasSuffix(e.Path, ".uid") {
			t.Errorf("listing leaked %s", e.Path)
		}
	}
	if _, err := sb.Write("res://scripts/enemy.gd", "extends Node\n\nfunc a():\n\tpass\n\nfunc b():\n\tpass\n", true); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.Edit("res://scripts/enemy.gd", "\tpass\n", "\treturn\n", false); err == nil {
		t.Error("ambiguous edit should fail")
	}
	n, err := sb.Edit("res://scripts/enemy.gd", "func b():\n\tpass", "func b():\n\tprint(1)", false)
	if err != nil || n != 1 {
		t.Fatalf("edit: n=%d err=%v", n, err)
	}
	res, err := sb.Read("res://scripts/enemy.gd", 6, 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "func b():\n\tprint(1)\n" || res.TotalLines != 7 {
		t.Errorf("read = %q (total %d)", res.Content, res.TotalLines)
	}
}

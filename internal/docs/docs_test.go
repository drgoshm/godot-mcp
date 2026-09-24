package docs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMarkdown(t *testing.T) {
	in := "Moves the body based on [member velocity]. See [method Node._physics_process] and [CharacterBody2D].\n" +
		"Use [code]delta[/code], [param up_direction] and [constant MOTION_MODE_GROUNDED].[br][b]Note:[/b] [url=https://x.y]docs[/url].\n" +
		"[codeblocks]\n\t\t[gdscript]\n\t\tfunc _ready():\n\t\t\tpass\n\t\t[/gdscript]\n\t\t[csharp]\n\t\tpublic override void _Ready() {}\n\t\t[/csharp]\n\t\t[/codeblocks]"
	got := markdown(in)
	for _, want := range []string{
		"based on `velocity`", "`Node._physics_process()`", "and CharacterBody2D.", "`delta`", "`up_direction`",
		"`MOTION_MODE_GROUNDED`", "\nNote: docs (https://x.y)", "```gdscript\nfunc _ready():\n\tpass\n```",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("markdown has no %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "csharp") || strings.Contains(got, "_Ready") || strings.Contains(got, "[") {
		t.Errorf("leftover markup or C#:\n%s", got)
	}
	if b := brief("First sentence here. Second one.", 140); b != "First sentence here." {
		t.Errorf("brief = %q", b)
	}
}

func TestSimilarAndPropType(t *testing.T) {
	names := []string{"CharacterBody2D", "CharacterBody3D", "RigidBody2D", "Node2D"}
	if s := similar("CharacterBody", names, 5); len(s) != 2 {
		t.Errorf("similar(CharacterBody) = %v", s)
	}
	if s := similar("Nod2D", names, 5); len(s) != 1 || s[0] != "Node2D" {
		t.Errorf("similar(Nod2D) = %v", s)
	}
	if got := propType("Texture2D,-AnimatedTexture,-AtlasTexture"); got != "Texture2D" {
		t.Errorf("propType = %q", got)
	}
	if got := propType("BaseMaterial3D,ShaderMaterial"); got != "BaseMaterial3D | ShaderMaterial" {
		t.Errorf("propType = %q", got)
	}
}

// Настоящая справка движка: GODOT_BIN=/path/to/godot.
func TestIndexFromEngine(t *testing.T) {
	bin := os.Getenv("GODOT_BIN")
	if bin == "" {
		t.Skip("GODOT_BIN not set")
	}
	cache := t.TempDir()
	l := &Loader{Bin: bin, Version: "test-version", CacheDir: cache}
	ix, err := l.Index(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ix.Version, "4.") {
		t.Errorf("version = %q", ix.Version)
	}

	look := func(name, member string) string {
		t.Helper()
		text, found := ix.Lookup(name, member, false, nil)
		if !found {
			t.Errorf("%s %s not found:\n%s", name, member, text)
		}
		return text
	}
	contains := func(label, text string, wants ...string) {
		t.Helper()
		for _, w := range wants {
			if !strings.Contains(text, w) {
				t.Errorf("%s has no %q:\n%s", label, w, text)
			}
		}
	}

	contains("CharacterBody2D", look("characterbody2d", ""),
		"# CharacterBody2D", "Inherits: PhysicsBody2D < CollisionObject2D < Node2D < CanvasItem < Node < Object",
		"- move_and_slide() -> bool — Moves the body based on `velocity`.",
		"- motion_mode: CharacterBody2D.MotionMode = 0",
		"- get_floor_angle(up_direction: Vector2 = Vector2(0, -1)) -> float [const]",
		"property setters/getters such as set_x()/get_x() are omitted",
		"enum MotionMode: MOTION_MODE_GROUNDED = 0, MOTION_MODE_FLOATING = 1")
	if o := look("CharacterBody2D", ""); strings.Contains(o, "- set_velocity(") || strings.Contains(o, "Godot Godot") {
		t.Errorf("overview lists accessors or repeats the engine name:\n%s", o[:600])
	}
	contains("accessor still reachable", look("CharacterBody2D", "set_velocity"), "set_velocity(velocity: Vector2) -> void")
	contains("inherited position", look("CharacterBody2D", "position"),
		"Inherited by CharacterBody2D from Node2D", "position: Vector2 = Vector2(0, 0)")
	contains("dot syntax + virtual", look("Node._process", ""), "virtual method", "_process(delta: float) -> void")
	contains("enum property", look("CharacterBody2D", "motion_mode"), "Values: MOTION_MODE_GROUNDED = 0, MOTION_MODE_FLOATING = 1")
	contains("signal", look("BaseButton", "pressed"), "signal pressed()")
	contains("builtin", look("Vector2", ""), "(built-in type)", "- Vector2(x: float, y: float)", "move_toward(to: Vector2, delta: float) -> Vector2")
	contains("global", look("@GlobalScope", "lerp"), "lerp(from: Variant, to: Variant, weight: Variant) -> Variant")
	contains("gdscript", look("@GDScript", ""), "preload(path: String) -> Resource", "@export")
	contains("singleton", look("Input", ""), "singleton")

	// Godot 3 -> 4.
	text, found := ix.Lookup("KinematicBody2D", "", false, nil)
	contains("rename", text, "Godot 3 name; in Godot 4 it is CharacterBody2D", "# CharacterBody2D")
	if !found {
		t.Error("renamed class should be found")
	}
	text, _ = ix.Lookup("YSort", "", false, nil)
	contains("removed", text, "does not exist in Godot 4", "y_sort_enabled")
	text, found = ix.Lookup("CharacterBody2D", "move_and_slide_with_snap", false, nil)
	contains("member rename", text, "has no member", "Godot 3 → 4: removed: set `velocity`")
	if found {
		t.Error("move_and_slide_with_snap must not be found")
	}
	text, _ = ix.Lookup("PackedScene", "instance", false, nil)
	contains("instance", text, "instantiate()")
	for old, r := range classRenames {
		if r.New != "" && ix.Class(r.New) == nil {
			t.Errorf("renames: %s -> %s, but %s does not exist in %s", old, r.New, r.New, ix.Version)
		}
		if r.New == "" && ix.Class(old) != nil {
			t.Errorf("renames: %s is marked removed but exists", old)
		}
	}

	// Опечатки, классы проекта, члены без класса.
	text, _ = ix.Lookup("CharacterBody2", "", false, nil)
	contains("typo", text, "Similar classes: CharacterBody2D")
	text, _ = ix.Lookup("ItemData", "", false, []ProjectClass{{Name: "ItemData", Base: "Resource", Path: "res://item.gd"}})
	contains("project class", text, "class of this project", "res://item.gd", "look up Resource")
	text, _ = ix.Lookup("move_and_slide", "", false, nil)
	contains("member as name", text, "is a member of: CharacterBody2D, CharacterBody3D")

	contains("search", ix.Search("raycast", 40), "- RayCast2D — class", "- RayCast3D — class")

	// Обзор огромного класса остаётся разумного размера.
	if n := len(look("Node", "")); n > 60_000 {
		t.Errorf("Node overview is %d bytes", n)
	}

	// Второй загрузчик берёт кеш и не запускает движок.
	l2 := &Loader{Bin: "/nonexistent/godot", Version: "test-version", CacheDir: cache}
	start := time.Now()
	if _, err := l2.Index(context.Background()); err != nil {
		t.Fatalf("cached index: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "test-version", extraFile)); err != nil {
		t.Errorf("extra.json missing: %v", err)
	}
	t.Logf("cached load took %s", time.Since(start))
}

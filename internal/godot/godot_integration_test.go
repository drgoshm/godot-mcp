package godot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Интеграционные тесты с настоящим движком: GODOT_BIN=/path/to/godot go test ./...
func testGodot(t *testing.T) *Godot {
	t.Helper()
	bin := os.Getenv("GODOT_BIN")
	if bin == "" {
		t.Skip("GODOT_BIN not set")
	}
	dir := t.TempDir()
	files := map[string]string{
		"project.godot": "config_version=5\n\n[application]\n\nconfig/name=\"T\"\nrun/main_scene=\"res://main.tscn\"\n",
		"main.tscn":     "[gd_scene format=3]\n\n[ext_resource type=\"Script\" path=\"res://main.gd\" id=\"1\"]\n\n[node name=\"Main\" type=\"Node\"]\nscript = ExtResource(\"1\")\n",
		"main.gd":       "extends Node\n\nfunc _ready() -> void:\n\tprint(\"ready!\")\n\tvar a: Variant = null\n\ta.boom()\n",
		"bad.gd":        "extends Node\n\nfunc _ready():\n\tundefined_thing()\n",
		"loop.gd":       "extends Node\n\nfunc _process(_d: float) -> void:\n\tpass\n",
	}
	for p, s := range files {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &Godot{Bin: bin, ProjectDir: dir}
}

func TestIntegrationCheckScript(t *testing.T) {
	g := testGodot(t)
	ctx := context.Background()
	res, err := g.CheckScript(ctx, "res://bad.gd")
	if err != nil {
		t.Fatal(err)
	}
	if res.OK() || len(res.Diagnostics) != 1 || res.Diagnostics[0].Line != 4 {
		t.Errorf("bad.gd: %+v", res)
	}
	res, err = g.CheckScript(ctx, "res://main.gd")
	if err != nil || !res.OK() {
		t.Errorf("main.gd should pass: %+v %v", res, err)
	}
}

func TestIntegrationRunAndStop(t *testing.T) {
	g := testGodot(t)
	r := NewRunner(g)
	ctx := context.Background()

	run, err := r.Start(StartOptions{Headless: true, QuitAfter: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !run.Wait(ctx, 30*time.Second) {
		t.Fatal("run with quit_after did not exit")
	}
	lines, _, _ := run.Output(0, 0)
	var all []string
	for _, l := range lines {
		all = append(all, l.Text)
	}
	if !strings.Contains(strings.Join(all, "\n"), "ready!") {
		t.Errorf("missing print output: %v", all)
	}
	ds := LinesDiagnostics(lines)
	if len(ds) == 0 || ds[0].File != "res://main.gd" || ds[0].Line != 6 {
		t.Errorf("runtime error not parsed: %+v", ds)
	}

	// Бесконечный запуск должен останавливаться.
	if err := os.WriteFile(filepath.Join(g.ProjectDir, "loop.tscn"),
		[]byte("[gd_scene format=3]\n\n[ext_resource type=\"Script\" path=\"res://loop.gd\" id=\"1\"]\n\n[node name=\"L\" type=\"Node\"]\nscript = ExtResource(\"1\")\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run2, err := r.Start(StartOptions{Headless: true, Scene: "res://loop.tscn"})
	if err != nil {
		t.Fatal(err)
	}
	if run2.Wait(ctx, 2*time.Second) {
		t.Fatal("endless scene exited early")
	}
	if _, err := r.Stop(ctx, run2.ID); err != nil {
		t.Fatal(err)
	}
	if run2.Alive() {
		t.Error("run still alive after Stop")
	}
}

func TestIntegrationRunSource(t *testing.T) {
	g := testGodot(t)
	res, err := g.RunSource(context.Background(), "extends SceneTree\n\nfunc _init() -> void:\n\tprint(\"sum=\", 2 + 3)\n\tquit()\n", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "sum=5") || !res.OK() {
		t.Errorf("unexpected result: %+v", res)
	}
}

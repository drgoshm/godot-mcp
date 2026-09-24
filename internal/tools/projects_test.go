package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/drgoshm/godot-mcp/internal/docs"
)

func newProject(t *testing.T, dir, name string) string {
	t.Helper()
	must(t, os.MkdirAll(dir, 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "project.godot"),
		[]byte("config_version=5\n\n[application]\n\nconfig/name=\""+name+"\"\n"), 0o644))
	return dir
}

func TestProjects(t *testing.T) {
	bin := os.Getenv("GODOT_BIN")
	if bin == "" {
		t.Skip("GODOT_BIN not set")
	}
	base := t.TempDir()
	projA := newProject(t, filepath.Join(base, "elsewhere", "alpha"), "Alpha")
	root := filepath.Join(base, "games")
	projB := newProject(t, filepath.Join(root, "beta"), "Beta")
	projC := newProject(t, filepath.Join(root, "team", "gamma"), "Gamma")
	newProject(t, filepath.Join(projC, "addons", "demo"), "Nested addon demo") // внутри проекта — не отдельный проект
	cfg := filepath.Join(base, "projects.cfg")
	must(t, os.WriteFile(cfg, []byte(fmt.Sprintf("[%s]\n\nfavorite=true\n\n[%s]\n\nfavorite=false\n\n[%s]\n\nfavorite=false\n",
		projA, filepath.Join(base, "deleted"), projB)), 0o644))

	ws := &Workspace{Bin: bin, Version: "test", UseLSP: false, EditorProjects: cfg,
		Docs: &docs.Loader{Bin: bin, Version: "test", CacheDir: t.TempDir()}}
	call, _ := connect(t, ws)
	errOf := func(name string, args map[string]any) string {
		t.Helper()
		out, ok := call(name, args)
		if ok {
			t.Fatalf("%s %v should fail: %v", name, args, out)
		}
		return fmt.Sprint(out["error"])
	}
	mustOK := func(name string, args map[string]any) map[string]any {
		t.Helper()
		out, ok := call(name, args)
		if !ok {
			t.Fatalf("%s %v: %v", name, args, out)
		}
		return out
	}

	// Без выбранного проекта инструменты объясняют, что делать; справка по движку работает.
	if e := errOf("godot_project_info", nil); !strings.Contains(e, "godot_select_project") {
		t.Errorf("no project: %s", e)
	}
	mustOK("godot_class_docs", map[string]any{"name": "Node2D"})

	// Список: проекты из менеджера Godot (избранные выше), удалённые пропущены.
	list := mustOK("godot_list_projects", nil)
	j := mustJSON(list)
	if !strings.Contains(j, `"name":"Alpha","path":"`+projA) || !strings.Contains(j, `"name":"Beta"`) || strings.Contains(j, "deleted") {
		t.Errorf("list: %s", j)
	}
	if first := list["projects"].([]any)[0].(map[string]any); first["name"] != "Alpha" || first["favorite"] != true {
		t.Errorf("favorites first: %s", j)
	}

	// Выбор проекта (можно путём к project.godot) и работа с другим проектом без переключения.
	info := mustOK("godot_select_project", map[string]any{"path": filepath.Join(projA, "project.godot")})
	if info["project"].(map[string]any)["name"] != "Alpha" {
		t.Errorf("select: %v", info)
	}
	mustOK("godot_write_file", map[string]any{"path": "res://note.txt", "content": "in alpha"})
	mustOK("godot_write_file", map[string]any{"project": projB, "path": "res://note.txt", "content": "in beta"})
	if a, _ := os.ReadFile(filepath.Join(projA, "note.txt")); string(a) != "in alpha" {
		t.Errorf("alpha note = %q", a)
	}
	if b, _ := os.ReadFile(filepath.Join(projB, "note.txt")); string(b) != "in beta" {
		t.Errorf("beta note = %q", b)
	}
	if r := mustOK("godot_read_file", map[string]any{"path": "res://note.txt"}); r["content"] != "in alpha" {
		t.Errorf("active project read: %v", r)
	}

	// Запуск в другом проекте; вывод находится по одному run_id, хотя активен Alpha.
	mustOK("godot_write_file", map[string]any{"project": projB, "path": "res://main.gd",
		"content": "extends Node\n\nfunc _ready() -> void:\n\tprint(\"hello from \", ProjectSettings.get_setting(\"application/config/name\"))\n"})
	mustOK("godot_create_scene", map[string]any{"project": projB, "path": "res://main.tscn",
		"root": map[string]any{"type": "Node", "name": "Main", "script": "res://main.gd"}})
	started := mustOK("godot_run_project", map[string]any{"project": projB, "scene": "res://main.tscn", "headless": true, "quit_after": 30, "no_bridge": true})
	runID := started["run"].(map[string]any)["run_id"].(string)
	out := mustOK("godot_get_output", map[string]any{"run_id": runID, "wait_seconds": 10})
	if !strings.Contains(mustJSON(out), "hello from Beta") {
		t.Errorf("output of beta's run: %s", mustJSON(out))
	}
	if runs := mustJSON(mustOK("godot_list_runs", nil)); strings.Contains(runs, runID) {
		t.Errorf("list_runs of the active project (Alpha) must not show Beta's runs: %s", runs)
	}
	if e := errOf("godot_run_project", map[string]any{"headless": true}); !strings.Contains(e, "main scene") {
		t.Errorf("alpha has no main scene: %s", e)
	}
	// Списки: открытые в сессии проекты отмечены, активный первым.
	if j := mustJSON(mustOK("godot_list_projects", map[string]any{"query": "a"})); !strings.Contains(j, `"active":true,"favorite":true,"name":"Alpha"`) || !strings.Contains(j, `"name":"Beta","open":true`) {
		t.Errorf("list after opening: %s", j)
	}

	// Ошибки путей.
	if e := errOf("godot_select_project", map[string]any{"path": base}); !strings.Contains(e, "not a Godot project") {
		t.Errorf("not a project: %s", e)
	}

	// --root: проекты только из разрешённых каталогов, и они же ищутся для списка.
	limited := &Workspace{Bin: bin, Version: "test", EditorProjects: cfg, Roots: []string{root},
		Docs: &docs.Loader{Bin: bin, Version: "test", CacheDir: t.TempDir()}}
	callLimited, _ := connect(t, limited)
	if out, ok := callLimited("godot_select_project", map[string]any{"path": projA}); ok || !strings.Contains(fmt.Sprint(out["error"]), "outside the allowed project roots") {
		t.Errorf("root restriction: %v", out)
	}
	lj := func() string { out, _ := callLimited("godot_list_projects", nil); return mustJSON(out) }()
	if strings.Contains(lj, "Alpha") || !strings.Contains(lj, `"name":"Beta"`) || !strings.Contains(lj, `"name":"Gamma","path":"`+projC+`","source":"root"`) ||
		strings.Contains(lj, "Nested addon") {
		t.Errorf("limited list: %s", lj)
	}
	if out, ok := callLimited("godot_read_file", map[string]any{"project": projA, "path": "res://note.txt"}); ok {
		t.Errorf("project param must respect roots too: %v", out)
	}
}

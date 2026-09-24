package lsp

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/drgoshm/godot-mcp/internal/godot"
)

func TestReadMessage(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("Content-Length: 7\r\nContent-Type: x\r\n\r\n{\"a\":1}Content-Length: 2\r\n\r\n{}"))
	for _, want := range []string{`{"a":1}`, `{}`} {
		got, err := readMessage(r)
		if err != nil || string(got) != want {
			t.Fatalf("readMessage = %q, %v; want %q", got, err, want)
		}
	}
	if fileURI("/tmp/a b/x.gd") != "file:///tmp/a%20b/x.gd" {
		t.Errorf("fileURI = %s", fileURI("/tmp/a b/x.gd"))
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// mtime с точностью до файловой системы: не даём двум записям совпасть по времени.
	time.Sleep(20 * time.Millisecond)
}

func TestCheckerWithEngine(t *testing.T) {
	bin := os.Getenv("GODOT_BIN")
	if bin == "" {
		t.Skip("GODOT_BIN not set")
	}
	dir := t.TempDir()
	write(t, dir, "project.godot", "config_version=5\n\n[application]\n\nconfig/name=\"L\"\n")
	write(t, dir, "item.gd", "class_name ItemData\nextends Resource\n\nvar power: int = 1\n")
	write(t, dir, "uses.gd", "extends Node\n\nfunc _ready() -> void:\n\tvar i: ItemData = ItemData.new()\n\tvar x: int = i.power\n\tvar unused := 5\n\tprint(x)\n")
	write(t, dir, "bad.gd", "extends Node\n\nfunc _ready() -> void:\n\tundefined_call()\n")
	// Как и --check-only, редактору нужен импортированный проект; он сделает это сам при запуске.
	c := &Checker{Bin: bin, ProjectDir: dir, IdleTimeout: time.Minute}
	defer c.Close()
	ctx := context.Background()

	check := func(paths ...string) map[string][]godot.Diagnostic {
		t.Helper()
		out, err := c.Check(ctx, paths)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	errorsOf := func(ds []godot.Diagnostic) []string {
		var msgs []string
		for _, d := range ds {
			if d.Severity == "error" {
				msgs = append(msgs, d.Message)
			}
		}
		return msgs
	}

	start := time.Now()
	out := check("res://bad.gd", "res://uses.gd")
	t.Logf("first check (editor start) took %s", time.Since(start))
	if bad := out["res://bad.gd"]; len(bad) != 1 || bad[0].Line != 4 || bad[0].Severity != "error" || !strings.Contains(bad[0].Message, "undefined_call") {
		t.Errorf("bad.gd = %+v", bad)
	}
	uses := out["res://uses.gd"]
	if len(errorsOf(uses)) != 0 || len(uses) != 1 || uses[0].Severity != "warning" || !strings.Contains(uses[0].Message, "UNUSED_VARIABLE") {
		t.Errorf("uses.gd = %+v", uses)
	}

	// Повторная проверка — без запуска процесса.
	write(t, dir, "bad.gd", "extends Node\n\nfunc _ready() -> void:\n\tpass\n")
	start = time.Now()
	out = check("res://bad.gd")
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("a check with a running editor took %s", d)
	}
	if len(out["res://bad.gd"]) != 0 {
		t.Errorf("fixed bad.gd = %+v", out["res://bad.gd"])
	}

	// Зависимость изменилась на диске — редактор должен узнать об этом до проверки.
	write(t, dir, "item.gd", "class_name ItemData\nextends Resource\n\nvar power: String = \"a\"\n")
	if msgs := errorsOf(check("res://uses.gd")["res://uses.gd"]); len(msgs) != 1 || !strings.Contains(msgs[0], "String") {
		t.Errorf("uses.gd after item.power became String: %v", msgs)
	}

	// Новый class_name: редактор перезапускается и видит класс.
	write(t, dir, "weapon.gd", "class_name Weapon\nextends Resource\n\nvar damage: int = 3\n")
	write(t, dir, "armed.gd", "extends Node\n\nfunc _ready() -> void:\n\tvar w: Weapon = Weapon.new()\n\tprint(w.damage)\n")
	if msgs := errorsOf(check("res://armed.gd")["res://armed.gd"]); len(msgs) != 0 {
		t.Errorf("new class_name not picked up: %v", msgs)
	}

	// Простой: редактор останавливается и снова стартует по требованию.
	c.IdleTimeout = 300 * time.Millisecond
	check("res://armed.gd")
	time.Sleep(time.Second)
	if c.Running() {
		t.Error("the editor should stop after the idle timeout")
	}
	if len(check("res://bad.gd")["res://bad.gd"]) != 0 || !c.Running() {
		t.Error("the editor should restart on demand")
	}
}

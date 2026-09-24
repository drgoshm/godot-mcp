package godot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOverrideCfg(t *testing.T) {
	dir := t.TempDir()
	o := &overrideCfg{dir: dir}
	cfg := filepath.Join(dir, "override.cfg")

	// Файла не было: появляется на время запуска и исчезает после.
	must(t, o.acquire())
	must(t, o.acquire()) // вторая игра стартует, пока первая не подключилась
	data, _ := os.ReadFile(cfg)
	if !strings.Contains(string(data), `GodotMcpBridge="*res://.godot/godot_mcp_bridge.gd"`) {
		t.Fatalf("override.cfg = %q", data)
	}
	if _, err := os.Stat(filepath.Join(dir, ".godot", "godot_mcp_bridge.gd")); err != nil {
		t.Errorf("bridge script not written: %v", err)
	}
	o.release()
	if _, err := os.Stat(cfg); err != nil {
		t.Error("override.cfg removed while another run still needs it")
	}
	o.release()
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Error("override.cfg must be removed after the last run connected")
	}

	// Файл пользователя: дописываемся в конец и возвращаем его байт в байт.
	user := "[autoload]\n\nUserAuto=\"*res://user.gd\"\n\n[display]\nwindow/size/viewport_width=320" // без \n в конце
	must(t, os.WriteFile(cfg, []byte(user), 0o644))
	must(t, o.acquire())
	data, _ = os.ReadFile(cfg)
	if !strings.HasPrefix(string(data), user) || !strings.Contains(string(data), overrideMarker) {
		t.Errorf("merged override.cfg = %q", data)
	}
	o.release()
	if data, _ = os.ReadFile(cfg); string(data) != user {
		t.Errorf("user override.cfg not restored exactly: %q", data)
	}

	// Остаток после аварийного выхода сервера убирается при старте.
	must(t, o.acquire())
	stale := &overrideCfg{dir: dir}
	stale.cleanupStale()
	if data, _ = os.ReadFile(cfg); strings.Contains(string(data), overrideMarker) || !strings.Contains(string(data), "UserAuto") {
		t.Errorf("after cleanup: %q", data)
	}
	os.Remove(cfg)
	must(t, o.acquire())
	(&overrideCfg{dir: dir}).cleanupStale()
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Error("a file that only held our section must be removed")
	}
	o.forceRestore()
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

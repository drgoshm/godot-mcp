package tools

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Настоящие фреймворки: GUT_ADDON_DIR=.../addons/gut, GDUNIT4_ADDON_DIR=.../addons/gdUnit4.
func installAddon(t *testing.T, env, dir, name string) {
	t.Helper()
	src := os.Getenv(env)
	if src == "" {
		t.Skip(env + " not set")
	}
	must(t, copyDir(src, filepath.Join(dir, "addons", name)))
}

func TestRunTestsGUT(t *testing.T) {
	call, dir, _ := newSession(t, "GUT")
	installAddon(t, "GUT_ADDON_DIR", dir, "gut")
	writeFiles(t, dir, map[string]string{
		"test/unit/test_calc.gd": "extends GutTest\n\nfunc test_passes() -> void:\n\tassert_eq(2 + 2, 4)\n\nfunc test_fails() -> void:\n\tassert_eq(2 + 2, 5, \"math is hard\")\n\nfunc test_crashes() -> void:\n\tvar a: Variant = null\n\ta.boom()\n\nfunc test_pending() -> void:\n\tpending(\"later\")\n",
		// GUT молча пропускает файлы, которые не компилируются, и выходит с кодом 0.
		"test/unit/test_broken.gd": "extends GutTest\n\nfunc test_broken() -> void:\n\tassert_eq(undefined_var, 1)\n",
	})

	out, ok := call("godot_run_tests", nil)
	if !ok {
		t.Fatalf("run_tests: %v", out)
	}
	j := mustJSON(out)
	if out["framework"] != "gut" || out["passed"] != false || out["imported"] != true {
		t.Errorf("header: %s", j)
	}
	if got := mustJSON(out["totals"]); got != `{"errors":0,"failed":2,"passed":1,"skipped":1,"tests":4}` {
		t.Errorf("totals = %s", got)
	}
	for _, want := range []string{
		`"file":"res://test/unit/test_calc.gd","line":7,"message":"[4] expected to equal [5]:  math is hard`,
		// строки нет в отчёте GUT ("at line -1") — берётся из вывода движка
		`"file":"res://test/unit/test_calc.gd","line":11`,
	} {
		if !strings.Contains(j, want) {
			t.Errorf("run_tests has no %s:\n%s", want, j)
		}
	}
	assertLoadError(t, out, "res://test/unit/test_broken.gd", 4)
	if strings.Contains(j, "remote port") || strings.Contains(j, "Remote Debugger") || strings.Contains(j, "at line") {
		t.Errorf("remote debugger noise leaked into results: %s", j)
	}

	// Фильтр по имени и путь к одному файлу.
	must(t, os.Remove(filepath.Join(dir, "test/unit/test_broken.gd")))
	out, ok = call("godot_run_tests", map[string]any{"framework": "gut", "paths": []string{"res://test/unit/test_calc.gd"}, "test": "passes"})
	if !ok || out["passed"] != true || out["totals"].(map[string]any)["passed"] != float64(1) || out["totals"].(map[string]any)["failed"] != float64(0) {
		t.Errorf("filtered run: %s", mustJSON(out))
	}
}

func TestRunTestsGdUnit4(t *testing.T) {
	call, dir, _ := newSession(t, "GdUnit")
	installAddon(t, "GDUNIT4_ADDON_DIR", dir, "gdUnit4")
	writeFiles(t, dir, map[string]string{
		"test/calc_test.gd":   "extends GdUnitTestSuite\n\nfunc test_passes() -> void:\n\tassert_int(2 + 2).is_equal(4)\n\nfunc test_fails() -> void:\n\tassert_int(2 + 2).is_equal(5)\n\nfunc test_crashes() -> void:\n\tvar a: Variant = null\n\ta.boom()\n",
		"test/broken_test.gd": "extends GdUnitTestSuite\n\nfunc test_broken() -> void:\n\tassert_int(undefined_var).is_equal(1)\n",
	})

	// gdUnit4 при ошибке разбора не запускает ничего и отчёта не пишет.
	out, ok := call("godot_run_tests", nil)
	if !ok || out["passed"] != false || !strings.Contains(fmt.Sprint(out["hint"]), "fix the scripts in load_errors") || out["output"] != nil {
		t.Errorf("broken suite: %s", mustJSON(out))
	}
	assertLoadError(t, out, "res://test/broken_test.gd", 4)

	must(t, os.Remove(filepath.Join(dir, "test/broken_test.gd")))
	out, ok = call("godot_run_tests", map[string]any{"paths": []string{"res://test"}})
	j := mustJSON(out)
	if !ok || out["framework"] != "gdunit4" || out["passed"] != false {
		t.Fatalf("run_tests: %s", j)
	}
	if got := mustJSON(out["totals"]); got != `{"errors":1,"failed":1,"passed":1,"skipped":0,"tests":3}` {
		t.Errorf("totals = %s", got)
	}
	for _, want := range []string{`"line":7`, `"line":11`, `"status":"error"`, `Nonexistent function 'boom'`} {
		if !strings.Contains(j, want) {
			t.Errorf("run_tests has no %s:\n%s", want, j)
		}
	}
	if out, ok := call("godot_run_tests", map[string]any{"test": "passes"}); ok || !strings.Contains(fmt.Sprint(out["error"]), "GUT only") {
		t.Errorf("name filter must be rejected for gdUnit4: %v", out)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, ".godot", "mcp-test-reports")); len(entries) > 0 {
		t.Errorf("reports left behind: %v", entries)
	}

	// Оба фреймворка — нужно выбрать явно.
	installAddon(t, "GUT_ADDON_DIR", dir, "gut")
	if out, ok := call("godot_run_tests", nil); ok || !strings.Contains(fmt.Sprint(out["error"]), "pass framework") {
		t.Errorf("both frameworks: %v", out)
	}
}

// assertLoadError: ровно одна ошибка загрузки — в нужном файле и строке,
// а не дубль "Failed to load script" с местом в коде фреймворка.
func assertLoadError(t *testing.T, out map[string]any, file string, line int) {
	t.Helper()
	errs, _ := out["load_errors"].([]any)
	if len(errs) != 1 {
		t.Fatalf("want 1 load error, got %s", mustJSON(out["load_errors"]))
	}
	e := errs[0].(map[string]any)
	if e["file"] != file || e["line"] != float64(line) || e["backtrace"] != nil {
		t.Errorf("load error = %s", mustJSON(e))
	}
	for _, a := range asSlice(out["addon_errors"]) {
		if strings.Contains(mustJSON(a), file) {
			t.Errorf("%s must not be reported as an addon error: %s", file, mustJSON(a))
		}
	}
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(content), 0o644))
	}
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/drgoshm/godot-mcp/internal/godot"
)

type RunTestsIn struct {
	ProjectArg
	Framework      string   `json:"framework,omitempty" jsonschema:"gut or gdunit4; detected from addons/ when only one is installed"`
	Paths          []string `json:"paths,omitempty" jsonschema:"res:// test directories or files; default: .gutconfig.json for GUT, else res://test or res://tests"`
	Test           string   `json:"test,omitempty" jsonschema:"GUT only: run tests whose name contains this text"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty" jsonschema:"kill the run after this many seconds (default 300, max 1800)"`
}

type RunTestsOut struct {
	Framework   string             `json:"framework"`
	Passed      bool               `json:"passed"`
	Totals      godot.TestTotals   `json:"totals"`
	Failures    []godot.TestCase   `json:"failures,omitempty"`
	LoadErrors  []godot.Diagnostic `json:"load_errors,omitempty"`
	AddonErrors []godot.Diagnostic `json:"addon_errors,omitempty"`
	ExitCode    int                `json:"exit_code"`
	Duration    string             `json:"duration"`
	Imported    bool               `json:"imported,omitempty"`
	Hint        string             `json:"hint,omitempty"`
	Output      string             `json:"output,omitempty"`
}

const runTestsDescription = `Run the project's unit tests with GUT or gdUnit4 (the addon must be in addons/) and return
structured results: totals and every failed test with file, line and message.
load_errors lists test scripts that failed to compile — their tests did not run at all (GUT silently skips them).
addon_errors lists errors inside addons/, e.g. a test framework version that does not match the engine.`

// framework описывает, как запускать один тестовый фреймворк.
type framework struct {
	name   string
	script string // res://-путь CLI-скрипта
	class  string // class_name базового класса тестов — есть в кеше после импорта
}

var frameworks = []framework{
	{name: "gut", script: "res://addons/gut/gut_cmdln.gd", class: "GutTest"},
	{name: "gdunit4", script: "res://addons/gdUnit4/bin/GdUnitCmdTool.gd", class: "GdUnitTestSuite"},
}

// Шум от --remote-debug tcp://127.0.0.1:0 (см. runTests).
var remoteDebugNoise = []string{"remote port number", "Remote Debugger: Unable to connect"}

func registerTestTools(s *mcp.Server, w *Workspace) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "godot_run_tests",
		Description: runTestsDescription,
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in RunTestsIn) (*mcp.CallToolResult, RunTestsOut, error) {
		d, err := w.Deps(in.Project)
		if err != nil {
			return nil, RunTestsOut{}, err
		}
		out, err := runTests(ctx, d, in)
		return nil, out, err
	})
}

func runTests(ctx context.Context, d *Deps, in RunTestsIn) (RunTestsOut, error) {
	var out RunTestsOut
	fw, err := pickFramework(d, in.Framework)
	if err != nil {
		return out, err
	}
	out.Framework = fw.name
	if in.Test != "" && fw.name != "gut" {
		return out, errors.New("test name filter is supported for GUT only; with gdUnit4 pass the test file in paths")
	}

	// Без импорта class_name фреймворка неизвестен и ни один тест не загрузится.
	if !classCached(d, fw.class) {
		if _, err := d.Godot.Import(ctx); err != nil {
			return out, err
		}
		out.Imported = true
		if !classCached(d, fw.class) {
			return out, fmt.Errorf("class %s is not registered even after import; is the %s addon complete?", fw.class, fw.name)
		}
	}

	dirs, files, err := testPaths(d, fw, in.Paths)
	if err != nil {
		return out, err
	}

	// Отчёт кладём в .godot/: gdUnit4 понимает только пути внутри проекта,
	// а .godot — служебный каталог движка, который не попадает в git.
	runID := fmt.Sprintf("run-%d", time.Now().UnixNano())
	reportAbs := filepath.Join(d.Sandbox.Root(), ".godot", "mcp-test-reports", runID)
	reportRes := "res://.godot/mcp-test-reports/" + runID
	if err := os.MkdirAll(reportAbs, 0o755); err != nil {
		return out, err
	}
	defer os.RemoveAll(reportAbs)

	// -d нужен фреймворкам для отслеживания ошибок, а --remote-debug на
	// заведомо закрытый порт не даёт Godot уйти в интерактивный отладчик
	// (бесконечный "debug>") при ошибке разбора — так же делает runtest.sh gdUnit4.
	args := []string{"--headless", "-d", "--remote-debug", "tcp://127.0.0.1:0", "-s", fw.script}
	switch fw.name {
	case "gut":
		args = append(args, "-gexit", "-gdisable_colors", "-ginclude_subdirs", "-gjunit_xml_file="+reportRes+"/results.xml")
		if len(dirs) > 0 {
			args = append(args, "-gdir="+strings.Join(dirs, ","))
		}
		if len(files) > 0 {
			args = append(args, "-gtest="+strings.Join(files, ","))
		}
		if in.Test != "" {
			args = append(args, "-gunit_test_name="+in.Test)
		}
	case "gdunit4":
		for _, p := range append(dirs, files...) {
			args = append(args, "-a", p)
		}
		args = append(args, "-c", "--ignoreHeadlessMode", "-rd", reportRes)
	}

	res, err := d.Godot.Exec(ctx, clampSeconds(in.TimeoutSeconds, 300, 1800), args...)
	if err != nil {
		return out, err
	}
	out.ExitCode, out.Duration = res.ExitCode, res.Duration
	classify(&out, res.Diagnostics)

	report := findReport(reportAbs)
	if report != nil {
		cases, totals, err := godot.ParseJUnit(report)
		if err != nil {
			return out, fmt.Errorf("cannot parse the test report: %w", err)
		}
		out.Totals = totals
		for _, c := range cases {
			if c.Status == "failed" || c.Status == "error" {
				fillLine(&c, res.Diagnostics)
				out.Failures = append(out.Failures, c)
			}
		}
	}

	out.Passed = report != nil && !res.TimedOut && out.Totals.Tests > 0 &&
		out.Totals.Failed == 0 && out.Totals.Errors == 0 && len(out.LoadErrors) == 0
	switch {
	case res.TimedOut:
		out.Hint = "the run timed out; a test may be stuck (await without timeout?) — raise timeout_seconds or run fewer tests"
	case report == nil && len(out.LoadErrors) > 0:
		out.Hint = "tests did not run: fix the scripts in load_errors and run again"
	case report == nil:
		out.Hint = "the framework produced no report; see output"
	case out.Totals.Tests == 0:
		out.Hint = "no tests found; check paths and file naming (GUT: test_*.gd, gdUnit4: *_test.gd)"
	case len(out.LoadErrors) > 0:
		out.Hint = "some test scripts failed to load and their tests were not run"
	}
	if res.TimedOut || (report == nil && len(out.LoadErrors) == 0) {
		out.Output = tailLines(res.Output, 40)
	}
	return out, nil
}

func pickFramework(d *Deps, name string) (framework, error) {
	var installed []framework
	for _, fw := range frameworks {
		if name != "" && fw.name != name {
			continue
		}
		abs, err := d.Sandbox.Resolve(fw.script)
		if err != nil {
			return framework{}, err
		}
		if _, err := os.Stat(abs); err == nil {
			installed = append(installed, fw)
		} else if name != "" {
			return framework{}, fmt.Errorf("%s is not installed: %s not found", name, fw.script)
		}
	}
	switch {
	case name != "" && len(installed) == 0:
		return framework{}, fmt.Errorf("unknown framework %q; use gut or gdunit4", name)
	case len(installed) == 0:
		return framework{}, errors.New("no test framework found: install GUT (addons/gut) or gdUnit4 (addons/gdUnit4)")
	case len(installed) > 1:
		return framework{}, errors.New("both GUT and gdUnit4 are installed; pass framework")
	}
	return installed[0], nil
}

func classCached(d *Deps, class string) bool {
	data, err := os.ReadFile(filepath.Join(d.Sandbox.Root(), ".godot", "global_script_class_cache.cfg"))
	return err == nil && strings.Contains(string(data), `&"`+class+`"`)
}

// testPaths делит пути на каталоги и файлы. Без путей: GUT берёт каталоги из
// .gutconfig.json, иначе ищем res://test или res://tests.
func testPaths(d *Deps, fw framework, paths []string) (dirs, files []string, err error) {
	if len(paths) == 0 {
		if fw.name == "gut" {
			if _, err := os.Stat(filepath.Join(d.Sandbox.Root(), ".gutconfig.json")); err == nil {
				return nil, nil, nil
			}
		}
		for _, p := range []string{"res://test", "res://tests"} {
			if abs, err := d.Sandbox.Resolve(p); err == nil {
				if info, err := os.Stat(abs); err == nil && info.IsDir() {
					return []string{p}, nil, nil
				}
			}
		}
		return nil, nil, errors.New("no res://test or res://tests directory; pass paths")
	}
	for _, p := range paths {
		abs, err := d.Sandbox.Resolve(p)
		if err != nil {
			return nil, nil, err
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, nil, fmt.Errorf("%s does not exist", p)
		}
		if info.IsDir() {
			dirs = append(dirs, d.Sandbox.ToRes(abs))
		} else {
			files = append(files, d.Sandbox.ToRes(abs))
		}
	}
	return dirs, files, nil
}

// classify раскладывает ошибки движка: скрипты, которые не загрузились (их тесты
// не выполнялись), и ошибки внутри addons/. Ошибки выполнения внутри тестов уже
// есть в отчёте как упавшие тесты.
func classify(out *RunTestsOut, ds []godot.Diagnostic) {
	for _, d := range ds {
		if d.Severity != "error" || isRemoteDebugNoise(d.Message) {
			continue
		}
		inAddon := strings.HasPrefix(d.File, "res://addons/") || strings.Contains(d.Message, "res://addons/")
		d.Backtrace = nil // при загрузке это стек фреймворка, агенту он не нужен
		switch {
		case inAddon:
			out.AddonErrors = append(out.AddonErrors, d)
		case strings.Contains(d.Message, "Parse Error") || strings.HasPrefix(d.Message, "Failed to load script"):
			out.LoadErrors = append(out.LoadErrors, d)
		}
	}
}

func isRemoteDebugNoise(msg string) bool {
	for _, n := range remoteDebugNoise {
		if strings.Contains(msg, n) {
			return true
		}
	}
	return false
}

// fillLine: GUT не знает строку ошибки выполнения ("at line -1"), а движок
// печатает её в SCRIPT ERROR с функцией теста.
func fillLine(c *godot.TestCase, ds []godot.Diagnostic) {
	if c.Line > 0 {
		return
	}
	for _, d := range ds {
		if d.File == c.File && d.Function == c.Name && d.Line > 0 {
			c.Line = d.Line
			return
		}
	}
}

// findReport ищет JUnit XML: GUT пишет results.xml прямо в каталог,
// gdUnit4 — в report_N/results.xml.
func findReport(dir string) []byte {
	for _, pattern := range []string{"results.xml", "report_*/results.xml"} {
		matches, _ := filepath.Glob(filepath.Join(dir, pattern))
		for _, m := range matches {
			if data, err := os.ReadFile(m); err == nil && len(data) > 0 {
				return data
			}
		}
	}
	return nil
}

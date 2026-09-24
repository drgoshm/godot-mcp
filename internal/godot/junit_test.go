package godot

import (
	"os"
	"strings"
	"testing"
)

// Отчёты сняты с настоящих GUT 9.7.1 и gdUnit4 6.2.1 на одинаковых тестах:
// проходит, падает на assert, падает с ошибкой выполнения (+ pending у GUT).
func TestParseJUnitGdUnit4(t *testing.T) {
	data, err := os.ReadFile("testdata/gdunit4.xml")
	if err != nil {
		t.Fatal(err)
	}
	cases, totals, err := ParseJUnit(data)
	if err != nil {
		t.Fatal(err)
	}
	if totals != (TestTotals{Tests: 3, Passed: 1, Failed: 1, Errors: 1}) {
		t.Errorf("totals = %+v", totals)
	}
	byName := index(cases)
	if c := byName["test_fails"]; c.Status != "failed" || c.File != "res://test/calc_test.gd" || c.Line != 7 || !strings.Contains(c.Message, "Expecting:\n5\nbut was\n4") {
		t.Errorf("test_fails = %+v", c)
	}
	if c := byName["test_crashes"]; c.Status != "error" || c.Line != 11 || !strings.Contains(c.Message, "Nonexistent function 'boom'") {
		t.Errorf("test_crashes = %+v", c)
	}
	if c := byName["test_passes"]; c.Status != "passed" || c.Message != "" {
		t.Errorf("test_passes = %+v", c)
	}
}

func TestParseJUnitGUT(t *testing.T) {
	data, err := os.ReadFile("testdata/gut.xml")
	if err != nil {
		t.Fatal(err)
	}
	cases, totals, err := ParseJUnit(data)
	if err != nil {
		t.Fatal(err)
	}
	if totals != (TestTotals{Tests: 4, Passed: 1, Failed: 2, Skipped: 1}) {
		t.Errorf("totals = %+v", totals)
	}
	byName := index(cases)
	if c := byName["test_fails"]; c.Status != "failed" || c.File != "res://test/unit/test_calc.gd" || c.Line != 7 || !strings.Contains(c.Message, "math is hard") {
		t.Errorf("test_fails = %+v", c)
	}
	// GUT не знает строку ошибки выполнения ("at line -1") — её дополняют из вывода движка.
	if c := byName["test_crashes"]; c.Status != "failed" || c.Line != 0 || !strings.Contains(c.Message, "boom") {
		t.Errorf("test_crashes = %+v", c)
	}
	if c := byName["test_pending"]; c.Status != "skipped" || c.Message != "later" {
		t.Errorf("test_pending = %+v", c)
	}
}

func TestSuiteFile(t *testing.T) {
	for in, want := range map[string]string{
		"test/unit/test_calc.gd":       "res://test/unit/test_calc.gd",
		"test/unit/test_calc.gd.Inner": "res://test/unit/test_calc.gd",
		"calc_test":                    "",
	} {
		if got := suiteFile(in, ""); got != want {
			t.Errorf("suiteFile(%q) = %q, want %q", in, got, want)
		}
	}
}

func index(cases []TestCase) map[string]TestCase {
	m := map[string]TestCase{}
	for _, c := range cases {
		m[c.Name] = c
	}
	return m
}

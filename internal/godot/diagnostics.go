// Package godot управляет процессами движка: одноразовые CLI-вызовы
// (--check-only, --import, --script) и долгоживущие запуски проекта.
package godot

import (
	"regexp"
	"strconv"
	"strings"
)

// Diagnostic — ошибка или предупреждение из вывода Godot в структурированном виде,
// чтобы агенту не приходилось разбирать простыню текста.
//
// Формат Godot 4.x:
//
//	SCRIPT ERROR: Invalid call. Nonexistent function 'foo' in base 'Nil'.
//	          at: _ready (res://player.gd:6)
//	          GDScript backtrace (most recent call first):
//	              [0] _ready (res://player.gd:6)
type Diagnostic struct {
	Severity  string   `json:"severity"` // "error" | "warning"
	Kind      string   `json:"kind"`     // "script" | "engine"
	Message   string   `json:"message"`
	File      string   `json:"file,omitempty"` // res://... если ошибка в пользовательском коде
	Line      int      `json:"line,omitempty"`
	Function  string   `json:"function,omitempty"`
	Source    string   `json:"source,omitempty"` // место в C++-исходниках движка
	Backtrace []string `json:"backtrace,omitempty"`
}

var (
	ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
	// "at: _ready (res://player.gd:6)" / "at: load (core/io/resource_loader.cpp:317)"
	atRe = regexp.MustCompile(`^at:\s*(.*?)\s*\((.+):(\d+)\)\s*$`)
	// "_ready (res://main.gd:7)" — кадр GDScript backtrace
	frameLocRe = regexp.MustCompile(`^(.*?)\s*\((res://.+):(\d+)\)\s*$`)
	frameRe    = regexp.MustCompile(`^\[\d+\]\s+`)
)

type header struct{ prefix, severity, kind string }

var headers = []header{
	{"SCRIPT ERROR:", "error", "script"},
	{"USER SCRIPT ERROR:", "error", "script"},
	{"SCRIPT WARNING:", "warning", "script"},
	{"USER SCRIPT WARNING:", "warning", "script"},
	{"USER ERROR:", "error", "script"}, // push_error()
	{"USER WARNING:", "warning", "script"},
	{"ERROR:", "error", "engine"},
	{"WARNING:", "warning", "engine"},
}

// Шум, который Godot печатает при выходе из headless-скриптов.
var noise = []string{
	"was leaked at exit",
	"were leaked at exit",
	"RID of type",
	"ObjectDB instance",
}

// StripANSI убирает цветовые escape-последовательности.
func StripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

// ParseDiagnostics извлекает ошибки и предупреждения из строк вывода.
func ParseDiagnostics(lines []string) []Diagnostic {
	var out []Diagnostic
	var cur *Diagnostic
	inBacktrace := false

	emit := func() {
		if cur != nil && !isNoise(cur.Message) {
			locateInScript(cur)
			out = append(out, *cur)
		}
		cur, inBacktrace = nil, false
	}

	for _, raw := range lines {
		line := strings.TrimSpace(StripANSI(raw))
		if h, ok := matchHeader(line); ok {
			emit()
			cur = &Diagnostic{
				Severity: h.severity,
				Kind:     h.kind,
				Message:  strings.TrimSpace(strings.TrimPrefix(line, h.prefix)),
			}
			continue
		}
		if cur == nil {
			continue
		}
		switch {
		case strings.HasPrefix(line, "at:"):
			if m := atRe.FindStringSubmatch(line); m != nil {
				n, _ := strconv.Atoi(m[3])
				if isUserPath(m[2]) {
					cur.File, cur.Line, cur.Function = m[2], n, m[1]
					cur.Kind = "script"
				} else {
					cur.Source = m[2] + ":" + m[3]
					cur.Function = m[1]
				}
			}
		case strings.HasPrefix(line, "GDScript backtrace"):
			inBacktrace = true
		case inBacktrace && frameRe.MatchString(line):
			cur.Backtrace = append(cur.Backtrace, frameRe.ReplaceAllString(line, ""))
		default:
			emit() // обычная строка вывода закрывает диагностику
		}
	}
	emit()

	// "Failed to load script ... Parse error" дублирует SCRIPT ERROR выше.
	filtered := out[:0]
	for _, d := range out {
		if d.Kind == "engine" && strings.HasPrefix(d.Message, "Failed to load script") && hasScriptError(out) {
			continue
		}
		filtered = append(filtered, d)
	}
	return filtered
}

// locateInScript: push_error() и ошибки движка, вызванные из скрипта, указывают
// в "at:" на C++-исходник, а место в коде игры есть только в backtrace.
// Берём верхний кадр — оттуда ошибку и вызвали.
func locateInScript(d *Diagnostic) {
	if d.File != "" || len(d.Backtrace) == 0 {
		return
	}
	m := frameLocRe.FindStringSubmatch(d.Backtrace[0])
	if m == nil {
		return
	}
	n, _ := strconv.Atoi(m[3])
	d.File, d.Line, d.Function, d.Kind = m[2], n, m[1], "script"
}

func matchHeader(line string) (header, bool) {
	for _, h := range headers {
		if strings.HasPrefix(line, h.prefix) {
			return h, true
		}
	}
	return header{}, false
}

func isUserPath(p string) bool {
	return strings.HasPrefix(p, "res://") || strings.HasSuffix(p, ".gd") || strings.HasSuffix(p, ".cs")
}

func isNoise(msg string) bool {
	for _, n := range noise {
		if strings.Contains(msg, n) {
			return true
		}
	}
	return false
}

func hasScriptError(ds []Diagnostic) bool {
	for _, d := range ds {
		if d.Kind == "script" && d.Severity == "error" {
			return true
		}
	}
	return false
}

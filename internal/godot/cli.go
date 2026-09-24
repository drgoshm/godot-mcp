package godot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Godot — обёртка над бинарником движка для конкретного проекта.
type Godot struct {
	Bin        string
	ProjectDir string
}

// FindBinary ищет Godot: явный путь -> $GODOT_BIN -> PATH -> стандартные места.
func FindBinary(explicit string) (string, error) {
	if explicit != "" {
		return exec.LookPath(explicit)
	}
	if env := os.Getenv("GODOT_BIN"); env != "" {
		return exec.LookPath(env)
	}
	for _, name := range []string{"godot", "godot4", "Godot"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		candidates = []string{
			"/Applications/Godot.app/Contents/MacOS/Godot",
			filepath.Join(os.Getenv("HOME"), "Applications/Godot.app/Contents/MacOS/Godot"),
		}
	case "windows":
		candidates = []string{`C:\Program Files\Godot\Godot.exe`}
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", errors.New("godot binary not found: pass --godot or set GODOT_BIN")
}

// Result — итог одноразового вызова.
type Result struct {
	ExitCode    int          `json:"exit_code"`
	TimedOut    bool         `json:"timed_out,omitempty"`
	Duration    string       `json:"duration"`
	Output      string       `json:"output"` // stdout+stderr, без баннера и ANSI
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

// OK — процесс завершился нормально и не выдал ошибок.
func (r *Result) OK() bool {
	if r.ExitCode != 0 || r.TimedOut {
		return false
	}
	for _, d := range r.Diagnostics {
		if d.Severity == "error" {
			return false
		}
	}
	return true
}

const maxOutput = 64 * 1024

// Exec запускает Godot с --path проекта и ждёт завершения.
func (g *Godot) Exec(ctx context.Context, timeout time.Duration, args ...string) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	full := append([]string{"--path", g.ProjectDir}, args...)
	cmd := exec.CommandContext(ctx, g.Bin, full...)
	cmd.Dir = g.ProjectDir
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killGroup(cmd, true) } // убиваем всё дерево
	cmd.WaitDelay = 2 * time.Second

	start := time.Now()
	err := cmd.Run()
	res := &Result{Duration: time.Since(start).Round(time.Millisecond).String()}

	var exitErr *exec.ExitError
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		res.TimedOut, res.ExitCode = true, -1
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	case err != nil:
		return nil, fmt.Errorf("failed to start godot: %w", err)
	}

	lines := cleanLines(buf.String())
	res.Diagnostics = ParseDiagnostics(lines)
	res.Output = truncate(strings.Join(lines, "\n"), maxOutput)
	return res, nil
}

// Version возвращает строку вида "4.7.2.stable.official.ed1daf0bf".
func (g *Godot) Version(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, g.Bin, "--version").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(StripANSI(string(out))), nil
}

// CheckScript проверяет синтаксис и типы GDScript без запуска.
// Для корректной работы class_name проект должен быть импортирован хотя бы раз.
func (g *Godot) CheckScript(ctx context.Context, resPath string) (*Result, error) {
	return g.Exec(ctx, 60*time.Second, "--headless", "--check-only", "--script", resPath)
}

// Import (пере)импортирует ассеты и обновляет кеш классов в .godot/, затем выходит.
func (g *Godot) Import(ctx context.Context) (*Result, error) {
	return g.Exec(ctx, 10*time.Minute, "--headless", "--import")
}

// RunScript выполняет GDScript, наследующий SceneTree или MainLoop, в headless-режиме.
// userArgs передаются после "--" и доступны через OS.get_cmdline_user_args().
func (g *Godot) RunScript(ctx context.Context, scriptPath string, timeout time.Duration, userArgs ...string) (*Result, error) {
	args := []string{"--headless", "--script", scriptPath}
	if len(userArgs) > 0 {
		args = append(append(args, "--"), userArgs...)
	}
	return g.Exec(ctx, timeout, args...)
}

// RunSource записывает GDScript во временный файл вне проекта и выполняет его.
// Godot умеет загружать --script по абсолютному пути, поэтому проект не засоряется.
func (g *Godot) RunSource(ctx context.Context, source string, timeout time.Duration, userArgs ...string) (*Result, error) {
	f, err := os.CreateTemp("", "godot-mcp-*.gd")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(source); err != nil {
		f.Close()
		return nil, err
	}
	f.Close()
	return g.RunScript(ctx, f.Name(), timeout, userArgs...)
}

var bannerRe = regexp.MustCompile(`^Godot Engine v\S+ - https://godotengine\.org$`)

// cleanLines убирает ANSI, баннер движка и пустые строки в начале.
func cleanLines(s string) []string {
	raw := strings.Split(strings.ReplaceAll(StripANSI(s), "\r\n", "\n"), "\n")
	out := make([]string, 0, len(raw))
	for _, l := range raw {
		if bannerRe.MatchString(strings.TrimSpace(l)) {
			continue
		}
		if len(out) == 0 && strings.TrimSpace(l) == "" {
			continue
		}
		out = append(out, l)
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n/2] + "\n... [output truncated] ...\n" + s[len(s)-n/2:]
}

// StartGroup и KillGroup — для долгоживущих процессов движка вне Runner
// (фоновый редактор для LSP): своя группа процессов и остановка всего дерева.
func StartGroup(cmd *exec.Cmd) error {
	setProcessGroup(cmd)
	return cmd.Start()
}

// KillGroup шлёт SIGTERM (или SIGKILL при force) группе процессов cmd.
func KillGroup(cmd *exec.Cmd, force bool) error { return killGroup(cmd, force) }

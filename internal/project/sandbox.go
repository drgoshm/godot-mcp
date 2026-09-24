// Package project отвечает за всё, что касается файлов Godot-проекта:
// песочницу путей res://, чтение/запись файлов и разбор project.godot.
package project

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Каталоги, в которые агенту нельзя лезть через файловые инструменты.
// .godot — кеш импорта и служебные данные редактора: правка руками
// ломает проект, а движок всё равно их перегенерирует.
var forbiddenDirs = []string{".godot", ".git"}

// ErrOutsideProject возвращается, когда путь выходит за корень проекта.
var ErrOutsideProject = errors.New("path is outside the project root")

// Sandbox сопоставляет пути res:// с путями файловой системы
// и гарантирует, что агент не выйдет за пределы проекта.
type Sandbox struct {
	root     string // абсолютный путь к корню (каталог с project.godot)
	realRoot string // тот же путь после раскрытия симлинков
}

// NewSandbox проверяет, что root — Godot-проект, и создаёт песочницу.
func NewSandbox(root string) (*Sandbox, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(abs, "project.godot")); err != nil {
		return nil, fmt.Errorf("%s is not a Godot project (no project.godot): %w", abs, err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	return &Sandbox{root: abs, realRoot: real}, nil
}

// Root возвращает абсолютный путь к корню проекта.
func (s *Sandbox) Root() string { return s.root }

// Resolve превращает "res://scenes/a.tscn" или "scenes/a.tscn" в абсолютный
// путь внутри проекта. Абсолютные пути ОС, выход через "..", служебные
// каталоги и симлинки наружу запрещены.
func (s *Sandbox) Resolve(p string) (string, error) {
	rel, err := normalize(p)
	if err != nil {
		return "", err
	}
	if rel != "." {
		first := strings.SplitN(rel, "/", 2)[0]
		for _, d := range forbiddenDirs {
			if first == d {
				return "", fmt.Errorf("access to %q is not allowed: it is managed by Godot/VCS", d+"/")
			}
		}
	}
	abs := filepath.Join(s.root, filepath.FromSlash(rel))
	if err := s.checkSymlinks(abs); err != nil {
		return "", err
	}
	return abs, nil
}

// ToRes превращает абсолютный путь внутри проекта в res://-путь.
func (s *Sandbox) ToRes(abs string) string {
	rel, err := filepath.Rel(s.root, abs)
	if err != nil || rel == "." {
		return "res://"
	}
	return "res://" + filepath.ToSlash(rel)
}

// normalize приводит путь к виду "a/b/c" относительно корня проекта.
func normalize(p string) (string, error) {
	p = strings.TrimSpace(p)
	switch {
	case p == "" || p == "res://":
		return ".", nil
	case strings.HasPrefix(p, "res://"):
		p = strings.TrimPrefix(p, "res://")
	case strings.HasPrefix(p, "user://"), strings.HasPrefix(p, "uid://"):
		return "", fmt.Errorf("%q: only res:// paths are supported here", p)
	case filepath.IsAbs(p) || strings.HasPrefix(p, "/"):
		return "", fmt.Errorf("%q: use res:// paths, not absolute OS paths", p)
	}
	p = strings.ReplaceAll(p, `\`, "/")
	clean := filepath.ToSlash(filepath.Clean(p))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%q: %w", p, ErrOutsideProject)
	}
	return clean, nil
}

// checkSymlinks раскрывает симлинки у самого глубокого существующего предка
// пути и проверяет, что он остаётся внутри проекта.
func (s *Sandbox) checkSymlinks(abs string) error {
	probe := abs
	for {
		if _, err := os.Lstat(probe); err == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return nil
		}
		probe = parent
	}
	real, err := filepath.EvalSymlinks(probe)
	if err != nil {
		return err
	}
	if real != s.realRoot && !strings.HasPrefix(real, s.realRoot+string(filepath.Separator)) {
		return fmt.Errorf("%s resolves outside the project via a symlink: %w", abs, ErrOutsideProject)
	}
	return nil
}

package project

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Config — сырой project.godot: секция -> ключ -> значение в синтаксисе Godot.
type Config map[string]map[string]string

// ParseConfig разбирает project.godot (ConfigFile-формат Godot).
// Значения остаются строками в синтаксисе Godot (например
// `PackedStringArray("4.3", "Forward Plus")`); многострочные значения
// вроде действий ввода склеиваются по балансу скобок.
func ParseConfig(data string) Config {
	cfg := Config{"": {}}
	section := ""
	var key string
	var val strings.Builder
	depth := 0

	flush := func() {
		if key != "" {
			cfg[section][key] = strings.TrimSpace(val.String())
		}
		key = ""
		val.Reset()
	}

	sc := bufio.NewScanner(strings.NewReader(data))
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if depth > 0 { // продолжение многострочного значения
			val.WriteString("\n" + line)
			depth += bracketDelta(line)
			if depth <= 0 {
				flush()
			}
			continue
		}
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "" || strings.HasPrefix(trimmed, ";"):
			continue
		case strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]"):
			section = trimmed[1 : len(trimmed)-1]
			if cfg[section] == nil {
				cfg[section] = map[string]string{}
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(k)
		val.WriteString(v)
		depth = bracketDelta(v)
		if depth <= 0 {
			flush()
		}
	}
	flush()
	return cfg
}

// bracketDelta считает баланс {[( вне строковых литералов.
func bracketDelta(s string) int {
	d, inStr, esc := 0, false, false
	for _, r := range s {
		switch {
		case esc:
			esc = false
		case r == '\\' && inStr:
			esc = true
		case r == '"':
			inStr = !inStr
		case inStr:
		case r == '{' || r == '[' || r == '(':
			d++
		case r == '}' || r == ']' || r == ')':
			d--
		}
	}
	return d
}

var quotedRe = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)

// Unquote снимает кавычки со строкового значения Godot.
func Unquote(v string) string {
	if m := quotedRe.FindStringSubmatch(v); m != nil {
		return strings.ReplaceAll(m[1], `\"`, `"`)
	}
	return v
}

// Info — сводка проекта для агента.
type Info struct {
	Name          string            `json:"name"`
	MainScene     string            `json:"main_scene,omitempty"`
	MainSceneFile string            `json:"main_scene_file,omitempty"`
	Features      []string          `json:"features,omitempty"`
	Autoloads     map[string]string `json:"autoloads,omitempty"`
	InputActions  []string          `json:"input_actions,omitempty"`
	Imported      bool              `json:"imported"`
}

// LoadInfo читает project.godot и собирает сводку.
func (s *Sandbox) LoadInfo() (*Info, error) {
	data, err := os.ReadFile(filepath.Join(s.root, "project.godot"))
	if err != nil {
		return nil, err
	}
	cfg := ParseConfig(string(data))
	app := cfg["application"]
	info := &Info{
		Name:      Unquote(app["config/name"]),
		MainScene: Unquote(app["run/main_scene"]),
	}
	if strings.HasPrefix(info.MainScene, "uid://") {
		if p, err := s.ResolveUID(info.MainScene); err == nil {
			info.MainSceneFile = p
		}
	}
	for _, m := range quotedRe.FindAllStringSubmatch(app["config/features"], -1) {
		info.Features = append(info.Features, m[1])
	}
	if al := cfg["autoload"]; len(al) > 0 {
		info.Autoloads = map[string]string{}
		for name, v := range al {
			// "*res://x.gd": звёздочка означает, что синглтон включён.
			info.Autoloads[name] = Unquote(v)
		}
	}
	for action := range cfg["input"] {
		info.InputActions = append(info.InputActions, action)
	}
	sort.Strings(info.InputActions)
	_, err = os.Stat(filepath.Join(s.root, ".godot", "global_script_class_cache.cfg"))
	info.Imported = err == nil
	return info, nil
}

var headerUIDRe = regexp.MustCompile(`^\[gd_(?:scene|resource)[^\]]*\buid="(uid://[^"]+)"`)

// ResolveUID находит res://-путь по uid://. Бинарный .godot/uid_cache.bin
// мы не парсим: достаточно заголовков .tscn/.tres и файлов *.uid
// (в них лежат UID скриптов и шейдеров, начиная с Godot 4.4).
func (s *Sandbox) ResolveUID(uid string) (string, error) {
	var found string
	errFound := errors.New("found")
	err := filepath.WalkDir(s.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if n := d.Name(); n == ".godot" || n == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		switch filepath.Ext(p) {
		case ".uid":
			if b, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(b)) == uid {
				found = s.ToRes(strings.TrimSuffix(p, ".uid"))
				return errFound
			}
		case ".tscn", ".tres":
			if line, err := firstLine(p); err == nil {
				if m := headerUIDRe.FindStringSubmatch(line); m != nil && m[1] == uid {
					found = s.ToRes(p)
					return errFound
				}
			}
		}
		return nil
	})
	if found != "" {
		return found, nil
	}
	if err != nil && !errors.Is(err, errFound) {
		return "", err
	}
	return "", errors.New(uid + " not found in project")
}

func firstLine(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 512)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return line, nil
}

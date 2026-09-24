// Package docs даёт справку по API установленной версии Godot: классы,
// встроенные типы, глобальные функции — с описаниями, сигнатурами и
// значениями по умолчанию. Данные берутся у самого движка, поэтому всегда
// совпадают с его версией, а не с тем, что модель помнит о Godot 3.
package docs

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Class — класс движка, встроенный тип (Vector2, Array...) или @GlobalScope/@GDScript.
type Class struct {
	Name        string
	Inherits    string
	Kind        string // class | builtin | global
	Singleton   bool
	Brief       string
	Description string
	Methods     []Method
	Properties  []Property
	Signals     []Signal
	Constants   []Constant // без констант перечислений — они в Enums
	Enums       []Enum
}

type Method struct {
	Name        string
	Return      string
	Args        []Arg
	Const       bool
	Static      bool
	Virtual     bool
	Vararg      bool
	Constructor bool
	Description string
}

type Arg struct{ Name, Type, Default string }

type Property struct {
	Name, Type, Default, Enum, Description string
	Setter, Getter                         string
}

type Signal struct {
	Name        string
	Args        []Arg
	Description string
}

type Constant struct{ Name, Value, Description string }

type Enum struct {
	Name     string
	Bitfield bool
	Values   []Constant
}

// Index — вся справка одной версии движка.
type Index struct {
	Version string
	classes map[string]*Class // ключ — имя в нижнем регистре
	names   []string          // имена классов по алфавиту
}

// Class ищет класс без учёта регистра.
func (ix *Index) Class(name string) *Class { return ix.classes[strings.ToLower(name)] }

// Chain — класс и его предки, начиная с него самого.
func (ix *Index) Chain(c *Class) []*Class {
	chain := []*Class{c}
	for seen := map[string]bool{c.Name: true}; c.Inherits != ""; {
		c = ix.Class(c.Inherits)
		if c == nil || seen[c.Name] {
			break
		}
		seen[c.Name] = true
		chain = append(chain, c)
	}
	return chain
}

// ---- загрузка ----

// Loader лениво строит индекс: при первом обращении дампит API движка
// в кеш пользователя (раз на версию), потом держит индекс в памяти.
type Loader struct {
	Bin      string // бинарник Godot
	Version  string // "4.6.3.stable.official.7d41c59c4"
	CacheDir string // по умолчанию os.UserCacheDir()/godot-mcp/api

	mu  sync.Mutex
	idx *Index
}

// Index возвращает индекс, при необходимости создавая кеш.
func (l *Loader) Index(ctx context.Context) (*Index, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.idx != nil {
		return l.idx, nil
	}
	dir, err := l.cacheDir()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(dir, apiFile)); err != nil {
		if err := l.generate(ctx, dir); err != nil {
			return nil, err
		}
	}
	idx, err := loadIndex(dir, l.Version)
	if err != nil {
		return nil, err
	}
	l.idx = idx
	return idx, nil
}

const (
	apiFile   = "extension_api.json"
	extraFile = "extra.json" // значения по умолчанию и @GDScript из --doctool
)

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func (l *Loader) cacheDir() (string, error) {
	base := l.CacheDir
	if base == "" {
		c, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(c, "godot-mcp", "api")
	}
	return filepath.Join(base, unsafeChars.ReplaceAllString(l.Version, "_")), nil
}

// generate дампит API двумя вызовами движка:
//   - --dump-extension-api-with-docs: всё API с описаниями (JSON, ~0.7 с);
//   - --doctool: XML без описаний, но со значениями свойств по умолчанию,
//     их enum-типами и функциями @GDScript, которых нет в JSON (~2 с).
//
// Пишем во временный каталог и переименовываем, чтобы параллельный
// процесс не прочитал половину кеша.
func (l *Loader) generate(ctx context.Context, dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".gen-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	// Дамп пишется в текущий каталог, поэтому запускаем из tmp и без проекта.
	if err := l.run(ctx, tmp, "--headless", "--dump-extension-api-with-docs"); err != nil {
		return fmt.Errorf("dumping the engine API: %w", err)
	}
	// --doctool сам каталог не создаёт.
	xmlDir := filepath.Join(tmp, "xml")
	if err := os.Mkdir(xmlDir, 0o755); err != nil {
		return err
	}
	if err := l.run(ctx, tmp, "--headless", "--doctool", xmlDir); err != nil {
		return fmt.Errorf("running --doctool: %w", err)
	}
	extra, err := parseDoctool(xmlDir)
	if err != nil {
		return err
	}
	data, err := json.Marshal(extra)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, extraFile), data, 0o644); err != nil {
		return err
	}
	if err := os.RemoveAll(xmlDir); err != nil {
		return err
	}
	if err := os.Rename(tmp, dir); err != nil && !exists(filepath.Join(dir, apiFile)) {
		return err
	}
	return nil
}

func (l *Loader) run(ctx context.Context, dir string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, l.Bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w\n%s", err, tail(string(out), 2000))
	}
	return nil
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// ---- --doctool XML ----

// extra — то, что есть только в XML: значения свойств по умолчанию и их
// enum-типы, плюс класс @GDScript целиком (без описаний).
type extra struct {
	Members  map[string]map[string]memberExtra `json:"members"` // класс -> свойство
	GDScript *Class                            `json:"gdscript,omitempty"`
}

type memberExtra struct {
	Default string `json:"default,omitempty"`
	Enum    string `json:"enum,omitempty"`
}

type xmlClass struct {
	Name     string `xml:"name,attr"`
	Inherits string `xml:"inherits,attr"`
	Members  []struct {
		Name    string `xml:"name,attr"`
		Default string `xml:"default,attr"`
		Enum    string `xml:"enum,attr"`
	} `xml:"members>member"`
	Methods     []xmlMethod `xml:"methods>method"`
	Annotations []xmlMethod `xml:"annotations>annotation"`
	Constants   []struct {
		Name  string `xml:"name,attr"`
		Value string `xml:"value,attr"`
	} `xml:"constants>constant"`
}

type xmlMethod struct {
	Name       string `xml:"name,attr"`
	Qualifiers string `xml:"qualifiers,attr"`
	Return     struct {
		Type string `xml:"type,attr"`
	} `xml:"return"`
	Params []struct {
		Name    string `xml:"name,attr"`
		Type    string `xml:"type,attr"`
		Default string `xml:"default,attr"`
	} `xml:"param"`
}

func parseDoctool(dir string) (*extra, error) {
	ex := &extra{Members: map[string]map[string]memberExtra{}}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".xml" {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var c xmlClass
		if err := xml.Unmarshal(data, &c); err != nil {
			return nil // не класс (например, class.xsd рядом) — пропускаем
		}
		if c.Name == "@GDScript" {
			ex.GDScript = gdscriptClass(c)
			return nil
		}
		for _, m := range c.Members {
			if m.Default == "" && m.Enum == "" {
				continue
			}
			if ex.Members[c.Name] == nil {
				ex.Members[c.Name] = map[string]memberExtra{}
			}
			ex.Members[c.Name][m.Name] = memberExtra{Default: m.Default, Enum: m.Enum}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(ex.Members) == 0 {
		return nil, errors.New("--doctool produced no class reference")
	}
	return ex, nil
}

func gdscriptClass(c xmlClass) *Class {
	out := &Class{Name: "@GDScript", Kind: "global",
		Brief: "Built-in GDScript functions (preload, range, len, ...) and annotations (@export, @onready, ...)."}
	add := func(ms []xmlMethod, prefix string) {
		for _, m := range ms {
			mm := Method{Name: prefix + strings.TrimPrefix(m.Name, "@"), Return: m.Return.Type, Vararg: strings.Contains(m.Qualifiers, "vararg")}
			for _, p := range m.Params {
				mm.Args = append(mm.Args, Arg{Name: p.Name, Type: p.Type, Default: p.Default})
			}
			out.Methods = append(out.Methods, mm)
		}
	}
	add(c.Methods, "")
	add(c.Annotations, "@")
	for _, k := range c.Constants {
		out.Constants = append(out.Constants, Constant{Name: k.Name, Value: k.Value})
	}
	return out
}

// ---- extension_api.json ----

type apiArg struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Default string `json:"default_value"`
}

type apiMethod struct {
	Name        string   `json:"name"`
	IsConst     bool     `json:"is_const"`
	IsStatic    bool     `json:"is_static"`
	IsVirtual   bool     `json:"is_virtual"`
	IsVararg    bool     `json:"is_vararg"`
	ReturnValue *apiArg  `json:"return_value"` // классы
	ReturnType  string   `json:"return_type"`  // встроенные типы и глобальные функции
	Arguments   []apiArg `json:"arguments"`
	Description string   `json:"description"`
}

type apiConstant struct {
	Name        string          `json:"name"`
	Value       json.RawMessage `json:"value"` // у классов число, у встроенных типов строка
	Description string          `json:"description"`
}

type apiEnum struct {
	Name       string        `json:"name"`
	IsBitfield bool          `json:"is_bitfield"`
	Values     []apiConstant `json:"values"`
}

type apiClass struct {
	Name        string        `json:"name"`
	Inherits    string        `json:"inherits"`
	Brief       string        `json:"brief_description"`
	Description string        `json:"description"`
	Methods     []apiMethod   `json:"methods"`
	Constants   []apiConstant `json:"constants"`
	Enums       []apiEnum     `json:"enums"`
	Signals     []struct {
		Name        string   `json:"name"`
		Arguments   []apiArg `json:"arguments"`
		Description string   `json:"description"`
	} `json:"signals"`
	Properties []struct {
		Name        string `json:"name"`
		Type        string `json:"type"`
		Setter      string `json:"setter"`
		Getter      string `json:"getter"`
		Description string `json:"description"`
	} `json:"properties"`
	// встроенные типы
	Members []struct {
		Name        string `json:"name"`
		Type        string `json:"type"`
		Description string `json:"description"`
	} `json:"members"`
	Constructors []struct {
		Arguments   []apiArg `json:"arguments"`
		Description string   `json:"description"`
	} `json:"constructors"`
}

type apiDoc struct {
	Header struct {
		FullName string `json:"version_full_name"`
	} `json:"header"`
	GlobalConstants  []apiConstant `json:"global_constants"`
	GlobalEnums      []apiEnum     `json:"global_enums"`
	UtilityFunctions []apiMethod   `json:"utility_functions"`
	BuiltinClasses   []apiClass    `json:"builtin_classes"`
	Classes          []apiClass    `json:"classes"`
	Singletons       []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"singletons"`
}

func loadIndex(dir, version string) (*Index, error) {
	raw, err := os.ReadFile(filepath.Join(dir, apiFile))
	if err != nil {
		return nil, err
	}
	var doc apiDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("reading %s: %w", apiFile, err)
	}
	var ex extra
	if data, err := os.ReadFile(filepath.Join(dir, extraFile)); err == nil {
		_ = json.Unmarshal(data, &ex)
	}
	return buildIndex(&doc, &ex, version), nil
}

func buildIndex(doc *apiDoc, ex *extra, version string) *Index {
	ix := &Index{Version: version, classes: map[string]*Class{}}
	if doc.Header.FullName != "" {
		ix.Version = strings.TrimPrefix(doc.Header.FullName, "Godot Engine v")
	}
	add := func(c *Class) {
		ix.classes[strings.ToLower(c.Name)] = c
		ix.names = append(ix.names, c.Name)
	}

	global := &Class{Name: "@GlobalScope", Kind: "global",
		Brief: "Global functions (print, lerp, randf...), constants and enums available everywhere."}
	for _, f := range doc.UtilityFunctions {
		global.Methods = append(global.Methods, convMethod(f))
	}
	for _, k := range doc.GlobalConstants {
		global.Constants = append(global.Constants, convConst(k))
	}
	for _, e := range doc.GlobalEnums {
		global.Enums = append(global.Enums, convEnum(e))
	}
	add(global)
	if ex.GDScript != nil {
		add(ex.GDScript)
	}

	for i := range doc.BuiltinClasses {
		b := &doc.BuiltinClasses[i]
		c := convClass(b, "builtin")
		for _, k := range b.Constructors {
			m := Method{Name: b.Name, Return: b.Name, Constructor: true, Description: k.Description}
			for _, a := range k.Arguments {
				m.Args = append(m.Args, Arg{Name: a.Name, Type: a.Type})
			}
			c.Methods = append([]Method{m}, c.Methods...)
		}
		for _, m := range b.Members {
			c.Properties = append(c.Properties, Property{Name: m.Name, Type: m.Type, Description: m.Description})
		}
		add(c)
	}

	singletons := map[string]bool{}
	for _, s := range doc.Singletons {
		singletons[s.Type] = true
	}
	for i := range doc.Classes {
		a := &doc.Classes[i]
		c := convClass(a, "class")
		c.Singleton = singletons[a.Name]
		for _, p := range a.Properties {
			prop := Property{Name: p.Name, Type: propType(p.Type), Description: p.Description, Setter: p.Setter, Getter: p.Getter}
			if m, ok := ex.Members[a.Name][p.Name]; ok {
				prop.Default, prop.Enum = m.Default, m.Enum
			}
			c.Properties = append(c.Properties, prop)
		}
		for _, s := range a.Signals {
			sig := Signal{Name: s.Name, Description: s.Description}
			for _, arg := range s.Arguments {
				sig.Args = append(sig.Args, Arg{Name: arg.Name, Type: arg.Type})
			}
			c.Signals = append(c.Signals, sig)
		}
		add(c)
	}
	sort.Strings(ix.names)
	return ix
}

func convClass(a *apiClass, kind string) *Class {
	c := &Class{Name: a.Name, Inherits: a.Inherits, Kind: kind, Brief: a.Brief, Description: a.Description}
	for _, m := range a.Methods {
		c.Methods = append(c.Methods, convMethod(m))
	}
	for _, k := range a.Constants {
		c.Constants = append(c.Constants, convConst(k))
	}
	for _, e := range a.Enums {
		c.Enums = append(c.Enums, convEnum(e))
	}
	return c
}

func convMethod(m apiMethod) Method {
	out := Method{Name: m.Name, Return: m.ReturnType, Const: m.IsConst, Static: m.IsStatic,
		Virtual: m.IsVirtual, Vararg: m.IsVararg, Description: m.Description}
	if m.ReturnValue != nil {
		out.Return = m.ReturnValue.Type
	}
	if out.Return == "" {
		out.Return = "void"
	}
	for _, a := range m.Arguments {
		out.Args = append(out.Args, Arg{Name: a.Name, Type: a.Type, Default: a.Default})
	}
	return out
}

func convConst(k apiConstant) Constant {
	v := strings.Trim(string(k.Value), `"`)
	return Constant{Name: k.Name, Value: v, Description: k.Description}
}

func convEnum(e apiEnum) Enum {
	out := Enum{Name: e.Name, Bitfield: e.IsBitfield}
	for _, v := range e.Values {
		out.Values = append(out.Values, convConst(v))
	}
	return out
}

// propType: "Texture2D,-AnimatedTexture,-AtlasTexture" -> "Texture2D";
// "BaseMaterial3D,ShaderMaterial" -> "BaseMaterial3D | ShaderMaterial".
func propType(t string) string {
	var keep []string
	for _, p := range strings.Split(t, ",") {
		if p != "" && !strings.HasPrefix(p, "-") {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, " | ")
}

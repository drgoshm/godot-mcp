package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Навигация по коду через языковой сервер фонового редактора (см. internal/lsp).

type SymbolInfoIn struct {
	Path       string `json:"path" jsonschema:"res:// path of the .gd file where the symbol is used or declared"`
	Symbol     string `json:"symbol,omitempty" jsonschema:"identifier to look up; without line, its first occurrence in the file is used"`
	Line       int    `json:"line,omitempty" jsonschema:"1-based line; the symbol is searched on this line"`
	Column     int    `json:"column,omitempty" jsonschema:"1-based column, instead of symbol"`
	References bool   `json:"references,omitempty" jsonschema:"also list every usage across the project"`
}

type CodeLocation struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text,omitempty"`
}

type SymbolInfoOut struct {
	At                  string         `json:"at"`
	Hover               string         `json:"hover,omitempty"`
	Definitions         []CodeLocation `json:"definitions,omitempty"`
	References          []CodeLocation `json:"references,omitempty"`
	ReferencesTruncated bool           `json:"references_truncated,omitempty"`
	Hint                string         `json:"hint,omitempty"`
}

type FindSymbolIn struct {
	Query         string `json:"query,omitempty" jsonschema:"find functions, variables, signals, constants, classes whose name contains this text, across all project scripts"`
	Path          string `json:"path,omitempty" jsonschema:"res:// .gd file: without query, return its outline; with query, search only in it"`
	IncludeAddons bool   `json:"include_addons,omitempty" jsonschema:"also search scripts under addons/"`
}

type SymbolEntry struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Container string `json:"container,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Doc       string `json:"doc,omitempty"`
}

type FindSymbolOut struct {
	Symbols   []SymbolEntry `json:"symbols"`
	Files     int           `json:"files"`
	Truncated bool          `json:"truncated,omitempty"`
}

const (
	maxReferences = 200
	maxSymbols    = 150
)

func registerCodeTools(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "godot_symbol_info",
		Description: "What a symbol in a GDScript file is and where it comes from: signature and doc comment (hover), " +
			"where it is defined, and optionally every usage across the project. Give path + symbol (first occurrence) " +
			"or path + line + symbol. Engine members have no definition in the project; use godot_class_docs for them.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SymbolInfoIn) (*mcp.CallToolResult, SymbolInfoOut, error) {
		out, err := symbolInfo(ctx, d, in)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "godot_find_symbol",
		Description: "Find where functions, variables, signals, constants and classes are declared in the project's scripts " +
			"by part of the name, or get the outline of one script (path without query).",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in FindSymbolIn) (*mcp.CallToolResult, FindSymbolOut, error) {
		out, err := findSymbol(ctx, d, in)
		return nil, out, err
	})
}

var errNoLSP = errors.New("code navigation needs the background editor, but the server runs with --lsp=off")

func symbolInfo(ctx context.Context, d *Deps, in SymbolInfoIn) (SymbolInfoOut, error) {
	var out SymbolInfoOut
	if d.LSP == nil {
		return out, errNoLSP
	}
	res, err := resolveScript(d, in.Path)
	if err != nil {
		return out, err
	}
	abs, _ := d.Sandbox.Resolve(res)
	text, err := os.ReadFile(abs)
	if err != nil {
		return out, err
	}
	line, char, err := locate(string(text), in.Symbol, in.Line, in.Column)
	if err != nil {
		return out, err
	}
	out.At = fmt.Sprintf("%s:%d:%d", res, line+1, char+1)
	pos := map[string]any{"position": map[string]any{"line": line, "character": char}}

	raw, err := d.LSP.Query(ctx, res, "textDocument/hover", pos)
	if err != nil {
		return out, err
	}
	out.Hover = hoverText(raw)
	raw, err = d.LSP.Query(ctx, res, "textDocument/definition", pos)
	if err != nil {
		return out, err
	}
	out.Definitions = d.locations(raw)
	if len(out.Definitions) == 0 {
		out.Hint = "no definition in the project: an engine member (see godot_class_docs) or a call the analyzer cannot resolve (untyped variable)"
	}

	if in.References {
		pos["context"] = map[string]any{"includeDeclaration": false}
		raw, err := d.LSP.Query(ctx, res, "textDocument/references", pos)
		if err != nil {
			return out, err
		}
		decl := map[CodeLocation]bool{}
		for _, l := range out.Definitions {
			decl[CodeLocation{Path: l.Path, Line: l.Line}] = true
		}
		// Godot возвращает и само объявление, даже с includeDeclaration: false.
		for _, l := range d.locations(raw) {
			if !decl[CodeLocation{Path: l.Path, Line: l.Line}] {
				out.References = append(out.References, l)
			}
		}
		if len(out.References) > maxReferences {
			out.References, out.ReferencesTruncated = out.References[:maxReferences], true
		}
	}
	return out, nil
}

// locate переводит symbol/line/column в позицию LSP (с нуля). Колонка — номер
// символа (не байта): Godot считает позиции в символах строки.
func locate(text, symbol string, line, column int) (int, int, error) {
	lines := strings.Split(text, "\n")
	if line > 0 {
		if line > len(lines) {
			return 0, 0, fmt.Errorf("line %d is past the end of the file (%d lines)", line, len(lines))
		}
		if column > 0 {
			return line - 1, column - 1, nil
		}
		if symbol == "" {
			return 0, 0, errors.New("pass symbol or column together with line")
		}
		if c, ok := findWord(lines[line-1], symbol); ok {
			return line - 1, c, nil
		}
		return 0, 0, fmt.Errorf("%q is not on line %d: %s", symbol, line, strings.TrimSpace(lines[line-1]))
	}
	if symbol == "" {
		return 0, 0, errors.New("pass symbol (optionally with line) or line + column")
	}
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		if c, ok := findWord(l, symbol); ok {
			return i, c, nil
		}
	}
	return 0, 0, fmt.Errorf("%q does not occur in this file", symbol)
}

func findWord(line, word string) (int, bool) {
	re := regexp.MustCompile(`(^|[^A-Za-z0-9_])(` + regexp.QuoteMeta(word) + `)([^A-Za-z0-9_]|$)`)
	m := re.FindStringSubmatchIndex(line)
	if m == nil {
		return 0, false
	}
	return utf8.RuneCountInString(line[:m[4]]), true
}

var definedInRe = regexp.MustCompile(`Defined in \[(res://[^\]]+)\]\([^)]*\)`)

func hoverText(raw json.RawMessage) string {
	var h struct {
		Contents json.RawMessage `json:"contents"`
	}
	if json.Unmarshal(raw, &h) != nil || len(h.Contents) == 0 {
		return ""
	}
	var mc struct {
		Value string `json:"value"`
	}
	var text string
	if json.Unmarshal(h.Contents, &mc) == nil && mc.Value != "" {
		text = mc.Value
	} else {
		_ = json.Unmarshal(h.Contents, &text)
	}
	// Абсолютный file://-путь агенту ни к чему — оставляем res://.
	text = definedInRe.ReplaceAllString(text, "Defined in $1")
	return strings.TrimSpace(text)
}

type lspLocation struct {
	URI   string `json:"uri"`
	Range struct {
		Start struct {
			Line      int `json:"line"`
			Character int `json:"character"`
		} `json:"start"`
	} `json:"range"`
}

// locations разбирает Location | Location[] и добавляет текст строки.
func (d *Deps) locations(raw json.RawMessage) []CodeLocation {
	var many []lspLocation
	if json.Unmarshal(raw, &many) != nil {
		var one lspLocation
		if json.Unmarshal(raw, &one) != nil || one.URI == "" {
			return nil
		}
		many = []lspLocation{one}
	}
	lines := map[string][]string{}
	var out []CodeLocation
	for _, l := range many {
		res := d.LSP.ResPath(l.URI)
		if res == "" {
			continue
		}
		if _, ok := lines[res]; !ok {
			if abs, err := d.Sandbox.Resolve(res); err == nil {
				if data, err := os.ReadFile(abs); err == nil {
					lines[res] = strings.Split(string(data), "\n")
				}
			}
		}
		loc := CodeLocation{Path: res, Line: l.Range.Start.Line + 1}
		if src := lines[res]; l.Range.Start.Line < len(src) {
			line := src[l.Range.Start.Line]
			// Godot находит имя и в комментариях ("## Doubles the power.") — это не использование.
			if inComment(line, l.Range.Start.Character) {
				continue
			}
			loc.Text = strings.TrimSpace(line)
		}
		out = append(out, loc)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Line < out[j].Line
	})
	return out
}

// ---- поиск символов ----

type lspSymbol struct {
	Name          string      `json:"name"`
	Kind          int         `json:"kind"`
	Detail        string      `json:"detail"`
	Documentation string      `json:"documentation"`
	Children      []lspSymbol `json:"children"`
	Selection     struct {
		Start struct {
			Line int `json:"line"`
		} `json:"start"`
	} `json:"selectionRange"`
}

// symbolKinds — SymbolKind из спецификации LSP, которые встречаются в GDScript.
var symbolKinds = map[int]string{
	5: "class", 6: "method", 7: "property", 8: "field", 10: "enum", 12: "function",
	13: "variable", 14: "constant", 22: "enum member", 23: "struct", 24: "signal",
}

func findSymbol(ctx context.Context, d *Deps, in FindSymbolIn) (FindSymbolOut, error) {
	out := FindSymbolOut{Symbols: []SymbolEntry{}}
	if d.LSP == nil {
		return out, errNoLSP
	}
	var paths []string
	switch {
	case in.Path != "":
		res, err := resolveScript(d, in.Path)
		if err != nil {
			return out, err
		}
		paths = []string{res}
	case in.Query != "":
		paths = projectScripts(d, in.IncludeAddons)
	default:
		return out, errors.New("pass query (search the project) or path (outline of a script)")
	}
	out.Files = len(paths)
	q := strings.ToLower(in.Query)

	type scored struct {
		e     SymbolEntry
		score int
	}
	var found []scored
	err := d.LSP.QueryEach(ctx, paths, "textDocument/documentSymbol", nil, func(res string, raw json.RawMessage) {
		var syms []lspSymbol
		if json.Unmarshal(raw, &syms) != nil {
			return
		}
		var walk func(ss []lspSymbol, container string)
		walk = func(ss []lspSymbol, container string) {
			for _, s := range ss {
				e := SymbolEntry{Path: res, Line: s.Selection.Start.Line + 1, Kind: symbolKinds[s.Kind],
					Name: s.Name, Container: container, Detail: s.Detail, Doc: firstLine(s.Documentation)}
				if e.Kind == "" {
					e.Kind = fmt.Sprintf("kind %d", s.Kind)
				}
				name := strings.ToLower(s.Name)
				switch {
				case q == "":
					found = append(found, scored{e, 0})
				case name == q:
					found = append(found, scored{e, 3})
				case strings.HasPrefix(name, q):
					found = append(found, scored{e, 2})
				case strings.Contains(name, q):
					found = append(found, scored{e, 1})
				}
				walk(s.Children, s.Name)
			}
		}
		walk(syms, "")
	})
	if err != nil {
		return out, err
	}
	if q != "" {
		sort.SliceStable(found, func(i, j int) bool { return found[i].score > found[j].score })
	}
	for _, f := range found {
		if len(out.Symbols) == maxSymbols {
			out.Truncated = true
			break
		}
		out.Symbols = append(out.Symbols, f.e)
	}
	return out, nil
}

// projectScripts — .gd-файлы проекта (без .godot/.git и, по умолчанию, addons/).
func projectScripts(d *Deps, includeAddons bool) []string {
	var out []string
	root := d.Sandbox.Root()
	_ = filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if e.IsDir() {
			rel, _ := filepath.Rel(root, p)
			if n := e.Name(); n == ".godot" || n == ".git" || (!includeAddons && filepath.ToSlash(rel) == "addons") {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".gd") {
			out = append(out, d.Sandbox.ToRes(p))
		}
		return nil
	})
	return out
}

// inComment: стоит ли символ с номером char после # (вне строкового литерала).
func inComment(line string, char int) bool {
	inStr := rune(0)
	for i, r := range []rune(line) {
		if i >= char {
			return false
		}
		switch {
		case inStr != 0 && r == inStr:
			inStr = 0
		case inStr == 0 && (r == '"' || r == '\''):
			inStr = r
		case inStr == 0 && r == '#':
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

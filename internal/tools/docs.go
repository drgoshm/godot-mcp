package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/drgoshm/godot-mcp/internal/docs"
)

type ClassDocsIn struct {
	Name      string `json:"name,omitempty" jsonschema:"class, built-in type or scope: CharacterBody2D, Vector2, @GlobalScope, @GDScript; also Class.member"`
	Member    string `json:"member,omitempty" jsonschema:"method, property, signal, enum or constant; looked up through base classes"`
	Search    string `json:"search,omitempty" jsonschema:"find classes and members whose name contains this text"`
	Inherited bool   `json:"inherited,omitempty" jsonschema:"also list names of inherited members in a class overview"`
}

type ClassDocsOut struct {
	Version string `json:"version"`
	Found   bool   `json:"found"`
}

const classDocsDescription = `Look up the API of the installed Godot version (not Godot 3): classes, built-in types like Vector2 or Array,
@GlobalScope functions and @GDScript annotations, with signatures, defaults and descriptions.
- name: overview of a class (properties with defaults, methods, signals, enums).
- name + member (or "Class.member"): full description; inherited members are found through base classes.
- search: find classes/members by part of the name when you don't know where something lives.
Godot 3 names (KinematicBody2D, instance(), yield...) are recognised and mapped to their Godot 4 replacements.
The first call dumps the engine's API reference (a few seconds, cached per engine version).`

func registerDocsTools(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "godot_class_docs",
		Description: classDocsDescription,
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ClassDocsIn) (*mcp.CallToolResult, ClassDocsOut, error) {
		if in.Name == "" && in.Search == "" {
			return nil, ClassDocsOut{}, errors.New("pass name (optionally with member) or search")
		}
		idx, err := d.Docs.Index(ctx)
		if err != nil {
			return nil, ClassDocsOut{}, err
		}
		out := ClassDocsOut{Version: idx.Version}
		var text string
		if in.Search != "" {
			text, out.Found = idx.Search(in.Search, 40), true
		} else {
			text, out.Found = idx.Lookup(in.Name, in.Member, in.Inherited, projectClasses(d))
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, out, nil
	})
}

var (
	classCacheEntryRe = regexp.MustCompile(`(?s)\{[^{}]*\}`)
	classCacheFieldRe = regexp.MustCompile(`"(base|class|path)":\s*&?"([^"]*)"`)
)

// projectClasses читает class_name проекта из кеша глобальных классов
// (.godot/global_script_class_cache.cfg; появляется после импорта).
func projectClasses(d *Deps) []docs.ProjectClass {
	data, err := os.ReadFile(filepath.Join(d.Sandbox.Root(), ".godot", "global_script_class_cache.cfg"))
	if err != nil {
		return nil
	}
	var out []docs.ProjectClass
	for _, entry := range classCacheEntryRe.FindAllString(string(data), -1) {
		var pc docs.ProjectClass
		for _, m := range classCacheFieldRe.FindAllStringSubmatch(entry, -1) {
			switch m[1] {
			case "base":
				pc.Base = m[2]
			case "class":
				pc.Name = m[2]
			case "path":
				pc.Path = m[2]
			}
		}
		if pc.Name != "" && !strings.HasPrefix(pc.Path, "res://addons/") {
			out = append(out, pc)
		}
	}
	return out
}

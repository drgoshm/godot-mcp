package tools

import (
	"context"
	"path"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/drgoshm/godot-mcp/internal/project"
)

// ---- godot_list_files ----

type ListFilesIn struct {
	Dir         string `json:"dir,omitempty" jsonschema:"directory to list, e.g. res://scenes (default res://)"`
	Recursive   bool   `json:"recursive,omitempty" jsonschema:"walk subdirectories"`
	Pattern     string `json:"pattern,omitempty" jsonschema:"glob on file name, e.g. *.gd or *.tscn"`
	IncludeMeta bool   `json:"include_meta,omitempty" jsonschema:"also show generated *.import and *.uid files"`
	Max         int    `json:"max,omitempty" jsonschema:"max entries (default 500)"`
}

type ListFilesOut struct {
	Entries   []project.FileEntry `json:"entries"`
	Truncated bool                `json:"truncated,omitempty"`
}

// ---- godot_read_file ----

type ReadFileIn struct {
	Path      string `json:"path" jsonschema:"res:// path of a text file (.gd .tscn .tres .cfg .json .gdshader ...)"`
	StartLine int    `json:"start_line,omitempty" jsonschema:"first line to return, 1-based"`
	MaxLines  int    `json:"max_lines,omitempty" jsonschema:"how many lines to return (default: whole file)"`
}

// ---- godot_write_file ----

type WriteFileIn struct {
	Path    string `json:"path" jsonschema:"res:// path to create or overwrite"`
	Content string `json:"content" jsonschema:"full file content; GDScript uses tabs for indentation"`
}

type WriteFileOut struct {
	Path    string `json:"path"`
	Created bool   `json:"created"`
	Bytes   int    `json:"bytes"`
	Hint    string `json:"hint,omitempty"`
}

// ---- godot_edit_file ----

type EditFileIn struct {
	Path       string `json:"path" jsonschema:"res:// path of the file to edit"`
	OldStr     string `json:"old_str" jsonschema:"exact text to replace; must be unique in the file unless replace_all"`
	NewStr     string `json:"new_str" jsonschema:"replacement text (may be empty to delete)"`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"replace every occurrence"`
}

type EditFileOut struct {
	Path         string `json:"path"`
	Replacements int    `json:"replacements"`
	Hint         string `json:"hint,omitempty"`
}

func registerFileTools(s *mcp.Server, d *Deps) {
	sb := d.Sandbox

	mcp.AddTool(s, &mcp.Tool{
		Name:        "godot_list_files",
		Description: "List files in the Godot project. Hides .godot/ and generated .import/.uid files by default.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ListFilesIn) (*mcp.CallToolResult, ListFilesOut, error) {
		entries, truncated, err := sb.List(in.Dir, project.ListOptions{
			Recursive: in.Recursive, Pattern: in.Pattern, IncludeMeta: in.IncludeMeta, Max: in.Max,
		})
		if err != nil {
			return nil, ListFilesOut{}, err
		}
		if entries == nil {
			entries = []project.FileEntry{}
		}
		return nil, ListFilesOut{Entries: entries, Truncated: truncated}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "godot_read_file",
		Description: "Read a text file from the project by res:// path, optionally a line range.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ReadFileIn) (*mcp.CallToolResult, *project.ReadResult, error) {
		res, err := sb.Read(in.Path, in.StartLine, in.MaxLines)
		return nil, res, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "godot_write_file",
		Description: "Create or overwrite a text file in the project (parent dirs are created). " +
			"For scenes prefer godot_create_scene: hand-written .tscn files easily break UIDs and resource ids. " +
			"After writing a .gd file, run godot_check_script.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in WriteFileIn) (*mcp.CallToolResult, WriteFileOut, error) {
		abs, err := sb.Resolve(in.Path)
		if err != nil {
			return nil, WriteFileOut{}, err
		}
		created, err := sb.Write(in.Path, in.Content, true)
		if err != nil {
			return nil, WriteFileOut{}, err
		}
		return nil, WriteFileOut{
			Path: sb.ToRes(abs), Created: created, Bytes: len(in.Content), Hint: hintFor(in.Path, created),
		}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "godot_edit_file",
		Description: "Replace an exact snippet in a text file. Safer than rewriting the whole file. " +
			"Copy old_str verbatim from godot_read_file, including tabs.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in EditFileIn) (*mcp.CallToolResult, EditFileOut, error) {
		n, err := sb.Edit(in.Path, in.OldStr, in.NewStr, in.ReplaceAll)
		if err != nil {
			return nil, EditFileOut{}, err
		}
		return nil, EditFileOut{Path: in.Path, Replacements: n, Hint: hintFor(in.Path, false)}, nil
	})
}

// hintFor подсказывает агенту следующий шаг после записи файла.
func hintFor(p string, created bool) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".gd":
		if created {
			return "run godot_check_script; if this script declares a class_name used elsewhere, run godot_import first to refresh the class cache"
		}
		return "run godot_check_script on this file"
	case ".tscn", ".tres":
		return "hand-edited resource: run the scene (godot_run_project with quit_after) to make sure it still loads"
	}
	return ""
}

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---- godot_scene_tree ----

type SceneTreeIn struct {
	ProjectArg
	Path          string `json:"path" jsonschema:"res:// or uid:// path of a .tscn/.scn scene"`
	Node          string `json:"node,omitempty" jsonschema:"return only this subtree, path relative to the scene root"`
	MaxValueChars int    `json:"max_value_chars,omitempty" jsonschema:"longer property values are replaced by {\"_omitted\": ...} (default 400)"`
}

type SceneTreeOut struct {
	Path        string           `json:"path"`
	UID         string           `json:"uid,omitempty"`
	Nodes       int              `json:"nodes"`
	Root        map[string]any   `json:"root"`
	Connections []map[string]any `json:"connections,omitempty"`
}

const sceneTreeDescription = `Read a scene as JSON in the same format godot_create_scene accepts: a root node with children,
only the properties saved in the file (non-default), embedded resources as {"_type": ...}, instances as {"scene": ...},
plus signal connections. Every node has a "path" relative to the root for use with godot_edit_scene.
Nodes marked "_inherited" come from an instanced scene and only carry overridden properties.
Huge values (e.g. tile data) are replaced by {"_omitted": "..."}; never write those back.`

// ---- godot_edit_scene ----

type EditSceneIn struct {
	ProjectArg
	Path       string           `json:"path" jsonschema:"res:// or uid:// path of the scene to change"`
	Operations []map[string]any `json:"operations" jsonschema:"operations applied in order; if one fails nothing is saved"`
}

type EditSceneOut struct {
	Path        string `json:"path"`
	UID         string `json:"uid,omitempty"`
	Applied     int    `json:"applied"`
	Nodes       int    `json:"nodes"`
	Connections int    `json:"connections"`
}

const editSceneDescription = `Change an existing scene through the engine and save it, keeping its UID. Operations run in order;
if any fails, the file is left untouched. Node paths are relative to the scene root ("." is the root). Operations:
  {"op": "add_node", "parent": "UI", "node": {...node spec as in godot_create_scene...}, "index": 0}
  {"op": "remove_node", "path": "UI/Old"}
  {"op": "set_properties", "path": "Player", "properties": {"speed": 300, "shape": {"_type": "CircleShape2D", "radius": 8}}}
  {"op": "rename", "path": "Player", "name": "Hero"}
  {"op": "move", "path": "UI/Label", "parent": "HUD", "index": 0, "keep_global_transform": false}
  {"op": "groups", "path": "Enemy", "add": ["enemies"], "remove": ["old"]}
  {"op": "connect", "from": "UI/Start", "signal": "pressed", "to": ".", "method": "_on_start_pressed", "binds": [], "flags": 0}
  {"op": "disconnect", "from": "UI/Start", "signal": "pressed", "to": ".", "method": "_on_start_pressed"}
Property values follow godot_create_scene rules. Set "script" with set_properties before properties it declares.
Nodes inside an instanced scene can't be edited here; edit that scene instead. Read the scene first with godot_scene_tree.`

func registerSceneTools(s *mcp.Server, w *Workspace) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "godot_scene_tree",
		Description: sceneTreeDescription,
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SceneTreeIn) (*mcp.CallToolResult, SceneTreeOut, error) {
		d, err := w.Deps(in.Project)
		if err != nil {
			return nil, SceneTreeOut{}, err
		}
		var out SceneTreeOut
		p, err := existingScene(d, in.Path)
		if err != nil {
			return nil, out, err
		}
		spec := map[string]any{"mode": "tree", "path": p, "node": in.Node}
		if in.MaxValueChars > 0 {
			spec["max_value_chars"] = in.MaxValueChars
		}
		err = runSceneTool(ctx, d, "cannot read scene", spec, &out)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "godot_edit_scene",
		Description: editSceneDescription,
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in EditSceneIn) (*mcp.CallToolResult, EditSceneOut, error) {
		d, err := w.Deps(in.Project)
		if err != nil {
			return nil, EditSceneOut{}, err
		}
		var out EditSceneOut
		p, err := existingScene(d, in.Path)
		if err != nil {
			return nil, out, err
		}
		if len(in.Operations) == 0 {
			return nil, out, errors.New("operations is empty")
		}
		err = runSceneTool(ctx, d, "scene not changed", map[string]any{
			"mode": "edit", "path": p, "operations": in.Operations,
		}, &out)
		return nil, out, err
	})
}

// existingScene приводит res:// или uid:// к res://-пути существующей сцены.
func existingScene(d *Deps, p string) (string, error) {
	if strings.HasPrefix(p, "uid://") {
		resolved, err := d.Sandbox.ResolveUID(p)
		if err != nil {
			return "", err
		}
		p = resolved
	}
	abs, err := d.Sandbox.Resolve(p)
	if err != nil {
		return "", err
	}
	if !strings.HasSuffix(abs, ".tscn") && !strings.HasSuffix(abs, ".scn") {
		return "", fmt.Errorf("%s is not a scene (.tscn or .scn)", p)
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("scene %s does not exist", p)
	}
	return d.Sandbox.ToRes(abs), nil
}

// runSceneTool запускает scene_builder.gd со спецификацией и читает его ответ
// из файла: stdout движка обрезается до 64 КБ, а дерево большой сцены бывает больше.
// errPrefix предваряет ошибку, которую сообщил сам скрипт.
func runSceneTool(ctx context.Context, d *Deps, errPrefix string, spec map[string]any, out any) error {
	specData, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	specFile, err := writeTemp("godot-mcp-spec-*.json", specData)
	if err != nil {
		return err
	}
	defer os.Remove(specFile)
	outFile, err := writeTemp("godot-mcp-out-*.json", nil)
	if err != nil {
		return err
	}
	defer os.Remove(outFile)

	res, err := d.Godot.RunSource(ctx, sceneBuilderGD, 60*time.Second, "--spec="+specFile, "--out="+outFile)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(outFile)
	if err != nil || len(data) == 0 {
		return fmt.Errorf("scene tool produced no result (exit %d):\n%s", res.ExitCode, res.Output)
	}
	var status struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &status); err != nil {
		return fmt.Errorf("scene tool returned invalid JSON: %w", err)
	}
	if !status.OK {
		return errors.New(errPrefix + ": " + status.Error)
	}
	return json.Unmarshal(data, out)
}

func writeTemp(pattern string, data []byte) (string, error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		os.Remove(f.Name())
		return "", errors.Join(werr, cerr)
	}
	return f.Name(), nil
}

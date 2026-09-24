package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/drgoshm/godot-mcp/internal/godot"
	"github.com/drgoshm/godot-mcp/internal/project"
)

//go:embed gdscript/scene_builder.gd
var sceneBuilderGD string

// ---- godot_project_info ----

type ProjectInfoOut struct {
	Project       *project.Info `json:"project"`
	Root          string        `json:"root"`
	EngineVersion string        `json:"engine_version"`
}

// ---- godot_check_script ----

type CheckScriptIn struct {
	Paths []string `json:"paths" jsonschema:"res:// paths of .gd files to check"`
}

type ScriptCheck struct {
	Path        string             `json:"path"`
	OK          bool               `json:"ok"`
	Diagnostics []godot.Diagnostic `json:"diagnostics,omitempty"`
}

type CheckScriptOut struct {
	AllOK   bool          `json:"all_ok"`
	Results []ScriptCheck `json:"results"`
}

// ---- godot_import ----

type ImportOut struct {
	OK          bool               `json:"ok"`
	Duration    string             `json:"duration"`
	Diagnostics []godot.Diagnostic `json:"diagnostics,omitempty"`
}

// ---- godot_run_script ----

type RunScriptIn struct {
	Code           string `json:"code" jsonschema:"GDScript source that extends SceneTree; do the work in _init() and call quit()"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"kill after this many seconds (default 30, max 300)"`
}

// ---- godot_create_scene ----

type CreateSceneIn struct {
	Path        string         `json:"path" jsonschema:"res:// path of the scene to create, ending in .tscn"`
	Root        map[string]any `json:"root" jsonschema:"root node spec: {type or scene, name, script?, properties?, groups?, children?[]}; children use the same shape"`
	Connections []Connection   `json:"connections,omitempty" jsonschema:"signal connections saved in the scene"`
	Overwrite   bool           `json:"overwrite,omitempty" jsonschema:"replace the scene if it already exists"`
}

// Connection — соединение сигнала, как [connection] в .tscn.
type Connection struct {
	From   string `json:"from" jsonschema:"emitting node path relative to the scene root; \".\" is the root"`
	Signal string `json:"signal" jsonschema:"signal name, e.g. pressed or body_entered"`
	To     string `json:"to" jsonschema:"receiving node path relative to the scene root; \".\" is the root"`
	Method string `json:"method" jsonschema:"method on the receiver; it must already exist in its script"`
	Binds  []any  `json:"binds,omitempty" jsonschema:"extra arguments appended after the signal's own"`
	Flags  int    `json:"flags,omitempty" jsonschema:"CONNECT_* flags: 1 deferred, 4 one-shot, 8 reference-counted"`
}

type CreateSceneOut struct {
	Path        string `json:"path"`
	UID         string `json:"uid,omitempty"`
	Nodes       int    `json:"nodes"`
	Connections int    `json:"connections,omitempty"`
}

const createSceneDescription = `Build a scene from a node tree and save it through Godot itself, so UIDs, ext_resources and owners are correct.
Node spec: {"type": "CharacterBody2D", "name": "Player", "script": "res://player.gd",
  "properties": {"position": "Vector2(100, 50)", "collision_layer": 2, "texture": "res://icon.svg"},
  "groups": ["players"], "children": [ ...node specs... ]}
Use {"scene": "res://enemy.tscn", "name": "Enemy1"} instead of "type" to instance another scene.
Property values: numbers/bools as JSON; strings for String/NodePath props;
anything else as a Godot literal string such as "Vector2(1, 2)" or "Color(1, 0, 0, 1)".
Resource properties take a res:// path or an embedded resource object saved inside the scene:
  "shape": {"_type": "RectangleShape2D", "size": "Vector2(32, 48)"}
  "texture": {"_type": "GradientTexture2D", "gradient": {"_type": "Gradient", "colors": "PackedColorArray(1,0,0,1, 0,0,1,1)"}}
Other keys of the object are the resource's properties (same rules, nesting allowed). "_type" may be a class_name
(run godot_import after creating it) or pass "_script": "res://item.gd". Arrays such as Array[ItemData] take lists of these.
Connect signals with "connections": [{"from": "UI/Start", "signal": "pressed", "to": ".", "method": "_on_start_pressed"}];
paths are relative to the root. The method must already exist in the receiver's script and accept the signal's arguments.`

func registerEngineTools(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "godot_project_info",
		Description: "Project summary from project.godot: name, main scene, engine features, autoloads, input actions. Call this first.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, ProjectInfoOut, error) {
		info, err := d.Sandbox.LoadInfo()
		if err != nil {
			return nil, ProjectInfoOut{}, err
		}
		return nil, ProjectInfoOut{Project: info, Root: d.Sandbox.Root(), EngineVersion: d.Version}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "godot_check_script",
		Description: "Parse and type-check GDScript files without running them (godot --check-only). Returns errors with file and line.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in CheckScriptIn) (*mcp.CallToolResult, CheckScriptOut, error) {
		if len(in.Paths) == 0 {
			return nil, CheckScriptOut{}, errors.New("paths is empty")
		}
		if len(in.Paths) > 50 {
			return nil, CheckScriptOut{}, errors.New("at most 50 scripts per call")
		}
		out := CheckScriptOut{AllOK: true, Results: make([]ScriptCheck, len(in.Paths))}
		var wg sync.WaitGroup
		sem := make(chan struct{}, 4) // каждый вызов — отдельный процесс Godot
		for i, p := range in.Paths {
			wg.Add(1)
			go func(i int, p string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				out.Results[i] = checkOne(ctx, d, p)
			}(i, p)
		}
		wg.Wait()
		for _, r := range out.Results {
			out.AllOK = out.AllOK && r.OK
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "godot_import",
		Description: "Run the Godot importer headlessly: imports new assets and refreshes the global class_name cache. " +
			"Needed after adding assets or new class_name scripts, and once for a freshly cloned project.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, ImportOut, error) {
		res, err := d.Godot.Import(ctx)
		if err != nil {
			return nil, ImportOut{}, err
		}
		return nil, ImportOut{OK: res.OK(), Duration: res.Duration, Diagnostics: res.Diagnostics}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "godot_run_script",
		Description: "Execute a one-off GDScript (extends SceneTree) headlessly inside the project and return its output. " +
			"Useful for inspecting resources, batch edits via ResourceSaver, or querying ClassDB. Code runs with full engine and OS access.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in RunScriptIn) (*mcp.CallToolResult, *godot.Result, error) {
		if !extendsMainLoop(in.Code) {
			return nil, nil, errors.New("script must start with 'extends SceneTree' (or MainLoop); put the work in _init() and finish with quit()")
		}
		timeout := clampSeconds(in.TimeoutSeconds, 30, 300)
		return run(d.Godot.RunSource(ctx, in.Code, timeout))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "godot_create_scene",
		Description: createSceneDescription,
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in CreateSceneIn) (*mcp.CallToolResult, CreateSceneOut, error) {
		return createScene(ctx, d, in)
	})
}

func checkOne(ctx context.Context, d *Deps, p string) ScriptCheck {
	abs, err := d.Sandbox.Resolve(p)
	if err != nil {
		return failCheck(p, err)
	}
	if !strings.HasSuffix(abs, ".gd") {
		return failCheck(p, errors.New("not a .gd file"))
	}
	if _, err := os.Stat(abs); err != nil {
		return failCheck(p, err)
	}
	resPath := d.Sandbox.ToRes(abs)
	res, err := d.Godot.CheckScript(ctx, resPath)
	if err != nil {
		return failCheck(resPath, err)
	}
	return ScriptCheck{Path: resPath, OK: res.OK(), Diagnostics: res.Diagnostics}
}

func failCheck(p string, err error) ScriptCheck {
	return ScriptCheck{Path: p, Diagnostics: []godot.Diagnostic{{Severity: "error", Kind: "tool", Message: err.Error()}}}
}

func createScene(ctx context.Context, d *Deps, in CreateSceneIn) (*mcp.CallToolResult, CreateSceneOut, error) {
	var zero CreateSceneOut
	abs, err := d.Sandbox.Resolve(in.Path)
	if err != nil {
		return nil, zero, err
	}
	if !strings.HasSuffix(abs, ".tscn") {
		return nil, zero, errors.New("path must end in .tscn")
	}
	if _, err := os.Stat(abs); err == nil && !in.Overwrite {
		return nil, zero, fmt.Errorf("%s already exists; set overwrite=true or edit it", in.Path)
	}
	if len(in.Root) == 0 {
		return nil, zero, errors.New("root is required")
	}

	spec, err := json.Marshal(map[string]any{"path": d.Sandbox.ToRes(abs), "root": in.Root, "connections": in.Connections})
	if err != nil {
		return nil, zero, err
	}
	specFile, err := os.CreateTemp("", "godot-mcp-spec-*.json")
	if err != nil {
		return nil, zero, err
	}
	defer os.Remove(specFile.Name())
	if _, err := specFile.Write(spec); err != nil {
		specFile.Close()
		return nil, zero, err
	}
	specFile.Close()

	res, err := d.Godot.RunSource(ctx, sceneBuilderGD, 60*time.Second, "--spec="+specFile.Name())
	if err != nil {
		return nil, zero, err
	}
	var payload struct {
		OK          bool   `json:"ok"`
		Error       string `json:"error"`
		Path        string `json:"path"`
		UID         string `json:"uid"`
		Nodes       int    `json:"nodes"`
		Connections int    `json:"connections"`
	}
	if !findResult(res.Output, &payload) {
		return nil, zero, fmt.Errorf("scene builder produced no result (exit %d):\n%s", res.ExitCode, res.Output)
	}
	if !payload.OK {
		return nil, zero, errors.New("scene not created: " + payload.Error)
	}
	return nil, CreateSceneOut{Path: payload.Path, UID: payload.UID, Nodes: payload.Nodes, Connections: payload.Connections}, nil
}

// findResult ищет строку MCP_RESULT:{...} в выводе служебного скрипта.
func findResult(output string, v any) bool {
	for _, line := range strings.Split(output, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "MCP_RESULT:"); ok {
			return json.Unmarshal([]byte(rest), v) == nil
		}
	}
	return false
}

var extendsRe = regexp.MustCompile(`(?m)^\s*extends\s+(SceneTree|MainLoop)\b`)

func extendsMainLoop(code string) bool { return extendsRe.MatchString(code) }

func clampSeconds(v, def, max int) time.Duration {
	if v <= 0 {
		v = def
	}
	if v > max {
		v = max
	}
	return time.Duration(v) * time.Second
}

func run(res *godot.Result, err error) (*mcp.CallToolResult, *godot.Result, error) {
	return nil, res, err
}

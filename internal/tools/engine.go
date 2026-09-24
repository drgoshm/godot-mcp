package tools

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log"
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

func projectInfo(d *Deps) (*mcp.CallToolResult, ProjectInfoOut, error) {
	info, err := d.Sandbox.LoadInfo()
	if err != nil {
		return nil, ProjectInfoOut{}, err
	}
	return nil, ProjectInfoOut{Project: info, Root: d.Sandbox.Root(), EngineVersion: d.Version}, nil
}

// ---- godot_check_script ----

type CheckScriptIn struct {
	ProjectArg
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
	Via     string        `json:"via"`            // lsp | cli
	Note    string        `json:"note,omitempty"` // почему пришлось обойтись без LSP
}

// ---- godot_import ----

type ImportOut struct {
	OK          bool               `json:"ok"`
	Duration    string             `json:"duration"`
	Diagnostics []godot.Diagnostic `json:"diagnostics,omitempty"`
}

// ---- godot_run_script ----

type RunScriptIn struct {
	ProjectArg
	Code           string `json:"code" jsonschema:"GDScript source that extends SceneTree; do the work in _init() and call quit()"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"kill after this many seconds (default 30, max 300)"`
}

// ---- godot_create_scene ----

type CreateSceneIn struct {
	ProjectArg
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

func registerEngineTools(s *mcp.Server, w *Workspace) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "godot_project_info",
		Description: "Project summary from project.godot: name, main scene, engine features, autoloads, input actions. Call this first.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ProjectOnlyIn) (*mcp.CallToolResult, ProjectInfoOut, error) {
		d, err := w.Deps(in.Project)
		if err != nil {
			return nil, ProjectInfoOut{}, err
		}
		return projectInfo(d)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "godot_check_script",
		Description: "Parse and type-check GDScript files without running them. Returns errors (and, via the language server, warnings) " +
			"with file and line. Uses a background headless editor's language server when available (milliseconds per file; " +
			"the first call starts it in a few seconds), otherwise godot --check-only per file.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in CheckScriptIn) (*mcp.CallToolResult, CheckScriptOut, error) {
		d, err := w.Deps(in.Project)
		if err != nil {
			return nil, CheckScriptOut{}, err
		}
		if len(in.Paths) == 0 {
			return nil, CheckScriptOut{}, errors.New("paths is empty")
		}
		if len(in.Paths) > 50 {
			return nil, CheckScriptOut{}, errors.New("at most 50 scripts per call")
		}
		out := CheckScriptOut{AllOK: true, Results: make([]ScriptCheck, len(in.Paths)), Via: "cli"}
		var todo []int // индексы путей, которые прошли проверку и ждут анализа
		for i, p := range in.Paths {
			res, err := resolveScript(d, p)
			if err != nil {
				out.Results[i] = failCheck(p, err)
				continue
			}
			out.Results[i].Path = res
			todo = append(todo, i)
		}

		if d.LSP != nil && len(todo) > 0 {
			paths := make([]string, len(todo))
			for k, i := range todo {
				paths[k] = out.Results[i].Path
			}
			diags, err := d.LSP.Check(ctx, paths)
			if err == nil {
				out.Via = "lsp"
				for _, i := range todo {
					out.Results[i] = lspCheck(out.Results[i].Path, diags[out.Results[i].Path])
				}
				todo = nil
			} else {
				log.Printf("language server check failed, using --check-only: %v", err)
				out.Note = "language server unavailable (" + err.Error() + "); checked with godot --check-only"
			}
		}

		var wg sync.WaitGroup
		sem := make(chan struct{}, 4) // каждый вызов — отдельный процесс Godot
		for _, i := range todo {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				out.Results[i] = checkOne(ctx, d, out.Results[i].Path)
			}(i)
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
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ProjectOnlyIn) (*mcp.CallToolResult, ImportOut, error) {
		d, err := w.Deps(in.Project)
		if err != nil {
			return nil, ImportOut{}, err
		}
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
		d, err := w.Deps(in.Project)
		if err != nil {
			return nil, nil, err
		}
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
		d, err := w.Deps(in.Project)
		if err != nil {
			return nil, CreateSceneOut{}, err
		}
		return createScene(ctx, d, in)
	})
}

// resolveScript проверяет, что путь — существующий .gd в проекте.
func resolveScript(d *Deps, p string) (string, error) {
	abs, err := d.Sandbox.Resolve(p)
	if err != nil {
		return "", err
	}
	if !strings.HasSuffix(abs, ".gd") {
		return "", errors.New("not a .gd file")
	}
	if _, err := os.Stat(abs); err != nil {
		return "", err
	}
	return d.Sandbox.ToRes(abs), nil
}

// lspCheck: ошибки делают проверку неуспешной, предупреждения — нет.
func lspCheck(resPath string, ds []godot.Diagnostic) ScriptCheck {
	ok := true
	for _, d := range ds {
		ok = ok && d.Severity != "error"
	}
	return ScriptCheck{Path: resPath, OK: ok, Diagnostics: ds}
}

// checkOne — проверка отдельным процессом godot --check-only.
func checkOne(ctx context.Context, d *Deps, resPath string) ScriptCheck {
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
		return nil, zero, fmt.Errorf("%s already exists; set overwrite=true or change it with godot_edit_scene", in.Path)
	}
	if len(in.Root) == 0 {
		return nil, zero, errors.New("root is required")
	}
	var out CreateSceneOut
	err = runSceneTool(ctx, d, "scene not created", map[string]any{
		"mode": "create", "path": d.Sandbox.ToRes(abs), "root": in.Root, "connections": in.Connections,
	}, &out)
	return nil, out, err
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

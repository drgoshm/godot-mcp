package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"godot-mcp/internal/godot"
)

// ---- godot_run_project ----

type RunProjectIn struct {
	Scene       string   `json:"scene,omitempty" jsonschema:"res:// or uid:// scene to run; default is the project's main scene"`
	Headless    bool     `json:"headless,omitempty" jsonschema:"run without a window (no rendering; logic, physics and prints still work)"`
	QuitAfter   int      `json:"quit_after,omitempty" jsonschema:"quit after N frames: handy as a smoke test that the scene loads and runs"`
	Debug       bool     `json:"debug,omitempty" jsonschema:"show collision shapes and navigation meshes"`
	Args        []string `json:"args,omitempty" jsonschema:"user arguments passed after --, readable via OS.get_cmdline_user_args()"`
	WaitSeconds int      `json:"wait_seconds,omitempty" jsonschema:"collect startup output for this long before returning (default 3, max 60)"`
}

// ---- godot_get_output ----

type GetOutputIn struct {
	RunID       string `json:"run_id,omitempty" jsonschema:"run to read; default is the latest run"`
	Since       int64  `json:"since,omitempty" jsonschema:"return only lines after this seq (use next_since from the previous call)"`
	MaxLines    int    `json:"max_lines,omitempty" jsonschema:"max lines to return (default 200)"`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"long-poll: wait up to N seconds for new output or exit (max 60)"`
	OnlyErrors  bool   `json:"only_errors,omitempty" jsonschema:"return diagnostics only, without raw lines"`
}

// RunOutput — общий ответ run/get_output/stop.
type RunOutput struct {
	Run         godot.RunState     `json:"run"`
	Lines       []godot.Line       `json:"lines,omitempty"`
	NextSince   int64              `json:"next_since"`
	Dropped     bool               `json:"dropped,omitempty"`
	Diagnostics []godot.Diagnostic `json:"diagnostics,omitempty"`
}

// ---- godot_stop_project ----

type StopProjectIn struct {
	RunID string `json:"run_id,omitempty" jsonschema:"run to stop; default is the latest run"`
}

type ListRunsOut struct {
	Runs []godot.RunState `json:"runs"`
}

func registerRunTools(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "godot_run_project",
		Description: "Start the game (or one scene) in the background and return its run_id plus the first seconds of output " +
			"and parsed runtime errors. Keep reading with godot_get_output; stop with godot_stop_project.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in RunProjectIn) (*mcp.CallToolResult, RunOutput, error) {
		scene, err := resolveScene(d, in.Scene)
		if err != nil {
			return nil, RunOutput{}, err
		}
		run, err := d.Runner.Start(godot.StartOptions{
			Scene: scene, Headless: in.Headless, QuitAfter: in.QuitAfter, Debug: in.Debug, UserArgs: in.Args,
		})
		if err != nil {
			return nil, RunOutput{}, err
		}
		run.Wait(ctx, clampSeconds(in.WaitSeconds, 3, 60))
		return nil, snapshot(run, 0, 200, false), nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "godot_get_output",
		Description: "Read new output of a running or finished game since a cursor, with parsed errors (file:line, backtrace).",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in GetOutputIn) (*mcp.CallToolResult, RunOutput, error) {
		run, err := d.Runner.Get(in.RunID)
		if err != nil {
			return nil, RunOutput{}, err
		}
		if in.WaitSeconds > 0 {
			waitForOutput(ctx, run, in.Since, clampSeconds(in.WaitSeconds, 0, 60))
		}
		max := in.MaxLines
		if max <= 0 {
			max = 200
		}
		return nil, snapshot(run, in.Since, max, in.OnlyErrors), nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "godot_stop_project",
		Description: "Stop a running game (SIGTERM, then SIGKILL after 3s). Returns final status and all errors from the buffered output.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in StopProjectIn) (*mcp.CallToolResult, RunOutput, error) {
		run, err := d.Runner.Stop(ctx, in.RunID)
		if err != nil {
			return nil, RunOutput{}, err
		}
		return nil, snapshot(run, 0, 0, true), nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "godot_list_runs",
		Description: "List recent game runs with their status.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, ListRunsOut, error) {
		runs := d.Runner.List()
		if runs == nil {
			runs = []godot.RunState{}
		}
		return nil, ListRunsOut{Runs: runs}, nil
	})
}

// resolveScene проверяет сцену и переводит uid:// в путь, чтобы ошибка
// «нет такой сцены» пришла сразу, а не из лога движка.
func resolveScene(d *Deps, scene string) (string, error) {
	if scene == "" {
		info, err := d.Sandbox.LoadInfo()
		if err != nil {
			return "", err
		}
		if info.MainScene == "" {
			return "", errors.New("project has no main scene; pass scene explicitly")
		}
		return "", nil // Godot сам запустит главную сцену
	}
	if strings.HasPrefix(scene, "uid://") {
		p, err := d.Sandbox.ResolveUID(scene)
		if err != nil {
			return "", err
		}
		scene = p
	}
	abs, err := d.Sandbox.Resolve(scene)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("scene %s does not exist", scene)
	}
	return d.Sandbox.ToRes(abs), nil
}

func snapshot(run *godot.Run, since int64, max int, onlyErrors bool) RunOutput {
	lines, next, dropped := run.Output(since, max)
	out := RunOutput{
		Run:         run.State(),
		NextSince:   next,
		Dropped:     dropped,
		Diagnostics: godot.LinesDiagnostics(lines),
	}
	if !onlyErrors {
		out.Lines = lines
	}
	return out
}

// waitForOutput — простой long-poll: ждём новых строк или завершения процесса.
func waitForOutput(ctx context.Context, run *godot.Run, since int64, max time.Duration) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if lines, _, _ := run.Output(since, 1); len(lines) > 0 || !run.Alive() {
			return
		}
		if run.Wait(ctx, 200*time.Millisecond) {
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}

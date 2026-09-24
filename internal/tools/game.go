package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/drgoshm/godot-mcp/internal/godot"
)

// Инструменты godot_game_* работают с запущенной игрой через мост
// (internal/godot/gdscript/mcp_bridge.gd). Пути узлов: абсолютные
// ("/root/Main/Player") или относительно текущей сцены ("Player").

type GameTreeIn struct {
	RunID      string   `json:"run_id,omitempty" jsonschema:"run to inspect; default is the latest run"`
	Node       string   `json:"node,omitempty" jsonschema:"subtree root: absolute (/root/Main) or relative to the current scene; default /root"`
	Depth      int      `json:"depth,omitempty" jsonschema:"how many levels to descend (default 4)"`
	Properties []string `json:"properties,omitempty" jsonschema:"property values to include for every node that has them, e.g. position, velocity, text"`
}

type GameEvalIn struct {
	RunID      string `json:"run_id,omitempty" jsonschema:"run to query; default is the latest run"`
	Expression string `json:"expression" jsonschema:"Godot Expression evaluated with the node as self, e.g. position, velocity.length(), get_node('Gun').ammo, is_on_floor(); variables tree and scene are available"`
	Node       string `json:"node,omitempty" jsonschema:"node used as self; default is the current scene"`
}

type GameSetIn struct {
	RunID      string         `json:"run_id,omitempty" jsonschema:"run to change; default is the latest run"`
	Node       string         `json:"node" jsonschema:"node to change: absolute or relative to the current scene"`
	Properties map[string]any `json:"properties" jsonschema:"values to set; strings are parsed as Godot literals for non-string properties (\"Vector2(1, 2)\"); \"position:x\" sets one component"`
}

type GameInputIn struct {
	RunID string           `json:"run_id,omitempty" jsonschema:"run to send input to; default is the latest run"`
	Steps []map[string]any `json:"steps" jsonschema:"input steps run in order"`
}

type GameScreenshotIn struct {
	RunID    string `json:"run_id,omitempty" jsonschema:"run to capture; default is the latest run"`
	MaxWidth int    `json:"max_width,omitempty" jsonschema:"scale the image down to this width (default 1280)"`
}

const gameInputDescription = `Send input to the running game, step by step, waiting where asked. Steps:
  {"action": "jump", "pressed": true}            press/release an Input Map action (pressed defaults to true)
  {"tap": "jump", "duration": 0.1}               press, hold, release
  {"key": "Space"}                               press and release a key; add "pressed" to only press or release
  {"text": "hello"}                              type text into the focused control
  {"mouse_button": "left", "position": [x, y]}   click; add "pressed" to only press or release
  {"mouse_move": [x, y]}
  {"wait": 0.5} or {"wait_frames": 10}
Events go through Input.parse_input_event, so both Input.is_action_pressed() and _input() see them. Works headless too.
Hold an action and check the result: [{"action": "move_right"}, {"wait": 0.5}, {"action": "move_right", "pressed": false}] then godot_game_eval.`

func registerGameTools(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "godot_game_tree",
		Description: "Live scene tree of the running game: node names, types, paths, scripts, instanced scenes, " +
			"plus any property values you ask for. Also returns the current scene, pause state, frame and FPS.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in GameTreeIn) (*mcp.CallToolResult, map[string]any, error) {
		args := map[string]any{"node": orDefault(in.Node, "/root"), "depth": in.Depth, "properties": in.Properties}
		if in.Depth <= 0 {
			args["depth"] = 4
		}
		out, err := gameCall(ctx, d, in.RunID, "tree", args, 10*time.Second)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "godot_game_eval",
		Description: "Evaluate a Godot Expression inside the running game with a node as self: read state " +
			"(position, velocity, health, is_on_floor()) or call methods (take_damage(10), get_tree().reload_current_scene()). " +
			"Expressions cannot assign; use godot_game_set for that.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in GameEvalIn) (*mcp.CallToolResult, map[string]any, error) {
		if in.Expression == "" {
			return nil, nil, errors.New("expression is empty")
		}
		out, err := gameCall(ctx, d, in.RunID, "eval", map[string]any{"expression": in.Expression, "node": in.Node}, 10*time.Second)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "godot_game_set",
		Description: "Set properties of a node in the running game (not saved to the scene file). Returns the values after the change.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in GameSetIn) (*mcp.CallToolResult, map[string]any, error) {
		if in.Node == "" || len(in.Properties) == 0 {
			return nil, nil, errors.New("node and properties are required")
		}
		out, err := gameCall(ctx, d, in.RunID, "set", map[string]any{"node": in.Node, "properties": in.Properties}, 10*time.Second)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "godot_game_input",
		Description: gameInputDescription,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in GameInputIn) (*mcp.CallToolResult, map[string]any, error) {
		if len(in.Steps) == 0 {
			return nil, nil, errors.New("steps is empty")
		}
		out, err := gameCall(ctx, d, in.RunID, "input", map[string]any{"steps": in.Steps}, inputTimeout(in.Steps))
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "godot_game_screenshot",
		Description: "Capture the current frame of the running game as an image (the game must not be headless).",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in GameScreenshotIn) (*mcp.CallToolResult, FrameInfo, error) {
		raw, err := gameCallRaw(ctx, d, in.RunID, "screenshot", nil, 15*time.Second)
		if err != nil {
			return nil, FrameInfo{}, err
		}
		var shot struct {
			PNG string `json:"png"`
		}
		if err := json.Unmarshal(raw, &shot); err != nil {
			return nil, FrameInfo{}, err
		}
		png, err := base64.StdEncoding.DecodeString(shot.PNG)
		if err != nil {
			return nil, FrameInfo{}, err
		}
		data, w, h, err := scalePNG(png, orDefaultInt(in.MaxWidth, defaultMaxWidth))
		if err != nil {
			return nil, FrameInfo{}, err
		}
		info := FrameInfo{Width: w, Height: h}
		summary, _ := json.Marshal(info)
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: string(summary)},
			&mcp.ImageContent{Data: data, MIMEType: "image/png"},
		}}, info, nil
	})
}

// gameCall отправляет команду и возвращает ответ игры как объект.
func gameCall(ctx context.Context, d *Deps, runID, cmd string, args map[string]any, timeout time.Duration) (map[string]any, error) {
	raw, err := gameCallRaw(ctx, d, runID, cmd, args, timeout)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("unexpected reply from the game: %w", err)
	}
	return out, nil
}

// gameCallRaw находит запуск и отправляет команду его мосту.
func gameCallRaw(ctx context.Context, d *Deps, runID, cmd string, args map[string]any, timeout time.Duration) (json.RawMessage, error) {
	run, err := d.Runner.Get(runID)
	if err != nil {
		return nil, err
	}
	if !run.Alive() {
		return nil, fmt.Errorf("%s has exited; start the game again with godot_run_project", run.ID)
	}
	if run.Bridge == nil {
		return nil, fmt.Errorf("%s was started with no_bridge; restart it without that option", run.ID)
	}
	out, err := run.Bridge.Call(ctx, cmd, args, timeout)
	if errors.Is(err, godot.ErrNoBridge) {
		return nil, fmt.Errorf("%w; check godot_get_output for errors", err)
	}
	return out, err
}

// inputTimeout: сумма ожиданий в шагах плюс запас.
func inputTimeout(steps []map[string]any) time.Duration {
	total := 15 * time.Second
	for _, s := range steps {
		for _, key := range []string{"wait", "duration"} {
			if v, ok := s[key].(float64); ok && v > 0 {
				total += time.Duration(v * float64(time.Second))
			}
		}
		if v, ok := s["wait_frames"].(float64); ok && v > 0 {
			total += time.Duration(v) * 50 * time.Millisecond
		}
	}
	return min(total, 10*time.Minute)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func orDefaultInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

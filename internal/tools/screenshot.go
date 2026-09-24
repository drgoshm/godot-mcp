package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/drgoshm/godot-mcp/internal/godot"
)

const (
	maxScreenshotFrames = 8
	maxScreenshotFrame  = 3600 // минута игрового времени при 60 FPS
	defaultMaxWidth     = 1280
)

type ScreenshotIn struct {
	Scene      string   `json:"scene,omitempty" jsonschema:"res:// or uid:// scene to run; default is the project's main scene"`
	Frames     []int    `json:"frames,omitempty" jsonschema:"frame numbers to capture (default [60]); frame N shows the game after N/fps seconds; up to 8 frames, at most 3600"`
	FPS        int      `json:"fps,omitempty" jsonschema:"fixed frame rate for the recording (default 60)"`
	Resolution string   `json:"resolution,omitempty" jsonschema:"window size such as 1280x720; default comes from project settings"`
	MaxWidth   int      `json:"max_width,omitempty" jsonschema:"scale images down to this width (default 1280)"`
	Args       []string `json:"args,omitempty" jsonschema:"user arguments passed after --, readable via OS.get_cmdline_user_args()"`
}

type FrameInfo struct {
	Frame   int     `json:"frame"`
	Seconds float64 `json:"seconds"`
	Width   int     `json:"width"`
	Height  int     `json:"height"`
}

type ScreenshotOut struct {
	Frames      []FrameInfo        `json:"frames"`
	Missing     []int              `json:"missing,omitempty"`
	ExitCode    int                `json:"exit_code"`
	Duration    string             `json:"duration"`
	Diagnostics []godot.Diagnostic `json:"diagnostics,omitempty"`
	Output      string             `json:"output,omitempty"`
}

const screenshotDescription = `Run the game (or one scene) in a window and return screenshots of chosen frames as images.
Time is deterministic: frame N shows the game after exactly N/fps seconds, so the same call gives the same picture.
Returns runtime errors from that period too. Opens a real window for about as long as the game time recorded
(it cannot run headless). Use args or a debug scene to put the game into the state you want to see.`

var (
	resolutionRe = regexp.MustCompile(`^\d{2,5}x\d{2,5}$`)
	// Итоговая сводка Movie Maker — шум для агента.
	movieStatsRe = regexp.MustCompile(`^(-{20,}|Done recording movie|\d+ frames at \d+ FPS|CPU render time|GPU render time|Encoding time)`)
)

func registerScreenshotTools(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "godot_screenshot",
		Description: screenshotDescription,
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ScreenshotIn) (*mcp.CallToolResult, ScreenshotOut, error) {
		return screenshot(ctx, d, in)
	})
}

func screenshot(ctx context.Context, d *Deps, in ScreenshotIn) (*mcp.CallToolResult, ScreenshotOut, error) {
	var zero ScreenshotOut
	frames := slices.Clone(in.Frames)
	if len(frames) == 0 {
		frames = []int{60}
	}
	slices.Sort(frames)
	frames = slices.Compact(frames)
	if len(frames) > maxScreenshotFrames {
		return nil, zero, fmt.Errorf("at most %d frames per call", maxScreenshotFrames)
	}
	if frames[0] < 0 || frames[len(frames)-1] > maxScreenshotFrame {
		return nil, zero, fmt.Errorf("frames must be between 0 and %d", maxScreenshotFrame)
	}
	fps := in.FPS
	if fps <= 0 {
		fps = 60
	}
	if fps > 240 {
		return nil, zero, errors.New("fps must be at most 240")
	}
	if in.Resolution != "" && !resolutionRe.MatchString(in.Resolution) {
		return nil, zero, fmt.Errorf("resolution %q must look like 1280x720", in.Resolution)
	}
	maxWidth := in.MaxWidth
	if maxWidth <= 0 {
		maxWidth = defaultMaxWidth
	}
	scene, err := resolveScene(d, in.Scene)
	if err != nil {
		return nil, zero, err
	}

	// Запись идёт примерно в реальном времени; запас на запуск и медленные машины.
	gameTime := time.Duration(frames[len(frames)-1]) * time.Second / time.Duration(fps)
	timeout := min(3*gameTime+30*time.Second, 10*time.Minute)
	capture, err := d.Godot.CaptureFrames(ctx, godot.CaptureOptions{
		Scene: scene, Frames: frames, FPS: fps, Resolution: in.Resolution, UserArgs: in.Args, Timeout: timeout,
	})
	if err != nil {
		return nil, zero, err
	}
	defer capture.Cleanup()

	out := ScreenshotOut{
		Missing:     capture.MissingFrames(frames),
		ExitCode:    capture.Result.ExitCode,
		Duration:    capture.Result.Duration,
		Diagnostics: capture.Result.Diagnostics,
		Frames:      []FrameInfo{},
	}
	var images []mcp.Content
	for _, f := range frames {
		p, ok := capture.Frames[f]
		if !ok {
			continue
		}
		data, w, h, err := loadPNG(p, maxWidth)
		if err != nil {
			return nil, zero, fmt.Errorf("frame %d: %w", f, err)
		}
		out.Frames = append(out.Frames, FrameInfo{Frame: f, Seconds: float64(f) / float64(fps), Width: w, Height: h})
		images = append(images, &mcp.ImageContent{Data: data, MIMEType: "image/png"})
	}
	if len(out.Missing) > 0 || capture.Result.TimedOut || !capture.Result.OK() {
		out.Output = tailLines(filterMovieStats(capture.Result.Output), 40)
	}
	if len(images) == 0 {
		msg := "no frames were captured"
		if capture.Result.TimedOut {
			msg += " (timed out)"
		}
		if strings.Contains(capture.Result.Output, "DisplayServer") || strings.Contains(capture.Result.Output, "display") {
			msg += "; a screenshot needs a display (on a headless Linux server run the MCP server under xvfb-run)"
		}
		return nil, zero, fmt.Errorf("%s, exit code %d:\n%s", msg, out.ExitCode, out.Output)
	}

	// Если заполнен Content, SDK не дублирует structured output текстом —
	// кладём JSON сами, чтобы клиенты без structuredContent видели ошибки.
	summary, _ := json.Marshal(out)
	res := &mcp.CallToolResult{Content: append([]mcp.Content{&mcp.TextContent{Text: string(summary)}}, images...)}
	return res, out, nil
}

func filterMovieStats(output string) string {
	var kept []string
	for _, l := range strings.Split(output, "\n") {
		if !movieStatsRe.MatchString(strings.TrimSpace(l)) {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = append([]string{fmt.Sprintf("... %d earlier lines omitted", len(lines)-n)}, lines[len(lines)-n:]...)
	}
	return strings.Join(lines, "\n")
}

package tools

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDownscale(t *testing.T) {
	// 4×2: левая половина — непрозрачный красный, правая — полупрозрачный синий.
	src := image.NewNRGBA(image.Rect(0, 0, 4, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 4; x++ {
			c := color.NRGBA{255, 0, 0, 255}
			if x >= 2 {
				c = color.NRGBA{0, 0, 255, 128}
			}
			src.SetNRGBA(x, y, c)
		}
	}
	dst := downscale(src, 2, 1)
	if got := dst.NRGBAAt(0, 0); got != (color.NRGBA{255, 0, 0, 255}) {
		t.Errorf("left pixel = %v", got)
	}
	// Цвет полупрозрачного пикселя не должен темнеть от premultiplied-арифметики.
	if got := dst.NRGBAAt(1, 0); got.B < 254 || got.R != 0 || got.A < 127 || got.A > 128 {
		t.Errorf("right pixel = %v", got)
	}
}

func TestLoadPNGScales(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.png")
	var buf bytes.Buffer
	must(t, png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 300, 100))))
	must(t, os.WriteFile(p, buf.Bytes(), 0o644))
	if _, w, h, err := loadPNG(p, 150); err != nil || w != 150 || h != 50 {
		t.Errorf("loadPNG scaled = %dx%d, %v", w, h, err)
	}
	if data, w, _, err := loadPNG(p, 1280); err != nil || w != 300 || !bytes.Equal(data, buf.Bytes()) {
		t.Errorf("small image must be returned as is: w=%d err=%v", w, err)
	}
}

// Настоящий Movie Maker: нужен Godot и дисплей (окно открывается на ~1 с).
func TestScreenshot(t *testing.T) {
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" {
		t.Skip("no display; run under xvfb-run")
	}
	call, dir, cs := newSession(t, "Shots")
	files := map[string]string{
		"project.godot": "config_version=5\n\n[application]\n\nconfig/name=\"Shots\"\nrun/main_scene=\"res://main.tscn\"\n\n[display]\n\nwindow/size/viewport_width=320\nwindow/size/viewport_height=240\n",
		// Круг едет вправо на 100 px в секунду игрового времени.
		"main.gd":   "extends Node2D\n\nvar t: float = 0.0\n\nfunc _ready() -> void:\n\tif \"--fail\" in OS.get_cmdline_user_args():\n\t\tpush_error(\"boom from ready\")\n\tif \"--quit\" in OS.get_cmdline_user_args():\n\t\tget_tree().create_timer(0.2).timeout.connect(get_tree().quit)\n\nfunc _process(delta: float) -> void:\n\tt += delta\n\tqueue_redraw()\n\nfunc _draw() -> void:\n\tdraw_rect(Rect2(0, 0, 320, 240), Color(0, 0, 0.5))\n\tdraw_circle(Vector2(20 + t * 100, 120), 20, Color(1, 0.5, 0))\n",
		"main.tscn": "[gd_scene format=3]\n\n[ext_resource type=\"Script\" path=\"res://main.gd\" id=\"1\"]\n\n[node name=\"Main\" type=\"Node2D\"]\nscript = ExtResource(\"1\")\n",
	}
	for name, content := range files {
		must(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
	}

	shoot := func(args map[string]any) (*mcp.CallToolResult, []image.Image) {
		t.Helper()
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "godot_screenshot", Arguments: args})
		must(t, err)
		var imgs []image.Image
		for _, c := range res.Content {
			if ic, ok := c.(*mcp.ImageContent); ok {
				img, err := png.Decode(bytes.NewReader(ic.Data))
				must(t, err)
				imgs = append(imgs, img)
			}
		}
		return res, imgs
	}
	isOrange := func(c color.Color) bool {
		r, g, b, _ := c.RGBA()
		return r>>8 > 200 && g>>8 > 100 && g>>8 < 160 && b>>8 < 50
	}

	res, imgs := shoot(map[string]any{"frames": []int{60, 0}})
	if res.IsError || len(imgs) != 2 {
		t.Fatalf("screenshot: error=%v images=%d %s", res.IsError, len(imgs), mustJSON(res.Content))
	}
	// Кадры отсортированы: 0, потом 60. Кадр N — ровно N/60 с игрового времени.
	if !isOrange(imgs[0].At(20, 120)) || isOrange(imgs[0].At(120, 120)) {
		t.Errorf("frame 0: circle should be at x=20")
	}
	if !isOrange(imgs[1].At(120, 120)) || isOrange(imgs[1].At(20, 120)) {
		t.Errorf("frame 60: circle should be at x=120")
	}
	if b := imgs[0].Bounds(); b.Dx() != 320 || b.Dy() != 240 {
		t.Errorf("frame size = %v", b)
	}

	// Уменьшение и ошибки игры в ответе.
	res, imgs = shoot(map[string]any{"frames": []int{30}, "max_width": 160, "args": []string{"--fail"}})
	if res.IsError || len(imgs) != 1 || imgs[0].Bounds().Dx() != 160 || !isOrange(imgs[0].At(35, 60)) {
		t.Fatalf("scaled screenshot: %s", mustJSON(res.Content))
	}
	if !strings.Contains(mustJSON(res.StructuredContent), "boom from ready") {
		t.Errorf("runtime errors must be reported: %s", mustJSON(res.StructuredContent))
	}

	// Игра вышла раньше: есть что есть, остальное в missing.
	out, ok := call("godot_screenshot", map[string]any{"frames": []int{5, 120}, "args": []string{"--quit"}})
	if !ok || !strings.Contains(mustJSON(out), `"missing":[120]`) || !strings.Contains(mustJSON(out), `"frame":5`) {
		t.Errorf("early quit: %v", out)
	}

	for _, bad := range []map[string]any{
		{"frames": []int{1, 2, 3, 4, 5, 6, 7, 8, 9}},
		{"frames": []int{5000}},
		{"resolution": "big"},
	} {
		if out, ok := call("godot_screenshot", bad); ok {
			t.Errorf("%v should be rejected: %v", bad, out)
		}
	}
	if entries, _ := filepath.Glob(filepath.Join(os.TempDir(), "godot-mcp-frames-*")); len(entries) > 0 {
		t.Errorf("temporary frame directories left behind: %v", entries)
	}
}

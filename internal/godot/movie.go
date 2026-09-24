package godot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"time"
)

// CaptureOptions — параметры съёмки кадров через Movie Maker (--write-movie).
type CaptureOptions struct {
	Scene      string   // res://-путь сцены; пусто = главная сцена
	Frames     []int    // какие кадры сохранить, с нуля
	FPS        int      // --fixed-fps: время в игре течёт ровно 1/FPS за кадр
	Resolution string   // размер окна "1280x720"; пусто = из настроек проекта
	UserArgs   []string // аргументы игры после "--"
	Timeout    time.Duration
}

// Capture — итог съёмки. Файлы лежат во временном каталоге; вызовите Cleanup.
type Capture struct {
	Result *Result
	Frames map[int]string // номер кадра -> путь к PNG
	dir    string
}

// Cleanup удаляет временный каталог с кадрами.
func (c *Capture) Cleanup() { os.RemoveAll(c.dir) }

var frameFileRe = regexp.MustCompile(`^frame(\d{8})\.png$`)

// CaptureFrames запускает проект с --write-movie и возвращает нужные кадры.
// Кадры рендерятся детерминированно (--fixed-fps), поэтому кадр N — это
// состояние игры ровно через N/FPS секунд. Нужно настоящее окно: в --headless
// рендера нет и Movie Maker падает.
func (g *Godot) CaptureFrames(ctx context.Context, opt CaptureOptions) (*Capture, error) {
	if len(opt.Frames) == 0 {
		return nil, errors.New("no frames requested")
	}
	dir, err := os.MkdirTemp("", "godot-mcp-frames-*")
	if err != nil {
		return nil, err
	}
	c := &Capture{Frames: map[int]string{}, dir: dir}

	last := slices.Max(opt.Frames)
	args := []string{
		"--write-movie", filepath.Join(dir, "frame.png"),
		"--fixed-fps", strconv.Itoa(opt.FPS),
		"--quit-after", strconv.Itoa(last + 1), // кадры 0..last
		"--disable-vsync",
	}
	if opt.Resolution != "" {
		args = append(args, "--resolution", opt.Resolution)
	}
	if opt.Scene != "" {
		args = append(args, opt.Scene)
	}
	if len(opt.UserArgs) > 0 {
		args = append(append(args, "--"), opt.UserArgs...)
	}

	// Movie Maker пишет каждый кадр. Ненужные удаляем по ходу, чтобы минута
	// записи не занимала сотни мегабайт.
	wanted := map[int]bool{}
	for _, f := range opt.Frames {
		wanted[f] = true
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				pruneFrames(dir, wanted, false)
			}
		}
	}()

	res, err := g.Exec(ctx, opt.Timeout, args...)
	close(stop)
	wg.Wait()
	if err != nil {
		c.Cleanup()
		return nil, err
	}
	c.Result = res
	for n, p := range pruneFrames(dir, wanted, true) {
		c.Frames[n] = p
	}
	return c, nil
}

// pruneFrames удаляет кадры, которые не просили, и возвращает оставшиеся нужные.
// Пока запись идёт (final=false), самый новый файл может быть недописан, поэтому
// трогаем только кадры старше него.
func pruneFrames(dir string, wanted map[int]bool, final bool) map[int]string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	frames := map[int]string{}
	newest := -1
	for _, e := range entries {
		m := frameFileRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		frames[n] = filepath.Join(dir, e.Name())
		newest = max(newest, n)
	}
	kept := map[int]string{}
	for n, p := range frames {
		switch {
		case wanted[n]:
			kept[n] = p
		case final || n < newest:
			os.Remove(p)
		}
	}
	return kept
}

// MissingFrames перечисляет запрошенные кадры, которых нет в съёмке, —
// например, если игра вышла раньше или упала.
func (c *Capture) MissingFrames(requested []int) []int {
	var missing []int
	for _, f := range requested {
		if _, ok := c.Frames[f]; !ok {
			missing = append(missing, f)
		}
	}
	return missing
}

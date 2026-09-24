package godot

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	bufferLines    = 5000 // сколько последних строк храним на запуск
	maxRuns        = 16   // сколько завершённых запусков помним
	stopGrace      = 3 * time.Second
	RunStatusAlive = "running"
	RunStatusDone  = "exited"
)

// Line — строка вывода с порядковым номером, по которому агент
// запрашивает «всё новое с момента N».
type Line struct {
	Seq    int64  `json:"seq"`
	Stream string `json:"stream"` // "stdout" | "stderr"
	Text   string `json:"text"`
}

// Run — один запуск проекта.
type Run struct {
	ID        string
	Scene     string
	Args      []string
	StartedAt time.Time

	cmd    *exec.Cmd
	done   chan struct{}
	Bridge *Bridge // nil, если игра запущена без моста

	mu       sync.Mutex
	lines    []Line // кольцевой буфер фиксированного размера
	nextSeq  int64
	exitCode int
	exitErr  string
	endedAt  time.Time
}

// RunState — снимок состояния для инструментов.
type RunState struct {
	ID        string `json:"run_id"`
	Scene     string `json:"scene,omitempty"`
	Status    string `json:"status"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Error     string `json:"error,omitempty"`
	StartedAt string `json:"started_at"`
	Uptime    string `json:"uptime"`
	Bridge    string `json:"bridge,omitempty"` // connected | waiting | closed
}

// Runner управляет запусками проекта.
type Runner struct {
	g        *Godot
	mu       sync.Mutex
	runs     map[string]*Run
	order    []string
	override *overrideCfg
}

// runSeq нумерует запуски во всех проектах сразу: run_id уникален в процессе,
// и по нему можно найти проект.
var runSeq atomic.Int64

func NewRunner(g *Godot) *Runner {
	o := &overrideCfg{dir: g.ProjectDir}
	o.cleanupStale()
	return &Runner{g: g, runs: map[string]*Run{}, override: o}
}

// StartOptions — параметры запуска.
type StartOptions struct {
	Scene     string   // res://-путь сцены; пусто = главная сцена проекта
	Headless  bool     // без окна (нет рендеринга, но логика и физика работают)
	QuitAfter int      // выйти через N кадров (для смоук-тестов); 0 = не выходить
	Debug     bool     // --debug-collisions, --debug-navigation
	UserArgs  []string // аргументы игры после "--"
	Bridge    bool     // подключить мост для godot_game_* (автозагрузка через override.cfg)
}

// Start запускает проект и сразу возвращает управление.
func (r *Runner) Start(opt StartOptions) (*Run, error) {
	args := []string{"--path", r.g.ProjectDir}
	if opt.Headless {
		args = append(args, "--headless")
	}
	if opt.QuitAfter > 0 {
		args = append(args, "--quit-after", fmt.Sprint(opt.QuitAfter))
	}
	if opt.Debug {
		args = append(args, "--debug-collisions", "--debug-navigation")
	}
	if opt.Scene != "" {
		args = append(args, opt.Scene)
	}
	if len(opt.UserArgs) > 0 {
		args = append(append(args, "--"), opt.UserArgs...)
	}

	cmd := exec.Command(r.g.Bin, args...)
	cmd.Dir = r.g.ProjectDir
	setProcessGroup(cmd)

	var bridge *Bridge
	if opt.Bridge {
		b, err := newBridge()
		if err != nil {
			return nil, err
		}
		if err := r.override.acquire(); err != nil {
			b.Close()
			return nil, fmt.Errorf("cannot set up the bridge: %w", err)
		}
		bridge = b
		cmd.Env = append(os.Environ(), b.Env())
	}
	fail := func(err error) (*Run, error) {
		if bridge != nil {
			bridge.Close()
			r.override.release()
		}
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fail(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fail(err)
	}
	if err := cmd.Start(); err != nil {
		return fail(fmt.Errorf("failed to start godot: %w", err))
	}

	run := &Run{
		ID:        fmt.Sprintf("run-%d", runSeq.Add(1)),
		Scene:     opt.Scene,
		Args:      args,
		StartedAt: time.Now(),
		cmd:       cmd,
		done:      make(chan struct{}),
		Bridge:    bridge,
	}
	if bridge != nil {
		// override.cfg нужен только на старте: как только игра подключилась
		// (или вышла, или так и не подключилась), возвращаем файл как был.
		go func() {
			select {
			case <-bridge.Connected():
			case <-run.done:
			case <-time.After(30 * time.Second):
			}
			r.override.release()
		}()
	}

	// Пайпы нужно вычитывать постоянно: если буфер пайпа переполнится,
	// Godot заблокируется на print() и «зависнет».
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); run.consume(stdout, "stdout") }()
	go func() { defer wg.Done(); run.consume(stderr, "stderr") }()
	go func() {
		wg.Wait() // дочитываем вывод до Wait, иначе потеряем хвост
		err := cmd.Wait()
		run.mu.Lock()
		run.endedAt = time.Now()
		var ee *exec.ExitError
		switch {
		case errors.As(err, &ee):
			run.exitCode = ee.ExitCode()
		case err != nil:
			run.exitCode, run.exitErr = -1, err.Error()
		}
		run.mu.Unlock()
		close(run.done)
		if bridge != nil {
			bridge.Close()
		}
	}()

	r.mu.Lock()
	r.runs[run.ID] = run
	r.order = append(r.order, run.ID)
	r.evictLocked()
	r.mu.Unlock()
	return run, nil
}

// evictLocked забывает самые старые завершённые запуски.
func (r *Runner) evictLocked() {
	for len(r.order) > maxRuns {
		evicted := false
		for i, id := range r.order {
			if !r.runs[id].Alive() {
				delete(r.runs, id)
				r.order = append(r.order[:i], r.order[i+1:]...)
				evicted = true
				break
			}
		}
		if !evicted {
			return
		}
	}
}

func (run *Run) consume(rd io.Reader, stream string) {
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		text := strings.TrimRight(StripANSI(sc.Text()), "\r")
		run.mu.Lock()
		run.nextSeq++
		l := Line{Seq: run.nextSeq, Stream: stream, Text: text}
		if len(run.lines) < bufferLines {
			run.lines = append(run.lines, l)
		} else {
			copy(run.lines, run.lines[1:])
			run.lines[len(run.lines)-1] = l
		}
		run.mu.Unlock()
	}
}

// Alive — процесс ещё работает.
func (run *Run) Alive() bool {
	select {
	case <-run.done:
		return false
	default:
		return true
	}
}

// Wait ждёт завершения процесса не дольше d. Возвращает true, если он завершился.
func (run *Run) Wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-run.done:
		return true
	case <-t.C:
	case <-ctx.Done():
	}
	return false
}

// State возвращает снимок состояния запуска.
func (run *Run) State() RunState {
	run.mu.Lock()
	defer run.mu.Unlock()
	s := RunState{
		ID:        run.ID,
		Scene:     run.Scene,
		Status:    RunStatusAlive,
		StartedAt: run.StartedAt.Format(time.RFC3339),
	}
	end := time.Now()
	if !run.endedAt.IsZero() {
		code := run.exitCode
		s.Status, s.ExitCode, s.Error, end = RunStatusDone, &code, run.exitErr, run.endedAt
	}
	s.Uptime = end.Sub(run.StartedAt).Round(time.Millisecond).String()
	if run.Bridge != nil {
		switch {
		case run.Bridge.IsConnected():
			s.Bridge = "connected"
		case s.Status == RunStatusAlive:
			s.Bridge = "waiting"
		default:
			s.Bridge = "closed"
		}
	}
	return s
}

// Output возвращает строки с Seq > since (не больше max) и курсор для
// следующего вызова. dropped=true — часть строк уже вытеснена из буфера.
func (run *Run) Output(since int64, max int) (lines []Line, next int64, dropped bool) {
	run.mu.Lock()
	defer run.mu.Unlock()
	if len(run.lines) > 0 && run.lines[0].Seq > since+1 {
		dropped = true
	}
	next = since
	for _, l := range run.lines {
		if l.Seq <= since {
			continue
		}
		if max > 0 && len(lines) >= max {
			break
		}
		lines = append(lines, l)
		next = l.Seq
	}
	return lines, next, dropped
}

// Get находит запуск по id; пустой id означает последний запуск.
func (r *Runner) Get(id string) (*Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id == "" {
		if len(r.order) == 0 {
			return nil, errors.New("no runs yet; start one with godot_run_project")
		}
		id = r.order[len(r.order)-1]
	}
	run, ok := r.runs[id]
	if !ok {
		return nil, fmt.Errorf("unknown run_id %q", id)
	}
	return run, nil
}

// List возвращает все известные запуски, новые первыми.
func (r *Runner) List() []RunState {
	r.mu.Lock()
	ids := append([]string(nil), r.order...)
	r.mu.Unlock()
	out := make([]RunState, 0, len(ids))
	for i := len(ids) - 1; i >= 0; i-- {
		if run, err := r.Get(ids[i]); err == nil {
			out = append(out, run.State())
		}
	}
	return out
}

// Stop мягко останавливает запуск, а через stopGrace добивает SIGKILL.
func (r *Runner) Stop(ctx context.Context, id string) (*Run, error) {
	run, err := r.Get(id)
	if err != nil {
		return nil, err
	}
	if !run.Alive() {
		return run, nil
	}
	_ = killGroup(run.cmd, false)
	if !run.Wait(ctx, stopGrace) {
		_ = killGroup(run.cmd, true)
		run.Wait(ctx, stopGrace)
	}
	return run, nil
}

// StopAll вызывается при завершении MCP-сервера, чтобы не оставлять сирот.
func (r *Runner) StopAll(ctx context.Context) {
	for _, s := range r.List() {
		if s.Status == RunStatusAlive {
			_, _ = r.Stop(ctx, s.ID)
		}
	}
	r.override.forceRestore()
}

// Diagnostics для строк запуска.
func LinesDiagnostics(lines []Line) []Diagnostic {
	texts := make([]string, len(lines))
	for i, l := range lines {
		texts[i] = l.Text
	}
	return ParseDiagnostics(texts)
}

package lsp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/drgoshm/godot-mcp/internal/godot"
)

const (
	defaultIdle     = 10 * time.Minute
	defaultStart    = 90 * time.Second // первый запуск редактора импортирует проект
	diagnosticsWait = 15 * time.Second
)

// Checker проверяет GDScript через языковой сервер фонового редактора
// (godot --editor --headless --lsp-port N). Редактор запускается при первой
// проверке и останавливается после IdleTimeout без проверок.
//
// Редактор держит скрипты в памяти, поэтому перед каждой проверкой изменённые
// на диске .gd-файлы отправляются ему через didSave. Новый или удалённый
// class_name и изменения project.godot (автозагрузки) редактор без фокуса окна
// не подхватывает — тогда он перезапускается (~2 с).
type Checker struct {
	Bin          string
	ProjectDir   string
	IdleTimeout  time.Duration
	StartTimeout time.Duration

	mu   sync.Mutex // проверки и жизненный цикл редактора — по одной
	ed   *editor
	idle *time.Timer
}

type stamp struct {
	mod  time.Time
	size int64
}

type editor struct {
	cmd  *exec.Cmd
	done chan struct{}
	cl   *client
	root string // корень проекта с раскрытыми симлинками: так пути видит редактор
	out  *tailBuffer

	classes     string // class_name проекта на момент запуска
	projectMod  time.Time
	files       map[string]stamp // что редактор знает о .gd-файлах
	classByFile map[string]string
	versions    map[string]int // uri -> версия документа

	wmu     sync.Mutex
	waiters map[string]chan []lspDiagnostic
}

type lspDiagnostic struct {
	Message  string `json:"message"`
	Severity int    `json:"severity"`
	Range    struct {
		Start struct {
			Line int `json:"line"`
		} `json:"start"`
	} `json:"range"`
}

// Check проверяет скрипты (res://-пути) и возвращает диагностику по каждому.
func (c *Checker) Check(ctx context.Context, resPaths []string) (map[string][]godot.Diagnostic, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensure(ctx); err != nil {
		return nil, err
	}
	c.touch()
	ed := c.ed
	if err := ed.syncFiles(); err != nil {
		return nil, err
	}

	type pending struct {
		res string
		ch  chan []lspDiagnostic
	}
	var waits []pending
	for _, res := range resPaths {
		abs := ed.abs(res)
		text, err := os.ReadFile(abs)
		if err != nil {
			return nil, err
		}
		uri := fileURI(abs)
		ch := ed.wait(uri)
		if err := ed.update(uri, string(text)); err != nil {
			return nil, err
		}
		waits = append(waits, pending{res, ch})
	}

	out := map[string][]godot.Diagnostic{}
	timeout := time.NewTimer(diagnosticsWait)
	defer timeout.Stop()
	for _, w := range waits {
		select {
		case ds := <-w.ch:
			out[w.res] = convert(w.res, ds)
		case <-ed.done:
			c.ed = nil
			return nil, errors.New("the background editor exited during the check")
		case <-timeout.C:
			return nil, fmt.Errorf("no diagnostics from the language server for %s", w.res)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return out, nil
}

// Query выполняет запрос языкового сервера к документу resPath (hover,
// definition, references, documentSymbol...). Перед запросом документ и
// изменённые на диске скрипты синхронизируются, как при проверке.
// В params поле textDocument заполняется автоматически.
func (c *Checker) Query(ctx context.Context, resPath, method string, params map[string]any) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.QueryEach(ctx, []string{resPath}, method, params, func(_ string, res json.RawMessage) { out = res })
	return out, err
}

// QueryEach — тот же запрос к нескольким документам с одной синхронизацией
// (поиск символов по всему проекту). fn вызывается для каждого документа.
func (c *Checker) QueryEach(ctx context.Context, resPaths []string, method string, params map[string]any, fn func(resPath string, result json.RawMessage)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensure(ctx); err != nil {
		return err
	}
	c.touch()
	ed := c.ed
	if err := ed.syncFiles(); err != nil {
		return err
	}
	for _, res := range resPaths {
		abs := ed.abs(res)
		text, err := os.ReadFile(abs)
		if err != nil {
			return err
		}
		uri := fileURI(abs)
		if err := ed.update(uri, string(text)); err != nil {
			return err
		}
		p := map[string]any{"textDocument": map[string]any{"uri": uri}}
		for k, v := range params {
			p[k] = v
		}
		qctx, cancel := context.WithTimeout(ctx, diagnosticsWait)
		result, err := ed.cl.Request(method, p, qctx.Done())
		cancel()
		if err != nil {
			return fmt.Errorf("%s %s: %w", method, res, err)
		}
		fn(res, result)
	}
	return nil
}

// ResPath переводит URI из ответа языкового сервера в res://-путь; "" если он вне проекта.
func (c *Checker) ResPath(uri string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ed == nil {
		return ""
	}
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	p := filepath.FromSlash(u.Path)
	if len(p) > 2 && p[0] == filepath.Separator && p[2] == ':' { // /C:/x -> C:/x
		p = p[1:]
	}
	rel, err := filepath.Rel(c.ed.root, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	return "res://" + filepath.ToSlash(rel)
}

// Close останавливает фоновый редактор.
func (c *Checker) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.idle != nil {
		c.idle.Stop()
	}
	c.stopLocked()
}

// Running — запущен ли сейчас фоновый редактор.
func (c *Checker) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ed != nil && c.ed.alive()
}

func (c *Checker) touch() {
	idle := c.IdleTimeout
	if idle <= 0 {
		idle = defaultIdle
	}
	if c.idle != nil {
		c.idle.Stop()
	}
	c.idle = time.AfterFunc(idle, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.stopLocked()
	})
}

// ensure запускает редактор или перезапускает его, если он умер или
// изменилось то, что он не умеет подхватить сам.
func (c *Checker) ensure(ctx context.Context) error {
	if c.ed != nil {
		if !c.ed.alive() {
			c.ed = nil
		} else if reason := c.ed.stale(); reason != "" {
			c.stopLocked()
		}
	}
	if c.ed != nil {
		return nil
	}
	ed, err := c.start(ctx)
	if err != nil {
		return err
	}
	c.ed = ed
	return nil
}

func (c *Checker) stopLocked() {
	if c.ed == nil {
		return
	}
	ed := c.ed
	c.ed = nil
	ed.cl.Close()
	_ = godot.KillGroup(ed.cmd, false)
	select {
	case <-ed.done:
	case <-time.After(3 * time.Second):
		_ = godot.KillGroup(ed.cmd, true)
		<-ed.done
	}
}

func (c *Checker) start(ctx context.Context) (*editor, error) {
	root, err := filepath.EvalSymlinks(c.ProjectDir)
	if err != nil {
		return nil, err
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	// --language en: описания движка в подсказках — на английском, как и остальная справка,
	// а не на языке, выбранном в настройках редактора пользователя.
	cmd := exec.Command(c.Bin, "--editor", "--headless", "--language", "en", "--path", root, "--lsp-port", strconv.Itoa(port))
	cmd.Dir = root
	out := &tailBuffer{max: 4096}
	cmd.Stdout, cmd.Stderr = out, out
	if err := godot.StartGroup(cmd); err != nil {
		return nil, fmt.Errorf("cannot start the background editor: %w", err)
	}
	ed := &editor{cmd: cmd, done: make(chan struct{}), root: root, out: out,
		versions: map[string]int{}, waiters: map[string]chan []lspDiagnostic{}}
	go func() { _ = cmd.Wait(); close(ed.done) }()

	fail := func(err error) (*editor, error) {
		_ = godot.KillGroup(cmd, true)
		<-ed.done
		return nil, err
	}

	// Редактор сначала сканирует и импортирует проект, потом открывает порт.
	limit := c.StartTimeout
	if limit <= 0 {
		limit = defaultStart
	}
	deadline := time.Now().Add(limit)
	var conn net.Conn
	for {
		conn, err = net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
		if err == nil {
			break
		}
		select {
		case <-ed.done:
			return nil, fmt.Errorf("the background editor exited on start:\n%s", out.String())
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fail(fmt.Errorf("the background editor did not open its language server port within %s", limit))
		}
	}
	ed.cl = newClient(conn, ed.onNotify)

	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err = ed.cl.Request("initialize", map[string]any{
		"processId": os.Getpid(), "rootUri": fileURI(root), "capabilities": map[string]any{},
	}, initCtx.Done())
	if err != nil {
		ed.cl.Close()
		return fail(fmt.Errorf("language server initialize: %w", err))
	}
	if err := ed.cl.Notify("initialized", map[string]any{}); err != nil {
		ed.cl.Close()
		return fail(err)
	}
	// Всё, что лежит на диске сейчас, редактор прочитает сам.
	ed.files, ed.classByFile = scanScripts(root, nil, nil)
	ed.classes = classSet(ed.classByFile)
	ed.projectMod = modTime(filepath.Join(root, "project.godot"))
	return ed, nil
}

func (ed *editor) abs(res string) string {
	return filepath.Join(ed.root, filepath.FromSlash(strings.TrimPrefix(res, "res://")))
}

func (ed *editor) alive() bool {
	select {
	case <-ed.done:
		return false
	default:
		return true
	}
}

// stale — причина перезапуска или "".
func (ed *editor) stale() string {
	if !modTime(filepath.Join(ed.root, "project.godot")).Equal(ed.projectMod) {
		return "project.godot changed"
	}
	_, classes := scanScripts(ed.root, ed.files, ed.classByFile)
	if classSet(classes) != ed.classes {
		return "class_name set changed"
	}
	return ""
}

// syncFiles отправляет редактору .gd-файлы, изменённые на диске с прошлого раза.
func (ed *editor) syncFiles() error {
	files, classes := scanScripts(ed.root, ed.files, ed.classByFile)
	for path, st := range files {
		if old, ok := ed.files[path]; ok && old == st {
			continue
		}
		text, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		// didSave с текстом перечитывает скрипт в кеше редактора — зависимые
		// файлы увидят новые типы и сигнатуры.
		uri := fileURI(path)
		if err := ed.cl.Notify("textDocument/didSave", map[string]any{
			"textDocument": map[string]any{"uri": uri}, "text": string(text),
		}); err != nil {
			return err
		}
		// Открытый документ живёт своим текстом: didSave его не меняет, и
		// переход к определению показывал бы старые строки.
		if ed.versions[uri] > 0 {
			if err := ed.update(uri, string(text)); err != nil {
				return err
			}
		}
	}
	ed.files, ed.classByFile = files, classes
	return nil
}

// update открывает документ или отправляет его новый текст.
func (ed *editor) update(uri, text string) error {
	v := ed.versions[uri] + 1
	ed.versions[uri] = v
	if v == 1 {
		return ed.cl.Notify("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{"uri": uri, "languageId": "gdscript", "version": v, "text": text},
		})
	}
	return ed.cl.Notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": v},
		"contentChanges": []any{map[string]any{"text": text}},
	})
}

func (ed *editor) wait(uri string) chan []lspDiagnostic {
	ch := make(chan []lspDiagnostic, 1)
	ed.wmu.Lock()
	ed.waiters[uri] = ch
	ed.wmu.Unlock()
	return ch
}

func (ed *editor) onNotify(method string, params json.RawMessage) {
	if method != "textDocument/publishDiagnostics" {
		return
	}
	var p struct {
		URI         string          `json:"uri"`
		Diagnostics []lspDiagnostic `json:"diagnostics"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	ed.wmu.Lock()
	ch := ed.waiters[p.URI]
	delete(ed.waiters, p.URI)
	ed.wmu.Unlock()
	if ch != nil {
		ch <- p.Diagnostics
	}
}

// convert: LSP -> формат check_script. Предупреждения приходят как
// "(UNUSED_VARIABLE): текст" — код оставляем в сообщении.
func convert(res string, ds []lspDiagnostic) []godot.Diagnostic {
	out := []godot.Diagnostic{}
	for _, d := range ds {
		sev := "error"
		if d.Severity >= 2 {
			sev = "warning"
		}
		out = append(out, godot.Diagnostic{Severity: sev, Kind: "script", Message: d.Message, File: res, Line: d.Range.Start.Line + 1})
	}
	return out
}

// ---- файлы проекта ----

var classNameRe = regexp.MustCompile(`(?m)^\s*class_name\s+([A-Za-z_][A-Za-z0-9_]*)`)

// scanScripts обходит .gd-файлы проекта. class_name перечитывается только
// у изменившихся файлов (prev/prevClasses — прошлый обход).
func scanScripts(root string, prev map[string]stamp, prevClasses map[string]string) (map[string]stamp, map[string]string) {
	files := map[string]stamp{}
	classes := map[string]string{}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if n := d.Name(); n == ".godot" || n == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".gd") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		st := stamp{info.ModTime(), info.Size()}
		files[p] = st
		if old, ok := prev[p]; ok && old == st {
			if c, ok := prevClasses[p]; ok {
				classes[p] = c
			}
			return nil
		}
		if data, err := os.ReadFile(p); err == nil {
			if m := classNameRe.FindSubmatch(data); m != nil {
				classes[p] = string(m[1])
			}
		}
		return nil
	})
	return files, classes
}

func classSet(byFile map[string]string) string {
	var names []string
	for p, c := range byFile {
		names = append(names, c+"@"+p)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

func modTime(p string) time.Time {
	if info, err := os.Stat(p); err == nil {
		return info.ModTime()
	}
	return time.Time{}
}

// fileURI: /a/b c.gd -> file:///a/b%20c.gd; C:\a\b.gd -> file:///C:/a/b.gd.
func fileURI(abs string) string {
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// tailBuffer хранит последние max байт вывода редактора — для сообщений об ошибках.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(bytes.TrimSpace(godot.StripANSIBytes(t.buf)))
}

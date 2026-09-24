package godot

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed gdscript/mcp_bridge.gd
var bridgeGD string

const (
	// Скрипт моста лежит в .godot/: каталог служебный, скрыт в редакторе и в git.
	bridgeScriptRes = "res://.godot/godot_mcp_bridge.gd"
	bridgeEnv       = "GODOT_MCP_BRIDGE"
	// Помечает наш кусок override.cfg, чтобы убрать его и после аварийного выхода.
	overrideMarker = "; godot-mcp bridge (temporary, safe to delete)"
	helloTimeout   = 5 * time.Second
)

// Bridge — TCP-сервер для одного запуска игры. Игра подключается к нему
// сама (автозагрузка mcp_bridge.gd) и выполняет команды.
type Bridge struct {
	ln    net.Listener
	token string

	connected chan struct{}
	closed    chan struct{}
	closeOnce sync.Once

	mu      sync.Mutex
	conn    net.Conn
	nextID  int64
	pending map[int64]chan bridgeReply
}

type bridgeReply struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
	ID     float64         `json:"id"` // float: числа из GDScript-JSON бывают вида 1.0
}

func newBridge() (*Bridge, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	tok := make([]byte, 16)
	if _, err := rand.Read(tok); err != nil {
		ln.Close()
		return nil, err
	}
	b := &Bridge{ln: ln, token: hex.EncodeToString(tok), connected: make(chan struct{}),
		closed: make(chan struct{}), pending: map[int64]chan bridgeReply{}}
	go b.accept()
	return b, nil
}

// Env — переменная окружения для процесса игры.
func (b *Bridge) Env() string {
	return fmt.Sprintf("%s=%d:%s", bridgeEnv, b.ln.Addr().(*net.TCPAddr).Port, b.token)
}

// Connected закрывается, когда игра подключилась и прошла проверку токена.
func (b *Bridge) Connected() <-chan struct{} { return b.connected }

// IsConnected — подключена ли игра сейчас.
func (b *Bridge) IsConnected() bool {
	select {
	case <-b.closed:
		return false
	case <-b.connected:
		return true
	default:
		return false
	}
}

// accept ждёт подключения игры. Чужие подключения (без токена) отбрасываются.
func (b *Bridge) accept() {
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			return
		}
		r := bufio.NewReader(conn)
		conn.SetReadDeadline(time.Now().Add(helloTimeout))
		line, err := r.ReadBytes('\n')
		var hello struct {
			Hello string `json:"hello"`
		}
		if err != nil || json.Unmarshal(line, &hello) != nil || hello.Hello != b.token {
			conn.Close()
			continue
		}
		conn.SetReadDeadline(time.Time{})
		b.mu.Lock()
		b.conn = conn
		b.mu.Unlock()
		b.ln.Close() // второй игры на этом мосту не будет
		close(b.connected)
		b.read(r)
		return
	}
}

func (b *Bridge) read(r *bufio.Reader) {
	defer b.Close()
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var rep bridgeReply
		if json.Unmarshal(bytes.TrimSpace(line), &rep) != nil {
			continue
		}
		b.mu.Lock()
		id := int64(rep.ID)
		ch := b.pending[id]
		delete(b.pending, id)
		b.mu.Unlock()
		if ch != nil {
			ch <- rep
		}
	}
}

// ErrNoBridge — игра не подключилась к мосту или уже отключилась.
var ErrNoBridge = errors.New("the game is not connected to the bridge")

// Call отправляет команду игре и ждёт ответа. Сначала ждёт подключения:
// сразу после запуска игра ещё загружается.
func (b *Bridge) Call(ctx context.Context, cmd string, args map[string]any, timeout time.Duration) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case <-b.connected:
	case <-b.closed:
		return nil, ErrNoBridge
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: it did not connect in time (is the game still loading, or did it crash?)", ErrNoBridge)
	}

	b.mu.Lock()
	if b.conn == nil {
		b.mu.Unlock()
		return nil, ErrNoBridge
	}
	b.nextID++
	id := b.nextID
	ch := make(chan bridgeReply, 1)
	b.pending[id] = ch
	msg := map[string]any{}
	for k, v := range args {
		msg[k] = v
	}
	msg["id"], msg["cmd"] = id, cmd
	data, err := json.Marshal(msg)
	if err == nil {
		_, err = b.conn.Write(append(data, '\n'))
	}
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}

	select {
	case rep := <-ch:
		if !rep.OK {
			return nil, errors.New(rep.Error)
		}
		return rep.Result, nil
	case <-b.closed:
		return nil, fmt.Errorf("%w: the game exited while running %s", ErrNoBridge, cmd)
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return nil, fmt.Errorf("the game did not answer %s within %s (is it frozen in a long loop?)", cmd, timeout)
	}
}

// Close закрывает мост (при выходе игры).
func (b *Bridge) Close() {
	b.closeOnce.Do(func() {
		close(b.closed)
		b.ln.Close()
		b.mu.Lock()
		if b.conn != nil {
			b.conn.Close()
		}
		b.mu.Unlock()
	})
}

// ---- override.cfg ----

// overrideCfg временно добавляет автозагрузку моста в override.cfg проекта.
// Godot читает этот файл при старте поверх project.godot. Несколько запусков
// подряд делят одну запись, поэтому ведём счётчик; когда последняя игра
// подключилась (или вышла), файл возвращается в исходное состояние.
type overrideCfg struct {
	dir string

	mu       sync.Mutex
	refs     int
	original []byte // nil — файла не было
}

func (o *overrideCfg) path() string { return filepath.Join(o.dir, "override.cfg") }

func (o *overrideCfg) acquire() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.refs == 0 {
		script := filepath.Join(o.dir, ".godot", "godot_mcp_bridge.gd")
		if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(script, []byte(bridgeGD), 0o644); err != nil {
			return err
		}
		orig, err := os.ReadFile(o.path())
		switch {
		case err == nil:
			o.original = stripOverride(orig)
			if bytes.Contains(orig, []byte(overrideMarker)) && len(bytes.TrimSpace(o.original)) == 0 {
				o.original = nil // файл был только нашим остатком
			}
		case os.IsNotExist(err):
			o.original = nil
		default:
			return err
		}
		// Секция [autoload] в конце файла: повторная секция в ConfigFile
		// дополняет существующую, автозагрузки пользователя остаются.
		content := append(bytes.Clone(o.original), []byte(fmt.Sprintf(
			"\n%s\n[autoload]\n\nGodotMcpBridge=\"*%s\"\n", overrideMarker, bridgeScriptRes))...)
		if err := os.WriteFile(o.path(), content, 0o644); err != nil {
			return err
		}
	}
	o.refs++
	return nil
}

func (o *overrideCfg) release() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.refs == 0 {
		return
	}
	o.refs--
	if o.refs == 0 {
		o.restore()
	}
}

// forceRestore возвращает файл при остановке сервера, даже если игры ещё не подключились.
func (o *overrideCfg) forceRestore() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.refs > 0 {
		o.refs = 0
		o.restore()
	}
}

func (o *overrideCfg) restore() {
	if o.original == nil {
		os.Remove(o.path())
	} else {
		os.WriteFile(o.path(), o.original, 0o644)
	}
}

// cleanupStale убирает нашу запись, оставшуюся после аварийного выхода сервера.
func (o *overrideCfg) cleanupStale() {
	data, err := os.ReadFile(o.path())
	if err != nil || !bytes.Contains(data, []byte(overrideMarker)) {
		return
	}
	if rest := stripOverride(data); len(bytes.TrimSpace(rest)) == 0 {
		os.Remove(o.path())
	} else {
		os.WriteFile(o.path(), rest, 0o644)
	}
}

// stripOverride отрезает нашу секцию: она всегда дописывается в конец после маркера.
func stripOverride(data []byte) []byte {
	if i := bytes.Index(data, []byte(overrideMarker)); i >= 0 {
		return []byte(strings.TrimRight(string(data[:i]), "\n") + "\n")
	}
	return data
}

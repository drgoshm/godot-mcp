// Package lsp держит фоновый headless-редактор Godot и проверяет GDScript
// через его языковой сервер. Проверка файла занимает миллисекунды вместо
// ~0,4 с на запуск отдельного процесса с --check-only.
package lsp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
)

// client — JSON-RPC 2.0 поверх TCP с заголовками Content-Length (транспорт LSP).
type client struct {
	conn net.Conn

	wmu sync.Mutex // запись сообщений целиком

	mu      sync.Mutex
	nextID  int
	pending map[int]chan rpcMessage
	notify  func(method string, params json.RawMessage)
	closed  chan struct{}
	err     error
}

type rpcMessage struct {
	ID     *int            `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func newClient(conn net.Conn, notify func(string, json.RawMessage)) *client {
	c := &client{conn: conn, pending: map[int]chan rpcMessage{}, notify: notify, closed: make(chan struct{})}
	go c.readLoop()
	return c
}

func (c *client) write(msg map[string]any) error {
	msg["jsonrpc"] = "2.0"
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = fmt.Fprintf(c.conn, "Content-Length: %d\r\n\r\n%s", len(body), body)
	return err
}

// Notify отправляет уведомление (без ответа).
func (c *client) Notify(method string, params any) error {
	return c.write(map[string]any{"method": method, "params": params})
}

// Request отправляет запрос и ждёт ответа.
func (c *client) Request(method string, params any, done <-chan struct{}) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan rpcMessage, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return nil, errors.New(m.Error.Message)
		}
		return m.Result, nil
	case <-c.closed:
		return nil, c.closeErr()
	case <-done:
		return nil, errors.New("timed out waiting for the language server")
	}
}

func (c *client) readLoop() {
	r := bufio.NewReader(c.conn)
	for {
		msg, err := readMessage(r)
		if err != nil {
			c.mu.Lock()
			c.err = err
			c.mu.Unlock()
			close(c.closed)
			return
		}
		var m rpcMessage
		if json.Unmarshal(msg, &m) != nil {
			continue
		}
		switch {
		case m.ID != nil && m.Method == "":
			c.mu.Lock()
			ch := c.pending[*m.ID]
			delete(c.pending, *m.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		case m.Method != "" && m.ID == nil:
			if c.notify != nil {
				c.notify(m.Method, m.Params)
			}
		}
	}
}

func (c *client) closeErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil && !errors.Is(c.err, io.EOF) {
		return fmt.Errorf("language server connection lost: %w", c.err)
	}
	return errors.New("language server connection closed")
}

func (c *client) Close() { c.conn.Close() }

// readMessage читает одно сообщение: заголовки до пустой строки, затем тело.
func readMessage(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if v, ok := strings.CutPrefix(line, "Content-Length:"); ok {
			length, err = strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return nil, fmt.Errorf("bad Content-Length %q", v)
			}
		}
	}
	if length < 0 {
		return nil, errors.New("message without Content-Length")
	}
	body := make([]byte, length)
	_, err := io.ReadFull(r, body)
	return body, err
}

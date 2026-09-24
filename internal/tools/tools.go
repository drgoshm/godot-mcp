// Package tools регистрирует MCP-инструменты. Каждый обработчик — тонкий
// слой: разбор аргументов -> вызов project/godot -> структурированный ответ.
// Ошибки, возвращённые обработчиком, SDK превращает в результат с isError=true,
// поэтому тексты ошибок написаны так, чтобы агент понял, что делать дальше.
package tools

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/drgoshm/godot-mcp/internal/godot"
	"github.com/drgoshm/godot-mcp/internal/project"
)

// Deps — всё, что нужно инструментам.
type Deps struct {
	Sandbox *project.Sandbox
	Godot   *godot.Godot
	Runner  *godot.Runner
	Version string // версия движка, для project_info
}

// Register добавляет все инструменты на сервер.
func Register(s *mcp.Server, d *Deps) {
	registerFileTools(s, d)
	registerEngineTools(s, d)
	registerRunTools(s, d)
}

func readOnly() *mcp.ToolAnnotations { return &mcp.ToolAnnotations{ReadOnlyHint: true} }

func boolPtr(b bool) *bool { return &b }

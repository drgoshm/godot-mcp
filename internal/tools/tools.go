// Package tools регистрирует MCP-инструменты. Каждый обработчик — тонкий
// слой: разбор аргументов -> вызов project/godot -> структурированный ответ.
// Ошибки, возвращённые обработчиком, SDK превращает в результат с isError=true,
// поэтому тексты ошибок написаны так, чтобы агент понял, что делать дальше.
package tools

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/drgoshm/godot-mcp/internal/docs"
	"github.com/drgoshm/godot-mcp/internal/godot"
	"github.com/drgoshm/godot-mcp/internal/project"
)

// Deps — всё, что нужно инструментам.
type Deps struct {
	Sandbox *project.Sandbox
	Godot   *godot.Godot
	Runner  *godot.Runner
	Version string       // версия движка, для project_info
	Docs    *docs.Loader // справка по API; по умолчанию кеш в каталоге пользователя
}

// Register добавляет все инструменты на сервер.
func Register(s *mcp.Server, d *Deps) {
	if d.Docs == nil {
		d.Docs = &docs.Loader{Bin: d.Godot.Bin, Version: d.Version}
	}
	registerFileTools(s, d)
	registerEngineTools(s, d)
	registerSceneTools(s, d)
	registerScreenshotTools(s, d)
	registerTestTools(s, d)
	registerDocsTools(s, d)
	registerGameTools(s, d)
	registerRunTools(s, d)
}

func readOnly() *mcp.ToolAnnotations { return &mcp.ToolAnnotations{ReadOnlyHint: true} }

func boolPtr(b bool) *bool { return &b }

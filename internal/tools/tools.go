// Package tools регистрирует MCP-инструменты. Каждый обработчик — тонкий
// слой: разбор аргументов -> вызов project/godot -> структурированный ответ.
// Ошибки, возвращённые обработчиком, SDK превращает в результат с isError=true,
// поэтому тексты ошибок написаны так, чтобы агент понял, что делать дальше.
package tools

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/drgoshm/godot-mcp/internal/docs"
	"github.com/drgoshm/godot-mcp/internal/godot"
	"github.com/drgoshm/godot-mcp/internal/lsp"
	"github.com/drgoshm/godot-mcp/internal/project"
)

// Deps — один открытый проект: всё, что нужно инструментам для работы с ним.
type Deps struct {
	Sandbox *project.Sandbox
	Godot   *godot.Godot
	Runner  *godot.Runner
	Version string       // версия движка, для project_info
	Docs    *docs.Loader // справка по API; по умолчанию кеш в каталоге пользователя
	LSP     *lsp.Checker // фоновый редактор для check_script; nil — только --check-only
}

// Register добавляет все инструменты на сервер.
func Register(s *mcp.Server, w *Workspace) {
	if w.Docs == nil {
		w.Docs = &docs.Loader{Bin: w.Bin, Version: w.Version}
	}
	registerProjectTools(s, w)
	registerFileTools(s, w)
	registerEngineTools(s, w)
	registerSceneTools(s, w)
	registerScreenshotTools(s, w)
	registerTestTools(s, w)
	registerDocsTools(s, w)
	registerGameTools(s, w)
	registerCodeTools(s, w)
	registerRunTools(s, w)
}

func readOnly() *mcp.ToolAnnotations { return &mcp.ToolAnnotations{ReadOnlyHint: true} }

func boolPtr(b bool) *bool { return &b }

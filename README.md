# godot-mcp

MCP-сервер для Godot 4.x. Даёт агенту читать и править файлы проекта, проверять GDScript, собирать сцены средствами самого движка и запускать игру с разбором ошибок.

## Сборка

Требуется Go 1.25+ (этого требует официальный `github.com/modelcontextprotocol/go-sdk`).

```sh
go mod tidy
make build            # -> bin/godot-mcp
make test             # юнит-тесты
make test-godot       # интеграционные тесты с настоящим движком (GODOT_BIN)
```

## Подключение к клиенту

Claude Desktop / Claude Code (`.mcp.json` или `claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "godot": {
      "command": "/path/to/bin/godot-mcp",
      "args": ["--project", "/path/to/my-game"],
      "env": { "GODOT_BIN": "/Applications/Godot.app/Contents/MacOS/Godot" }
    }
  }
}
```

Бинарник Godot ищется так: `--godot` → `$GODOT_BIN` → `godot`/`godot4` в PATH → `/Applications/Godot.app`.

## Инструменты

| Инструмент | Что делает |
|---|---|
| `godot_project_info` | имя, главная сцена, фичи движка, autoload'ы, действия ввода |
| `godot_list_files` | листинг проекта (без `.godot/`, `*.import`, `*.uid`) |
| `godot_read_file` | чтение текстового файла, опционально диапазон строк |
| `godot_write_file` | атомарная запись файла |
| `godot_edit_file` | замена уникального фрагмента (в стиле str_replace) |
| `godot_check_script` | `--check-only` для одного или нескольких `.gd`, ошибки с файлом и строкой |
| `godot_import` | headless-импорт ассетов и обновление кеша `class_name` |
| `godot_create_scene` | сборка `.tscn` из JSON-дерева узлов через `PackedScene` + `ResourceSaver` |
| `godot_run_script` | одноразовый GDScript (`extends SceneTree`) внутри проекта |
| `godot_run_project` | запуск игры или сцены в фоне, возвращает `run_id` и первые секунды вывода |
| `godot_get_output` | новый вывод по курсору `since`, long-poll, разобранные ошибки |
| `godot_stop_project` | SIGTERM → SIGKILL всей группы процессов |
| `godot_list_runs` | последние запуски и их статус |

## Безопасность

Файловые инструменты работают только внутри проекта. Запрещены абсолютные пути, `..`, симлинки наружу, а также `.godot/` и `.git/`. При этом `godot_run_script` и запуск игры исполняют произвольный код с правами пользователя. Это осознанный компромисс: для агента, который пишет игру, так работает любой запуск проекта.

## Что дальше

- Скриншоты: autoload, который по запросу сохраняет `get_viewport().get_texture().get_image()` в PNG (работает только без `--headless`).
- `run_tests` для GUT / gdUnit4.
- Диагностика через GDScript LSP редактора (порт 6005).
- Мост в открытый редактор (EditorPlugin + WebSocket) для работы с живым деревом сцены и undo/redo.

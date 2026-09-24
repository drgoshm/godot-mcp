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
| `godot_create_scene` | сборка `.tscn` из JSON-дерева узлов через `PackedScene` + `ResourceSaver`, со встроенными подресурсами и сигналами |
| `godot_scene_tree` | сцена в JSON в том же формате, что принимает `godot_create_scene`: узлы, изменённые свойства, инстансы, соединения |
| `godot_edit_scene` | атомарная правка существующей сцены списком операций с сохранением UID |
| `godot_run_script` | одноразовый GDScript (`extends SceneTree`) внутри проекта |
| `godot_run_project` | запуск игры или сцены в фоне, возвращает `run_id` и первые секунды вывода |
| `godot_get_output` | новый вывод по курсору `since`, long-poll, разобранные ошибки |
| `godot_stop_project` | SIGTERM → SIGKILL всей группы процессов |
| `godot_list_runs` | последние запуски и их статус |

### Встроенные подресурсы в `godot_create_scene`

Свойству-ресурсу можно передать не только путь `res://`, но и объект с ключом `_type`. Такой ресурс создаётся движком и сохраняется внутри `.tscn` как `sub_resource`:

```json
{"type": "CollisionShape2D", "name": "Shape", "properties": {
  "shape": {"_type": "RectangleShape2D", "size": "Vector2(32, 48)"}
}}
```

Остальные ключи объекта — свойства ресурса, по тем же правилам, что и у узлов, поэтому вложенность работает (`GradientTexture2D` → `Gradient`). В `_type` можно указать `class_name` пользовательского ресурса (после `godot_import`) или передать `"_script": "res://item.gd"`. Типизированные массивы вроде `Array[ItemData]` принимают списки таких объектов. Ошибки говорят, что ожидалось: `CollisionShape2D.shape: cannot assign Gradient, the property expects Shape2D`.

### Сигналы

Соединения задаются списком `connections` рядом с `root`, пути узлов — относительно корня сцены (`"."` — сам корень):

```json
"connections": [
  {"from": "UI/Start", "signal": "pressed", "to": ".", "method": "_on_start_pressed"},
  {"from": "Timer", "signal": "timeout", "to": ".", "method": "_on_timeout", "binds": ["tick"], "flags": 1}
]
```

Метод должен уже существовать в скрипте получателя. Узлы, сигнал, метод и число аргументов (аргументы сигнала + `binds` против обязательных и необязательных параметров метода) проверяются при сборке. Поэтому ошибка вроде `UI/Sound:toggled -> .:_on_start_pressed: the method takes 0 arguments, but the signal passes 1` приходит сразу, а не во время игры. `flags` — это `CONNECT_*`: 1 — отложенный вызов, 4 — однократный.

### Правка существующих сцен

`godot_scene_tree` читает сцену через `SceneState` — ровно то, что лежит в файле, — и отдаёт её в формате `godot_create_scene`, так что прочитанное дерево можно сразу пересобрать. У каждого узла есть `path` относительно корня. Слишком большие значения (например, данные тайлов) заменяются заглушкой `{"_omitted": ...}`, записать её обратно нельзя.

`godot_edit_scene` применяет операции по порядку: `add_node`, `remove_node`, `set_properties`, `rename`, `move`, `groups`, `connect`, `disconnect`. Если хоть одна не удалась, файл не меняется, а ошибка называет операцию: `operations[1] remove_node: no node at 'Nope'`. UID сцены, `unique_id` узлов и id внешних ресурсов сохраняются, поэтому diff содержит только сделанные изменения. Узлы внутри инстанцированной сцены здесь не правятся — ошибка подскажет, какую сцену открыть.

## Безопасность

Файловые инструменты работают только внутри проекта. Запрещены абсолютные пути, `..`, симлинки наружу, а также `.godot/` и `.git/`. При этом `godot_run_script` и запуск игры исполняют произвольный код с правами пользователя. Это осознанный компромисс: для агента, который пишет игру, так работает любой запуск проекта.

## Что дальше

- Скриншоты: autoload, который по запросу сохраняет `get_viewport().get_texture().get_image()` в PNG (работает только без `--headless`).
- `run_tests` для GUT / gdUnit4.
- Диагностика через GDScript LSP редактора (порт 6005).
- Мост в открытый редактор (EditorPlugin + WebSocket) для работы с живым деревом сцены и undo/redo.

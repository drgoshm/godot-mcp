## Мост между запущенной игрой и godot-mcp. Подключается автозагрузкой через
## override.cfg только на время запуска; без переменной окружения
## GODOT_MCP_BRIDGE="порт:токен" ничего не делает и удаляет себя, поэтому
## забытый override.cfg не влияет на запуск из редактора.
##
## Протокол: TCP 127.0.0.1, по строке JSON на сообщение.
##   игра -> сервер: {"hello": token}
##   сервер -> игра: {"id": 1, "cmd": "tree" | "eval" | "set" | "input" | "screenshot", ...}
##   игра -> сервер: {"id": 1, "ok": true, "result": ...} или {"id": 1, "ok": false, "error": "..."}
##
## Скрипт выполняется с настройками проекта пользователя, где предупреждения
## могут быть ошибками, поэтому он не даёт ни одного предупреждения GDScript.
extends Node

const MAX_NODES: int = 500
const MAX_VALUE_CHARS: int = 400
const MAX_COLLECTION_ITEMS: int = 100

var _peer: StreamPeerTCP = null
var _token: String = ""
var _hello_sent: bool = false
var _buffer: PackedByteArray = PackedByteArray()


func _ready() -> void:
	var spec: String = OS.get_environment("GODOT_MCP_BRIDGE")
	var parts: PackedStringArray = spec.split(":")
	if parts.size() != 2:
		queue_free()
		return
	_token = parts[1]
	# Мост должен отвечать и на паузе.
	process_mode = Node.PROCESS_MODE_ALWAYS
	_peer = StreamPeerTCP.new()
	var err: Error = _peer.connect_to_host("127.0.0.1", parts[0].to_int())
	if err != OK:
		push_warning("godot-mcp bridge: cannot connect: " + error_string(err))
		_peer = null
		queue_free()


func _process(_delta: float) -> void:
	if _peer == null:
		return
	var poll_err: Error = _peer.poll()
	var status: StreamPeerTCP.Status = _peer.get_status()
	if poll_err != OK or status == StreamPeerTCP.STATUS_ERROR or status == StreamPeerTCP.STATUS_NONE:
		_peer = null  # сервер ушёл — игра продолжает работать без моста
		return
	if status != StreamPeerTCP.STATUS_CONNECTED:
		return
	if not _hello_sent:
		_hello_sent = true
		_send({"hello": _token})
	var available: int = _peer.get_available_bytes()
	if available <= 0:
		return
	var chunk: Array = _peer.get_partial_data(available)
	var data: PackedByteArray = chunk[1]
	_buffer.append_array(data)
	var newline: int = _buffer.find(10)
	while newline >= 0:
		var line: String = _buffer.slice(0, newline).get_string_from_utf8()
		_buffer = _buffer.slice(newline + 1)
		# Корутина без await: команды с ожиданием не должны задерживать чтение.
		_handle.call_deferred(line)
		newline = _buffer.find(10)


func _send(message: Dictionary) -> void:
	if _peer == null:
		return
	var err: Error = _peer.put_data((JSON.stringify(message) + "\n").to_utf8_buffer())
	if err != OK:
		_peer = null


## Выполняет команду. Команды с ожиданием (input, screenshot) — корутины,
## поэтому ответы могут приходить не по порядку; их различают по id.
func _handle(line: String) -> void:
	var parsed: Variant = JSON.parse_string(line)
	if typeof(parsed) != TYPE_DICTIONARY:
		return
	var req: Dictionary = parsed
	var reply: Dictionary = {}
	match str(req.get("cmd", "")):
		"tree":
			reply = _cmd_tree(req)
		"eval":
			reply = _cmd_eval(req)
		"set":
			reply = _cmd_set(req)
		"input":
			reply = await _cmd_input(req)
		"screenshot":
			reply = await _cmd_screenshot()
		_:
			reply = _fail("unknown command")
	# JSON.parse_string делает все числа float; id возвращаем целым.
	reply["id"] = _int(req.get("id", 0))
	_send(reply)


func _ok(result: Variant) -> Dictionary:
	return {"ok": true, "result": result}


func _fail(message: String) -> Dictionary:
	return {"ok": false, "error": message}


## Узел по абсолютному пути ("/root/Main/Player") или относительно текущей сцены ("Player").
func _find(path: String) -> Node:
	var scene: Node = get_tree().current_scene
	if path.is_empty() or path == ".":
		return scene if scene != null else get_tree().root
	if path.begins_with("/"):
		return get_tree().root.get_node_or_null(NodePath(path))
	if scene == null:
		return null
	return scene.get_node_or_null(NodePath(path))


# ---- tree ----

func _cmd_tree(req: Dictionary) -> Dictionary:
	var path: String = str(req.get("node", "/root"))
	var node: Node = _find(path)
	if node == null:
		return _fail("no node at '%s'" % path)
	var depth: int = _int(req.get("depth", 4))
	var props: Array = _array(req.get("properties"))
	var counter: Array[int] = [0]
	var scene: Node = get_tree().current_scene
	return _ok({
		"current_scene": str(scene.get_path()) if scene != null else "",
		"paused": get_tree().paused,
		"frame": Engine.get_process_frames(),
		"fps": Engine.get_frames_per_second(),
		"root": _describe_node(node, depth, props, counter),
		"truncated": counter[0] >= MAX_NODES,
	})


func _describe_node(node: Node, depth: int, props: Array, counter: Array[int]) -> Dictionary:
	counter[0] += 1
	var d: Dictionary = {"name": str(node.name), "type": node.get_class(), "path": str(node.get_path())}
	var script: Variant = node.get_script()
	if script is Script:
		var s: Script = script
		d["script"] = s.resource_path
	if not node.scene_file_path.is_empty():
		d["scene"] = node.scene_file_path
	if node is CanvasItem:
		var ci: CanvasItem = node
		if not ci.visible:
			d["visible"] = false
	if not props.is_empty():
		var values: Dictionary = {}
		for p: Variant in props:
			var prop_name: String = str(p)
			if prop_name in node:
				values[prop_name] = _to_json(node.get_indexed(NodePath(prop_name)), 0)
		if not values.is_empty():
			d["properties"] = values
	var children: Array = []
	for child: Node in node.get_children():
		if child == self:
			continue
		if depth <= 0 or counter[0] >= MAX_NODES:
			d["child_count"] = node.get_child_count()
			break
		children.append(_describe_node(child, depth - 1, props, counter))
	if not children.is_empty():
		d["children"] = children
	return d


# ---- eval ----

## Выражение Godot (класс Expression) с узлом как self: "position", "get_node('Gun').ammo",
## "velocity.length() > 10". Доступны переменные tree (SceneTree) и scene (текущая сцена).
func _cmd_eval(req: Dictionary) -> Dictionary:
	var node: Node = _find(str(req.get("node", "")))
	if node == null:
		return _fail("no node at '%s'" % str(req.get("node", "")))
	var expr: Expression = Expression.new()
	var text: String = str(req.get("expression", ""))
	var parse_err: Error = expr.parse(text, PackedStringArray(["tree", "scene"]))
	if parse_err != OK:
		return _fail("cannot parse expression: " + expr.get_error_text())
	var value: Variant = expr.execute([get_tree(), get_tree().current_scene], node, false)
	if expr.has_execute_failed():
		return _fail("expression failed: " + expr.get_error_text())
	return _ok({"value": _to_json(value, 0), "type": type_string(typeof(value)), "node": str(node.get_path())})


# ---- set ----

func _cmd_set(req: Dictionary) -> Dictionary:
	var node: Node = _find(str(req.get("node", "")))
	if node == null:
		return _fail("no node at '%s'" % str(req.get("node", "")))
	if typeof(req.get("properties")) != TYPE_DICTIONARY:
		return _fail("properties must be an object")
	var props: Dictionary = req["properties"]
	var result: Dictionary = {}
	for key: Variant in props.keys():
		var prop: String = str(key)
		# "position:x" меняет компонент; проверяем, что есть само свойство.
		if not (prop.get_slice(":", 0) in node):
			return _fail("%s has no property '%s'" % [node.get_path(), prop.get_slice(":", 0)])
		var current: Variant = node.get_indexed(NodePath(prop))
		var value: Variant = _convert(props[key], typeof(current))
		if value == null and props[key] != null:
			return _fail("%s: cannot convert %s to %s" % [prop, JSON.stringify(props[key]), type_string(typeof(current))])
		node.set_indexed(NodePath(prop), value)
		result[prop] = _to_json(node.get_indexed(NodePath(prop)), 0)
	return _ok(result)


## JSON -> значение того же типа, что у текущего: строки для не-строковых свойств
## разбираются как литералы Godot ("Vector2(1, 2)"), res://-пути загружаются.
func _convert(raw: Variant, target_type: int) -> Variant:
	if typeof(raw) == TYPE_STRING:
		var s: String = raw
		match target_type:
			TYPE_STRING, TYPE_STRING_NAME, TYPE_NODE_PATH:
				return s
			TYPE_OBJECT, TYPE_NIL:
				if s.begins_with("res://") or s.begins_with("uid://"):
					return load(s)
				if target_type == TYPE_NIL:
					var parsed: Variant = str_to_var(s)
					return parsed if parsed != null else s
				return null
		return str_to_var(s)
	if typeof(raw) == TYPE_FLOAT and target_type == TYPE_INT:
		var f: float = raw
		return int(f)
	return raw


# ---- input ----

## Шаги ввода по порядку. Каждый шаг — одно из:
##   {"action": "jump", "pressed": true, "strength": 1.0}
##   {"tap": "jump", "duration": 0.1}
##   {"key": "Space", "pressed": true}        (без pressed — нажать и отпустить)
##   {"text": "hello"}                          (набрать строку)
##   {"mouse_button": "left", "position": [x, y], "pressed": true}  (без pressed — клик)
##   {"mouse_move": [x, y]}
##   {"wait": 0.5} или {"wait_frames": 10}
func _cmd_input(req: Dictionary) -> Dictionary:
	var steps: Array = _array(req.get("steps"))
	for i: int in steps.size():
		if typeof(steps[i]) != TYPE_DICTIONARY:
			return _fail("steps[%d]: expected an object" % i)
		var step: Dictionary = steps[i]
		var err: String = await _input_step(step)
		if not err.is_empty():
			return _fail("steps[%d]: %s" % [i, err])
	# Даём кадр, чтобы игра успела обработать последние события.
	await get_tree().process_frame
	return _ok({"steps": steps.size(), "frame": Engine.get_process_frames()})


func _input_step(step: Dictionary) -> String:
	if step.has("action") or step.has("tap"):
		var action: String = str(step.get("action", step.get("tap", "")))
		if not InputMap.has_action(StringName(action)):
			return "unknown input action '%s'; defined: %s" % [action, ", ".join(_user_actions())]
		if step.has("tap"):
			_action(action, true, 1.0)
			await get_tree().create_timer(_float(step.get("duration", 0.1)), true).timeout
			_action(action, false, 0.0)
		else:
			_action(action, _bool(step.get("pressed", true)), _float(step.get("strength", 1.0)))
	elif step.has("key"):
		var keycode: Key = OS.find_keycode_from_string(str(step["key"]))
		if keycode == KEY_NONE:
			return "unknown key '%s' (use names like Space, Enter, Escape, A, Left, F1)" % str(step["key"])
		if step.has("pressed"):
			_key(keycode, _bool(step["pressed"]), 0)
		else:
			_key(keycode, true, 0)
			await get_tree().process_frame
			_key(keycode, false, 0)
	elif step.has("text"):
		for c: String in str(step["text"]):
			_key(KEY_NONE, true, c.unicode_at(0))
			_key(KEY_NONE, false, c.unicode_at(0))
			await get_tree().process_frame
	elif step.has("mouse_button"):
		var button: MouseButton = MOUSE_BUTTON_LEFT
		match str(step["mouse_button"]):
			"left":
				button = MOUSE_BUTTON_LEFT
			"right":
				button = MOUSE_BUTTON_RIGHT
			"middle":
				button = MOUSE_BUTTON_MIDDLE
			_:
				return "mouse_button must be left, right or middle"
		var pos: Vector2 = _vec2(step.get("position"))
		_mouse_move(pos)
		if step.has("pressed"):
			_mouse(button, pos, _bool(step["pressed"]))
		else:
			_mouse(button, pos, true)
			await get_tree().process_frame
			_mouse(button, pos, false)
	elif step.has("mouse_move"):
		_mouse_move(_vec2(step["mouse_move"]))
	elif step.has("wait"):
		await get_tree().create_timer(_float(step["wait"]), true).timeout
	elif step.has("wait_frames"):
		for _i: int in _int(step["wait_frames"]):
			await get_tree().process_frame
	else:
		return "unknown step; use action, tap, key, text, mouse_button, mouse_move, wait or wait_frames"
	return ""


func _action(action: String, pressed: bool, strength: float) -> void:
	# InputEventAction и меняет состояние Input, и доходит до _input()/_unhandled_input().
	var ev: InputEventAction = InputEventAction.new()
	ev.action = StringName(action)
	ev.pressed = pressed
	ev.strength = strength
	Input.parse_input_event(ev)


func _key(keycode: Key, pressed: bool, unicode: int) -> void:
	var ev: InputEventKey = InputEventKey.new()
	ev.keycode = keycode
	ev.physical_keycode = keycode
	ev.unicode = unicode
	ev.pressed = pressed
	Input.parse_input_event(ev)


func _mouse(button: MouseButton, pos: Vector2, pressed: bool) -> void:
	var ev: InputEventMouseButton = InputEventMouseButton.new()
	ev.button_index = button
	ev.position = pos
	ev.global_position = pos
	ev.pressed = pressed
	Input.parse_input_event(ev)


func _mouse_move(pos: Vector2) -> void:
	var ev: InputEventMouseMotion = InputEventMouseMotion.new()
	ev.position = pos
	ev.global_position = pos
	Input.parse_input_event(ev)


func _user_actions() -> PackedStringArray:
	var out: Array[String] = []
	for a: StringName in InputMap.get_actions():
		if not str(a).begins_with("ui_"):
			out.append(str(a))
	return PackedStringArray(out)


# ---- screenshot ----

func _cmd_screenshot() -> Dictionary:
	if DisplayServer.get_name() == "headless":
		return _fail("the game runs headless, nothing is rendered; run it without headless")
	await RenderingServer.frame_post_draw
	var img: Image = get_viewport().get_texture().get_image()
	return _ok({"png": Marshalls.raw_to_base64(img.save_png_to_buffer()), "width": img.get_width(), "height": img.get_height()})


# ---- значения ----

func _to_json(value: Variant, depth: int) -> Variant:
	match typeof(value):
		TYPE_NIL, TYPE_BOOL, TYPE_INT, TYPE_STRING:
			return value
		TYPE_FLOAT:
			var f: float = value
			if is_finite(f):
				return f
			return var_to_str(f)
		TYPE_STRING_NAME, TYPE_NODE_PATH:
			return str(value)
		TYPE_OBJECT:
			var obj: Object = value
			if obj == null:
				return null
			if obj is Node:
				var n: Node = obj
				return "%s(%s)" % [n.get_class(), n.get_path()] if n.is_inside_tree() else n.get_class()
			if obj is Resource:
				var r: Resource = obj
				return r.resource_path if not r.resource_path.is_empty() else "<%s>" % r.get_class()
			return "<%s>" % obj.get_class()
		TYPE_ARRAY:
			var arr: Array = value
			var out: Array = []
			for i: int in mini(arr.size(), MAX_COLLECTION_ITEMS):
				out.append(_to_json(arr[i], depth + 1) if depth < 4 else str(arr[i]))
			if arr.size() > MAX_COLLECTION_ITEMS:
				out.append("... %d more" % (arr.size() - MAX_COLLECTION_ITEMS))
			return out
		TYPE_DICTIONARY:
			var dict: Dictionary = value
			var out_dict: Dictionary = {}
			for key: Variant in dict.keys():
				if out_dict.size() >= MAX_COLLECTION_ITEMS:
					out_dict["..."] = "%d more" % (dict.size() - MAX_COLLECTION_ITEMS)
					break
				out_dict[str(key)] = _to_json(dict[key], depth + 1) if depth < 4 else str(dict[key])
			return out_dict
	var text: String = var_to_str(value)
	if text.length() > MAX_VALUE_CHARS:
		return "%s… (%d chars)" % [text.substr(0, MAX_VALUE_CHARS), text.length()]
	return text


func _int(v: Variant) -> int:
	if typeof(v) == TYPE_FLOAT or typeof(v) == TYPE_INT:
		var f: float = v
		return int(f)
	return 0


func _bool(v: Variant) -> bool:
	if typeof(v) == TYPE_BOOL:
		var b: bool = v
		return b
	return false


func _float(v: Variant) -> float:
	if typeof(v) == TYPE_FLOAT or typeof(v) == TYPE_INT:
		var f: float = v
		return f
	return 0.0


func _array(v: Variant) -> Array:
	if typeof(v) == TYPE_ARRAY:
		var arr: Array = v
		return arr
	return []


func _vec2(v: Variant) -> Vector2:
	var arr: Array = _array(v)
	if arr.size() >= 2:
		return Vector2(_float(arr[0]), _float(arr[1]))
	return Vector2.ZERO

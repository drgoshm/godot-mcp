## Служебный скрипт для сцен: собирает, читает и правит .tscn средствами самого
## движка (PackedScene, SceneState, ResourceSaver). Так Godot сам проставляет UID,
## ext_resource и owner'ов — в отличие от ручного редактирования текста.
##
## Запуск: godot --headless --path <proj> --script scene_builder.gd -- --spec=<in.json> --out=<out.json>
## Режим задаётся полем "mode" спецификации:
##   create — собрать новую сцену из дерева узлов;
##   tree   — вернуть сцену в том же формате, что принимает create;
##   edit   — применить список операций к существующей сцене и сохранить её.
## Результат записывается в --out одним JSON-объектом {"ok": ..., "error"?: ...}.
## Вывод движка (stdout) при этом не нужен, поэтому большие деревья не обрезаются.
##
## Скрипт выполняется с настройками проекта, а в нём предупреждения могут быть
## ошибками. Поэтому код не даёт ни одного предупреждения GDScript (это проверяет
## TestCreateSceneStrictWarnings): Variant сначала кладётся в типизированную
## переменную, а ошибки копятся в Array[String], чей append ничего не возвращает.
extends SceneTree

## Длиннее этого значения в режиме tree заменяются заглушкой {"_omitted": ...}.
const DEFAULT_MAX_VALUE_CHARS: int = 400
## Сколько элементов Array/Dictionary показываем в режиме tree.
const MAX_COLLECTION_ITEMS: int = 200

var _errors: Array[String] = []
var _out_path: String = ""
var _max_value_chars: int = DEFAULT_MAX_VALUE_CHARS


func _init() -> void:
	var spec_path: String = ""
	for arg: String in OS.get_cmdline_user_args():
		if arg.begins_with("--spec="):
			spec_path = arg.trim_prefix("--spec=")
		elif arg.begins_with("--out="):
			_out_path = arg.trim_prefix("--out=")
	if spec_path.is_empty():
		_finish({}, "missing --spec=<path>")
		return

	var text: String = FileAccess.get_file_as_string(spec_path)
	var parsed: Variant = JSON.parse_string(text)
	if typeof(parsed) != TYPE_DICTIONARY:
		_finish({}, "spec is not a JSON object")
		return
	var spec: Dictionary = parsed

	var path: String = str(spec.get("path", ""))
	if not path.begins_with("res://") or not (path.ends_with(".tscn") or path.ends_with(".scn")):
		_finish({}, "path must be a res:// path ending in .tscn or .scn")
		return

	var mode: String = str(spec.get("mode", "create"))
	match mode:
		"create":
			_create(spec, path)
		"tree":
			_tree(spec, path)
		"edit":
			_edit(spec, path)
		_:
			_finish({}, "unknown mode '%s'" % mode)


# ---- create ----

func _create(spec: Dictionary, path: String) -> void:
	if typeof(spec.get("root")) != TYPE_DICTIONARY:
		_finish({}, "root node spec is required")
		return
	var root_spec: Dictionary = spec["root"]

	var scene_root: Node = _build(root_spec, null, null)
	if scene_root != null and typeof(spec.get("connections")) == TYPE_ARRAY:
		var connections: Array = spec["connections"]
		for i: int in connections.size():
			var conn: Variant = connections[i]
			if typeof(conn) != TYPE_DICTIONARY:
				_errors.append("connections[%d]: expected an object {from, signal, to, method}" % i)
				continue
			var conn_spec: Dictionary = conn
			_connect(scene_root, conn_spec, "connections[%d]" % i)
	if scene_root == null or not _errors.is_empty():
		if scene_root != null:
			scene_root.free()
		_finish({}, "; ".join(PackedStringArray(_errors)))
		return
	_finish_save(scene_root, path, ResourceUID.INVALID_ID, {})


# ---- tree ----

## Читает сцену через SceneState — ровно то, что лежит в файле: только
## изменённые свойства, инстансы, группы и соединения.
func _tree(spec: Dictionary, path: String) -> void:
	if typeof(spec.get("max_value_chars")) == TYPE_FLOAT:
		var m: float = spec["max_value_chars"]
		if m > 0:
			_max_value_chars = int(m)
	var scene: PackedScene = ResourceLoader.load(path, "PackedScene", ResourceLoader.CACHE_MODE_IGNORE) as PackedScene
	if scene == null:
		_finish({}, "cannot load scene " + path)
		return
	var state: SceneState = scene.get_state()

	var by_path: Dictionary = {}
	var root_dict: Dictionary = {}
	for i: int in state.get_node_count():
		var node_path: String = _rel_path(state.get_node_path(i))
		var d: Dictionary = {"name": str(state.get_node_name(i)), "path": node_path}
		var instance: PackedScene = state.get_node_instance(i)
		var type_name: String = str(state.get_node_type(i))
		if instance != null:
			d["scene"] = instance.resource_path
		elif not type_name.is_empty():
			d["type"] = type_name
		else:
			# Узел из инстанцированной сцены, у которого здесь переопределены свойства.
			d["_inherited"] = true

		var props: Dictionary = {}
		for j: int in state.get_node_property_count(i):
			var prop_name: String = str(state.get_node_property_name(i, j))
			var value: Variant = state.get_node_property_value(i, j)
			if prop_name == "script" and value is Script:
				var script: Script = value
				if not script.resource_path.is_empty() and not script.resource_path.contains("::"):
					d["script"] = script.resource_path
					continue
			props[prop_name] = _to_json(value, 0)
		if not props.is_empty():
			d["properties"] = props
		var groups: PackedStringArray = state.get_node_groups(i)
		if not groups.is_empty():
			d["groups"] = Array(groups)

		if i == 0:
			root_dict = d
		else:
			var parent: Dictionary = _nearest_parent(by_path, _rel_path(state.get_node_path(i, true)), root_dict)
			if not parent.has("children"):
				parent["children"] = []
			var siblings: Array = parent["children"]
			siblings.append(d)
		by_path[node_path] = d

	var connections: Array = []
	for c: int in state.get_connection_count():
		var conn: Dictionary = {
			"from": _rel_path(state.get_connection_source(c)),
			"signal": str(state.get_connection_signal(c)),
			"to": _rel_path(state.get_connection_target(c)),
			"method": str(state.get_connection_method(c)),
		}
		var flags: int = state.get_connection_flags(c) & ~CONNECT_PERSIST
		if flags != 0:
			conn["flags"] = flags
		var binds: Array = state.get_connection_binds(c)
		if not binds.is_empty():
			conn["binds"] = _to_json(binds, 0)
		connections.append(conn)

	var result: Dictionary = {
		"path": path,
		"uid": _uid_text(_file_uid(path)),
		"nodes": state.get_node_count(),
		"root": root_dict,
		"connections": connections,
	}
	var only: String = _rel_path(NodePath(str(spec.get("node", ""))))
	if not only.is_empty() and only != ".":
		if not by_path.has(only):
			_finish({}, "no node at '%s' in %s" % [only, path])
			return
		# Для поддерева соединения всей сцены не нужны.
		result = {"path": result["path"], "uid": result["uid"], "nodes": result["nodes"], "root": by_path[only]}
	_finish(result, "")


## SceneState хранит пути как "./UI/Score"; наружу отдаём "UI/Score" и "." для корня,
## как в соединениях и операциях правки.
func _rel_path(p: NodePath) -> String:
	var s: String = str(p)
	while s.begins_with("./"):
		s = s.substr(2)
	return s if not s.is_empty() else "."


## Узлы внутри инстанса без переопределений в SceneState не попадают, поэтому
## родитель может отсутствовать — берём ближайшего известного предка.
func _nearest_parent(by_path: Dictionary, parent_path: String, root_dict: Dictionary) -> Dictionary:
	var p: String = parent_path
	while not p.is_empty() and p != ".":
		if by_path.has(p):
			var found: Dictionary = by_path[p]
			return found
		var cut: int = p.rfind("/")
		p = p.substr(0, cut) if cut > 0 else ""
	return root_dict


## Значение Godot -> JSON в формате, который принимает create/set_properties.
func _to_json(value: Variant, depth: int) -> Variant:
	match typeof(value):
		TYPE_NIL, TYPE_BOOL, TYPE_INT, TYPE_STRING:
			return value
		TYPE_FLOAT:
			var f: float = value
			if is_finite(f):
				return f
			return var_to_str(f)  # inf/nan в JSON не представимы
		TYPE_STRING_NAME, TYPE_NODE_PATH:
			return str(value)
		TYPE_OBJECT:
			if value is Resource:
				var res: Resource = value
				return _resource_to_json(res, depth)
			var obj: Object = value
			return _omitted("a %s object" % obj.get_class() if obj != null else "null object")
		TYPE_ARRAY:
			var arr: Array = value
			if arr.size() > MAX_COLLECTION_ITEMS:
				return _omitted("Array of %d items" % arr.size())
			var out: Array = []
			for item: Variant in arr:
				out.append(_to_json(item, depth + 1))
			return out
		TYPE_DICTIONARY:
			var dict: Dictionary = value
			if dict.size() > MAX_COLLECTION_ITEMS:
				return _omitted("Dictionary of %d items" % dict.size())
			var out_dict: Dictionary = {}
			for key: Variant in dict.keys():
				out_dict[str(key)] = _to_json(dict[key], depth + 1)
			return out_dict
	var text: String = var_to_str(value)
	if text.length() > _max_value_chars:
		return _omitted("%s, %d chars" % [type_string(typeof(value)), text.length()])
	return text


## Внешний ресурс -> его путь; встроенный -> {"_type": ..., свойства}.
func _resource_to_json(res: Resource, depth: int) -> Variant:
	var res_path: String = res.resource_path
	if not res_path.is_empty() and not res_path.contains("::"):
		return res_path
	if res is Script:
		return _omitted("built-in script")
	if depth > 8:
		return _omitted("%s nested too deep" % res.get_class())

	var d: Dictionary = {}
	var script_name: String = _script_class_name(res.get_script())
	var script_value: Variant = res.get_script()
	if not script_name.is_empty():
		d["_type"] = script_name
	elif script_value is Script:
		var script: Script = script_value
		d["_script"] = script.resource_path
	else:
		d["_type"] = res.get_class()

	for p: Dictionary in res.get_property_list():
		var usage: int = p["usage"]
		var prop_name: String = p["name"]
		if usage & PROPERTY_USAGE_STORAGE == 0 or prop_name in ["script", "resource_path", "resource_scene_unique_id"]:
			continue
		var current: Variant = res.get(prop_name)
		var default: Variant = _default_value(res, prop_name)
		if typeof(current) == typeof(default) and current == default:
			continue
		d[prop_name] = _to_json(current, depth + 1)
	return d


func _default_value(res: Resource, prop_name: String) -> Variant:
	var script_value: Variant = res.get_script()
	if script_value is Script:
		var script: Script = script_value
		var from_script: Variant = script.get_property_default_value(prop_name)
		if from_script != null:
			return from_script
	return ClassDB.class_get_property_default_value(res.get_class(), prop_name)


func _omitted(what: String) -> Dictionary:
	return {"_omitted": what}


# ---- edit ----

## Применяет операции по порядку к загруженной сцене. Первая ошибка отменяет
## всё: файл не перезаписывается.
func _edit(spec: Dictionary, path: String) -> void:
	if typeof(spec.get("operations")) != TYPE_ARRAY:
		_finish({}, "operations must be a list")
		return
	var ops: Array = spec["operations"]
	var uid: int = _file_uid(path)
	var scene: PackedScene = ResourceLoader.load(path, "PackedScene", ResourceLoader.CACHE_MODE_IGNORE) as PackedScene
	if scene == null:
		_finish({}, "cannot load scene " + path)
		return
	var scene_root: Node = scene.instantiate(PackedScene.GEN_EDIT_STATE_INSTANCE)
	if scene_root == null:
		_finish({}, "cannot instantiate " + path)
		return

	for i: int in ops.size():
		var op_value: Variant = ops[i]
		if typeof(op_value) != TYPE_DICTIONARY:
			_errors.append("operations[%d]: expected an object with \"op\"" % i)
			break
		var op: Dictionary = op_value
		var where: String = "operations[%d] %s" % [i, str(op.get("op", "?"))]
		_apply(scene_root, op, where)
		if not _errors.is_empty():
			# Ошибки из _build/_set_property не знают номера операции — дописываем.
			for k: int in _errors.size():
				if not _errors[k].begins_with(where):
					_errors[k] = where + ": " + _errors[k]
			break
	if not _errors.is_empty():
		scene_root.free()
		_finish({}, "; ".join(PackedStringArray(_errors)) + " (no changes were saved)")
		return
	_finish_save(scene_root, path, uid, {"applied": ops.size()})


func _apply(scene_root: Node, op: Dictionary, where: String) -> void:
	var kind: String = str(op.get("op", ""))
	match kind:
		"add_node":
			var parent: Node = _editable_node(scene_root, str(op.get("parent", ".")), where)
			if parent == null:
				return
			if typeof(op.get("node")) != TYPE_DICTIONARY:
				_errors.append("%s: \"node\" must be a node spec" % where)
				return
			var node_spec: Dictionary = op["node"]
			var node: Node = _build(node_spec, parent, scene_root)
			if node != null and _is_number(op.get("index")):
				parent.move_child(node, _int(op["index"]))
		"remove_node":
			var node: Node = _editable_node(scene_root, str(op.get("path", "")), where)
			if node == null:
				return
			if node == scene_root:
				_errors.append("%s: cannot remove the scene root" % where)
				return
			node.get_parent().remove_child(node)
			node.free()
		"set_properties":
			var node: Node = _editable_node(scene_root, str(op.get("path", ".")), where)
			if node == null:
				return
			if typeof(op.get("properties")) != TYPE_DICTIONARY:
				_errors.append("%s: \"properties\" must be an object" % where)
				return
			var props: Dictionary = op["properties"]
			var label: String = "%s: %s" % [where, str(op.get("path", "."))]
			for key: Variant in props.keys():
				_set_property(node, label, str(key), props[key])
		"rename":
			var node: Node = _editable_node(scene_root, str(op.get("path", "")), where)
			if node == null:
				return
			_rename(node, str(op.get("name", "")), where)
		"move":
			_move(scene_root, op, where)
		"groups":
			var node: Node = _editable_node(scene_root, str(op.get("path", ".")), where)
			if node == null:
				return
			for g: Variant in _list(op.get("add")):
				node.add_to_group(str(g), true)
			for g: Variant in _list(op.get("remove")):
				node.remove_from_group(str(g))
		"connect":
			_connect(scene_root, op, where)
		"disconnect":
			_disconnect(scene_root, op, where)
		_:
			_errors.append("%s: unknown op; use add_node, remove_node, set_properties, rename, move, groups, connect or disconnect" % where)


## Узел, который можно править в этой сцене: корень или узел с owner == корень.
## Узлы внутри инстанса принадлежат своей сцене и сохраняются там.
func _editable_node(scene_root: Node, path: String, where: String) -> Node:
	var node: Node = scene_root.get_node_or_null(NodePath(path))
	if node == null:
		_errors.append("%s: no node at '%s' (paths are relative to the scene root, \".\" is the root)" % [where, path])
		return null
	if node != scene_root and node.owner != scene_root:
		var source: String = node.owner.scene_file_path if node.owner != null else "another scene"
		_errors.append("%s: '%s' belongs to the instanced scene %s; edit that scene instead" % [where, path, source])
		return null
	return node


func _rename(node: Node, new_name: String, where: String) -> void:
	if new_name.is_empty() or new_name.validate_node_name() != new_name:
		_errors.append("%s: '%s' is not a valid node name" % [where, new_name])
		return
	var parent: Node = node.get_parent()
	if parent != null and new_name != str(node.name) and parent.has_node(NodePath(new_name)):
		_errors.append("%s: '%s' already has a child named '%s'" % [where, parent.name, new_name])
		return
	node.name = new_name


func _move(scene_root: Node, op: Dictionary, where: String) -> void:
	var node: Node = _editable_node(scene_root, str(op.get("path", "")), where)
	if node == null:
		return
	if node == scene_root:
		_errors.append("%s: cannot move the scene root" % where)
		return
	var new_parent: Node = node.get_parent()
	if op.has("parent"):
		new_parent = _editable_node(scene_root, str(op["parent"]), where)
		if new_parent == null:
			return
	if new_parent == node or node.is_ancestor_of(new_parent):
		_errors.append("%s: cannot move a node into itself or its own child" % where)
		return
	if new_parent != node.get_parent():
		if new_parent.has_node(NodePath(str(node.name))):
			_errors.append("%s: '%s' already has a child named '%s'" % [where, new_parent.name, node.name])
			return
		# reparent сохраняет owner не всегда — запоминаем и восстанавливаем.
		var owned: Array[Node] = _owned_by(node, scene_root)
		var keep_global: bool = op.get("keep_global_transform", false) == true
		node.reparent(new_parent, keep_global)
		for n: Node in owned:
			n.owner = scene_root
	if _is_number(op.get("index")):
		new_parent.move_child(node, _int(op["index"]))


func _owned_by(node: Node, scene_root: Node) -> Array[Node]:
	var out: Array[Node] = []
	if node.owner == scene_root:
		out.append(node)
	for child: Node in node.get_children():
		out.append_array(_owned_by(child, scene_root))
	return out


func _disconnect(scene_root: Node, spec: Dictionary, where: String) -> void:
	var from_path: String = str(spec.get("from", "."))
	var to_path: String = str(spec.get("to", "."))
	var signal_name: String = str(spec.get("signal", ""))
	var method: String = str(spec.get("method", ""))
	var from_node: Node = scene_root.get_node_or_null(NodePath(from_path))
	var to_node: Node = scene_root.get_node_or_null(NodePath(to_path))
	if from_node == null or to_node == null:
		_errors.append("%s: no node at '%s'" % [where, from_path if from_node == null else to_path])
		return
	if not from_node.has_signal(signal_name):
		_errors.append("%s: %s has no signal '%s'" % [where, from_path, signal_name])
		return
	for c: Dictionary in from_node.get_signal_connection_list(signal_name):
		var callable: Callable = c["callable"]
		if callable.get_object() == to_node and str(callable.get_method()) == method:
			from_node.disconnect(signal_name, callable)
			return
	_errors.append("%s: %s:%s is not connected to %s:%s" % [where, from_path, signal_name, to_path, method])


# ---- общее ----

## Упаковывает дерево, освобождает его и сохраняет сцену. UID сохраняется:
## на сцену могут ссылаться другие сцены по uid://.
func _finish_save(scene_root: Node, path: String, uid: int, extra: Dictionary) -> void:
	var packed: PackedScene = PackedScene.new()
	var pack_err: Error = packed.pack(scene_root)
	scene_root.free()
	if pack_err != OK:
		_finish({}, "PackedScene.pack failed: " + error_string(pack_err))
		return
	var dir_err: Error = DirAccess.make_dir_recursive_absolute(path.get_base_dir())
	if dir_err != OK:
		_finish({}, "cannot create directory %s: %s" % [path.get_base_dir(), error_string(dir_err)])
		return
	var save_err: Error = ResourceSaver.save(packed, path)
	if save_err != OK:
		_finish({}, "ResourceSaver.save failed: " + error_string(save_err))
		return

	# Вне редактора ResourceSaver не всегда назначает UID (и может назначить новый) —
	# ставим прежний или создаём свой.
	var saved_uid: int = _file_uid(path)
	if uid == ResourceUID.INVALID_ID:
		uid = saved_uid if saved_uid != ResourceUID.INVALID_ID else ResourceUID.create_id()
	if saved_uid != uid and ResourceSaver.set_uid(path, uid) != OK:
		uid = saved_uid

	var state: SceneState = packed.get_state()
	var result: Dictionary = extra.duplicate()
	result["path"] = path
	result["uid"] = _uid_text(uid)
	result["nodes"] = state.get_node_count()
	result["connections"] = state.get_connection_count()
	_finish(result, "")


## UID сцены. ResourceLoader.get_resource_uid опирается на кеш UID редактора,
## которого нет в неимпортированном проекте, поэтому у .tscn читаем заголовок сами.
func _file_uid(path: String) -> int:
	if path.ends_with(".tscn"):
		var f: FileAccess = FileAccess.open(path, FileAccess.READ)
		if f != null:
			var header: String = f.get_line()
			f.close()
			var start: int = header.find("uid=\"")
			if start >= 0:
				start += 5
				var end: int = header.find("\"", start)
				if end > start:
					return ResourceUID.text_to_id(header.substr(start, end - start))
	return ResourceLoader.get_resource_uid(path)


func _uid_text(uid: int) -> String:
	return ResourceUID.id_to_text(uid) if uid != ResourceUID.INVALID_ID else ""


func _is_number(v: Variant) -> bool:
	return typeof(v) == TYPE_FLOAT or typeof(v) == TYPE_INT


func _int(v: Variant) -> int:
	var f: float = v
	return int(f)


func _list(v: Variant) -> Array:
	if typeof(v) == TYPE_ARRAY:
		var arr: Array = v
		return arr
	return []


## Рекурсивно создаёт узел по спецификации:
## {"type": "Sprite2D" | "scene": "res://x.tscn", "name": "...",
##  "script": "res://x.gd", "properties": {...}, "groups": [...], "children": [...]}
func _build(node_spec: Dictionary, parent: Node, owner_node: Node) -> Node:
	var node: Node = null
	var is_instance: bool = node_spec.has("scene")
	if node_spec.get("_inherited", false) == true:
		_errors.append("node '%s' comes from an instanced scene and cannot be created; set its properties with set_properties instead" % str(node_spec.get("name", "?")))
		return null

	if is_instance:
		var scene_path: String = str(node_spec["scene"])
		var scene: PackedScene = load(scene_path) as PackedScene
		if scene == null:
			_errors.append("cannot load scene " + scene_path)
			return null
		node = scene.instantiate(PackedScene.GEN_EDIT_STATE_INSTANCE)
	else:
		var type_name: String = str(node_spec.get("type", "Node"))
		if not ClassDB.class_exists(type_name) or not ClassDB.is_parent_class(type_name, "Node"):
			_errors.append("unknown node type '%s'" % type_name)
			return null
		if not ClassDB.can_instantiate(type_name):
			_errors.append("node type '%s' is abstract" % type_name)
			return null
		node = ClassDB.instantiate(type_name)

	var wanted_name: String = str(node_spec.get("name", node.get_class()))
	node.name = wanted_name

	if parent != null:
		parent.add_child(node)
		# При совпадении имён Godot молча переименует узел в "@Name@2" — лучше сказать.
		if str(node.name) != wanted_name:
			_errors.append("cannot name a node '%s' under '%s': a sibling already has that name or it has invalid characters" % [wanted_name, parent.name])
		# Только узлы с owner сохраняются в PackedScene. Детей инстанса не трогаем:
		# они принадлежат своей сцене.
		node.owner = owner_node

	if node_spec.has("script"):
		var script_path: String = str(node_spec["script"])
		var script: Script = load(script_path) as Script
		if script == null:
			_errors.append("cannot load script " + script_path)
		else:
			node.set_script(script)

	if typeof(node_spec.get("properties")) == TYPE_DICTIONARY:
		var props: Dictionary = node_spec["properties"]
		for key: Variant in props.keys():
			_set_property(node, str(node.name), str(key), props[key])

	if typeof(node_spec.get("groups")) == TYPE_ARRAY:
		var groups: Array = node_spec["groups"]
		for g: Variant in groups:
			node.add_to_group(str(g), true)

	if typeof(node_spec.get("children")) == TYPE_ARRAY:
		var children: Array = node_spec["children"]
		var child_owner: Node = node if owner_node == null else owner_node
		for child: Variant in children:
			if typeof(child) == TYPE_DICTIONARY:
				var child_spec: Dictionary = child
				var _child: Node = _build(child_spec, node, child_owner)

	return node


## Подключает сигнал так, чтобы соединение сохранилось в сцене ([connection] в .tscn):
## {"from": "UI/Button", "signal": "pressed", "to": ".", "method": "_on_pressed",
##  "binds": [...], "flags": CONNECT_DEFERRED | CONNECT_ONE_SHOT}.
## Пути узлов — относительно корня сцены, "." — сам корень. Всё, что Godot
## проверил бы только при срабатывании сигнала, проверяем сразу.
func _connect(scene_root: Node, spec: Dictionary, where: String) -> void:
	var from_path: String = str(spec.get("from", "."))
	var to_path: String = str(spec.get("to", "."))
	var signal_name: String = str(spec.get("signal", ""))
	var method: String = str(spec.get("method", ""))
	if signal_name.is_empty() or method.is_empty():
		_errors.append("%s: signal and method are required" % where)
		return

	var from_node: Node = scene_root.get_node_or_null(NodePath(from_path))
	var to_node: Node = scene_root.get_node_or_null(NodePath(to_path))
	if from_node == null or to_node == null:
		_errors.append("%s: no node at '%s' (paths are relative to the scene root, \".\" is the root)" % [where, from_path if from_node == null else to_path])
		return
	var label: String = "%s: %s:%s -> %s:%s" % [where, from_path, signal_name, to_path, method]

	var signal_info: Dictionary = {}
	for s: Dictionary in from_node.get_signal_list():
		if s["name"] == signal_name:
			signal_info = s
	if signal_info.is_empty():
		_errors.append("%s: %s has no signal '%s'" % [label, from_node.get_class(), signal_name])
		return

	# Последнее совпадение: методы скрипта идут после нативных и переопределяют их.
	var method_info: Dictionary = {}
	for m: Dictionary in to_node.get_method_list():
		if m["name"] == method:
			method_info = m
	if method_info.is_empty():
		_errors.append("%s: the target has no method '%s'; add it to the target's script first" % [label, method])
		return

	var binds: Array = []
	if typeof(spec.get("binds")) == TYPE_ARRAY:
		binds = spec["binds"]
	var signal_args: Array = signal_info["args"]
	var method_args: Array = method_info["args"]
	var defaults: Array = method_info["default_args"]
	var flags: int = method_info["flags"]
	var passed: int = signal_args.size() + binds.size()
	var required: int = method_args.size() - defaults.size()
	if flags & METHOD_FLAG_VARARG == 0 and (passed < required or passed > method_args.size()):
		var accepted: String = str(required) if required == method_args.size() else "%d..%d" % [required, method_args.size()]
		_errors.append("%s: the method takes %s arguments, but the signal passes %d (+%d binds)" % [label, accepted, signal_args.size(), binds.size()])
		return

	var conn_flags: int = CONNECT_PERSIST
	if typeof(spec.get("flags")) == TYPE_FLOAT or typeof(spec.get("flags")) == TYPE_INT:
		var f: float = spec["flags"]
		conn_flags |= int(f)
	var callable: Callable = Callable(to_node, method)
	if not binds.is_empty():
		callable = callable.bindv(binds)
	var err: Error = from_node.connect(signal_name, callable, conn_flags)
	if err != OK:
		_errors.append("%s: connect failed: %s" % [label, error_string(err)])


## Приводит JSON-значение к типу свойства узла или ресурса и присваивает его:
## - Object-свойства (texture, shape, mesh...) принимают res://-путь или
##   встроенный ресурс {"_type": "RectangleShape2D", "size": "Vector2(32, 32)"};
## - массивы (в том числе Array[MyResource]) — поэлементно по тем же правилам;
## - String/StringName/NodePath берутся как есть;
## - остальное из строки разбирается как литерал Godot: "Vector2(10, 20)", "Color(1, 0, 0)".
## label — путь для сообщений об ошибках, например "Player/Shape.shape".
func _set_property(target: Object, label: String, prop: String, raw: Variant) -> void:
	var info: Dictionary = {}
	for p: Dictionary in target.get_property_list():
		if p["name"] == prop:
			info = p
			break
	var where: String = label + "." + prop
	if info.is_empty():
		_errors.append("%s has no property '%s'" % [label, prop])
		return
	var prop_type: int = info["type"]

	var value: Variant = raw
	if _has_key(raw, "_omitted"):
		_errors.append("%s: this value was omitted by godot_scene_tree and cannot be written back; leave the property out" % where)
		return
	if _is_resource_spec(raw):
		if prop_type != TYPE_OBJECT and prop_type != TYPE_NIL:
			_errors.append("%s: a {\"_type\": ...} resource was given, but the property is %s" % [where, type_string(prop_type)])
			return
		var res_spec: Dictionary = raw
		value = _make_resource(res_spec, where)
		if value == null:
			return
	elif typeof(raw) == TYPE_ARRAY and (prop_type == TYPE_ARRAY or prop_type == TYPE_NIL):
		var items: Array = raw
		value = _convert_array(target.get(prop), items, where)
		if value == null:
			return
	elif typeof(raw) == TYPE_STRING:
		var s: String = raw
		value = _parse_string(s, prop_type, where)
		if value == null:
			return
	elif typeof(raw) == TYPE_FLOAT and prop_type == TYPE_INT:
		var f: float = raw
		value = int(f)  # JSON не различает int и float

	target.set(prop, value)

	# Нативный сеттер молча превращает объект не того класса в null, а
	# типизированное свойство скрипта просто не меняется. Проверяем чтением.
	if typeof(value) == TYPE_OBJECT and target.get(prop) != value:
		_errors.append("%s: cannot assign %s, the property expects %s" % [where, _describe(value), _expected_type(info)])


## Строка -> значение: путь к ресурсу для Object-свойств, иначе литерал Godot.
## Возвращает null и пишет ошибку, если разобрать не удалось.
func _parse_string(s: String, prop_type: int, where: String) -> Variant:
	match prop_type:
		TYPE_STRING, TYPE_STRING_NAME, TYPE_NODE_PATH:
			return s
		TYPE_NIL:  # нетипизированное свойство или элемент: литерал, если разбирается, иначе строка
			var parsed: Variant = str_to_var(s)
			return parsed if parsed != null else s
		TYPE_OBJECT:
			var res: Resource = load(s) if (s.begins_with("res://") or s.begins_with("uid://")) else null
			if res == null:
				_errors.append("%s: cannot load resource '%s'" % [where, s])
			return res
	var value: Variant = str_to_var(s)
	if value == null:
		_errors.append("%s: cannot parse '%s' as a Godot value" % [where, s])
	return value


func _has_key(v: Variant, key: String) -> bool:
	if typeof(v) != TYPE_DICTIONARY:
		return false
	var d: Dictionary = v
	return d.has(key)


func _is_resource_spec(v: Variant) -> bool:
	if typeof(v) != TYPE_DICTIONARY:
		return false
	var d: Dictionary = v
	return d.has("_type") or d.has("_script")


## Создаёт встроенный ресурс по спецификации
## {"_type": "Класс или class_name", "_script": "res://x.gd", ...свойства}.
## PackedScene.pack сохранит его внутри .tscn как sub_resource.
func _make_resource(spec: Dictionary, where: String) -> Resource:
	var res: Resource = null
	var type_name: String = str(spec.get("_type", ""))

	if spec.has("_script"):
		res = _instantiate_script(str(spec["_script"]), where)
	elif ClassDB.class_exists(type_name):
		if not ClassDB.is_parent_class(type_name, "Resource"):
			_errors.append("%s: '%s' is not a Resource type" % [where, type_name])
			return null
		if not ClassDB.can_instantiate(type_name):
			_errors.append("%s: resource type '%s' is abstract; use a concrete subclass" % [where, type_name])
			return null
		res = ClassDB.instantiate(type_name)
	else:
		var script_path: String = _global_class_path(type_name)
		if script_path.is_empty():
			_errors.append("%s: unknown resource type '%s' (for a class_name resource, run godot_import first or pass _script)" % [where, type_name])
			return null
		res = _instantiate_script(script_path, where)
	if res == null:
		return null

	var label: String = "%s<%s>" % [where, _describe(res)]
	for key: Variant in spec.keys():
		var k: String = str(key)
		if k == "_type" or k == "_script":
			continue
		_set_property(res, label, k, spec[key])
	return res


func _instantiate_script(script_path: String, where: String) -> Resource:
	var script: Script = load(script_path) as Script
	if script == null:
		_errors.append("%s: cannot load script %s" % [where, script_path])
		return null
	if not script.can_instantiate():
		_errors.append("%s: cannot instantiate %s" % [where, script_path])
		return null
	# new() есть у GDScript и CSharpScript, но не у базового Script — зовём динамически.
	var obj: Variant = script.call("new")
	if not (obj is Resource):
		if obj is Object and not (obj is RefCounted):
			var o: Object = obj
			o.free()
		_errors.append("%s: %s does not extend Resource" % [where, script_path])
		return null
	var res: Resource = obj
	return res


## Путь скрипта с данным class_name из кеша глобальных классов проекта.
func _global_class_path(class_name_: String) -> String:
	for c: Dictionary in ProjectSettings.get_global_class_list():
		if str(c["class"]) == class_name_:
			return str(c["path"])
	return ""


## Преобразует JSON-массив, сохраняя тип текущего значения свойства
## (Array[MyResource]): элементы-словари становятся ресурсами, строки — путями или литералами.
func _convert_array(current: Variant, raw: Array, where: String) -> Variant:
	var out: Array = []
	if current is Array:
		var cur: Array = current
		out = cur.duplicate()
		out.clear()
	var elem_type: int = out.get_typed_builtin() if out.is_typed() else TYPE_NIL
	for i: int in raw.size():
		var item: Variant = raw[i]
		var item_where: String = "%s[%d]" % [where, i]
		if _is_resource_spec(item):
			var item_spec: Dictionary = item
			item = _make_resource(item_spec, item_where)
		elif typeof(item) == TYPE_STRING:
			var s: String = item
			item = _parse_string(s, elem_type, item_where)
		elif typeof(item) == TYPE_FLOAT and elem_type == TYPE_INT:
			var f: float = item
			item = int(f)
		if item == null and typeof(raw[i]) != TYPE_NIL:
			return null  # ошибка уже записана
		var before: int = out.size()
		out.append(item)
		if out.size() == before:
			_errors.append("%s: %s does not fit the array element type %s" % [item_where, _describe(item), _array_elem_type(out)])
			return null
	return out


func _array_elem_type(arr: Array) -> String:
	var name_: String = _script_class_name(arr.get_typed_script())
	if not name_.is_empty():
		return name_
	if not arr.get_typed_class_name().is_empty():
		return str(arr.get_typed_class_name())
	return type_string(arr.get_typed_builtin())


func _describe(value: Variant) -> String:
	if value is Resource:
		var res: Resource = value
		var name_: String = _script_class_name(res.get_script())
		return name_ if not name_.is_empty() else res.get_class()
	return type_string(typeof(value))


## class_name скрипта или "", если скрипта нет или он безымянный.
func _script_class_name(v: Variant) -> String:
	if not (v is Script):
		return ""
	var s: Script = v
	return str(s.get_global_name())


func _expected_type(info: Dictionary) -> String:
	var hint: String = str(info.get("hint_string", ""))
	var t: int = info["type"]
	return hint if not hint.is_empty() else type_string(t)


func _finish(result: Dictionary, error: String) -> void:
	var payload: Dictionary = result.duplicate()
	payload["ok"] = error.is_empty()
	if not error.is_empty():
		payload["error"] = error
	var json: String = JSON.stringify(payload)
	var f: FileAccess = FileAccess.open(_out_path, FileAccess.WRITE) if not _out_path.is_empty() else null
	if f != null:
		# store_string возвращает bool только с Godot 4.4 — зовём динамически.
		var _stored: Variant = f.call("store_string", json)
		f.close()
	else:
		print("MCP_RESULT:" + json)
	quit(0 if error.is_empty() else 1)

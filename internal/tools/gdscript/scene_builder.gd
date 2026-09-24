## Собирает сцену из JSON-спецификации и сохраняет её через ResourceSaver.
## Так Godot сам проставляет UID, ext_resource и owner'ов — в отличие от
## ручного редактирования .tscn.
##
## Запуск: godot --headless --path <proj> --script scene_builder.gd -- --spec=<file.json>
## Результат печатается одной строкой: MCP_RESULT:{...json...}
##
## Скрипт выполняется с настройками проекта, а в нём предупреждения могут быть
## ошибками. Поэтому код не даёт ни одного предупреждения GDScript (это проверяет
## TestCreateSceneStrictWarnings): Variant сначала кладётся в типизированную
## переменную, а ошибки копятся в Array[String], чей append ничего не возвращает.
extends SceneTree

var _errors: Array[String] = []
var _node_count: int = 0
var _connection_count: int = 0


func _init() -> void:
	var spec_path: String = ""
	for arg: String in OS.get_cmdline_user_args():
		if arg.begins_with("--spec="):
			spec_path = arg.trim_prefix("--spec=")
	if spec_path.is_empty():
		_finish({}, "missing --spec=<path>")
		return

	var text: String = FileAccess.get_file_as_string(spec_path)
	var parsed: Variant = JSON.parse_string(text)
	if typeof(parsed) != TYPE_DICTIONARY:
		_finish({}, "spec is not a JSON object")
		return
	var spec: Dictionary = parsed

	var out_path: String = str(spec.get("path", ""))
	if not out_path.begins_with("res://") or not (out_path.ends_with(".tscn") or out_path.ends_with(".scn")):
		_finish({}, "path must be a res:// path ending in .tscn or .scn")
		return
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

	var packed: PackedScene = PackedScene.new()
	var pack_err: Error = packed.pack(scene_root)
	if pack_err != OK:
		scene_root.free()
		_finish({}, "PackedScene.pack failed: " + error_string(pack_err))
		return

	var dir_err: Error = DirAccess.make_dir_recursive_absolute(out_path.get_base_dir())
	if dir_err != OK:
		scene_root.free()
		_finish({}, "cannot create directory %s: %s" % [out_path.get_base_dir(), error_string(dir_err)])
		return
	var save_err: Error = ResourceSaver.save(packed, out_path)
	scene_root.free()
	if save_err != OK:
		_finish({}, "ResourceSaver.save failed: " + error_string(save_err))
		return

	# Вне редактора ResourceSaver не всегда назначает UID — назначаем сами,
	# чтобы на сцену можно было ссылаться стабильно (uid://...).
	var uid: int = ResourceLoader.get_resource_uid(out_path)
	if uid == ResourceUID.INVALID_ID:
		uid = ResourceUID.create_id()
		if ResourceSaver.set_uid(out_path, uid) != OK:
			uid = ResourceUID.INVALID_ID
	var uid_text: String = ResourceUID.id_to_text(uid) if uid != ResourceUID.INVALID_ID else ""
	_finish({"path": out_path, "uid": uid_text, "nodes": _node_count, "connections": _connection_count}, "")


## Рекурсивно создаёт узел по спецификации:
## {"type": "Sprite2D" | "scene": "res://x.tscn", "name": "...",
##  "script": "res://x.gd", "properties": {...}, "groups": [...], "children": [...]}
func _build(node_spec: Dictionary, parent: Node, owner_node: Node) -> Node:
	var node: Node = null
	var is_instance: bool = node_spec.has("scene")

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

	node.name = str(node_spec.get("name", node.get_class()))
	_node_count += 1

	if parent != null:
		parent.add_child(node)
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
		return
	_connection_count += 1


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
	print("MCP_RESULT:" + JSON.stringify(payload))
	quit(0 if error.is_empty() else 1)

## Собирает сцену из JSON-спецификации и сохраняет её через ResourceSaver.
## Так Godot сам проставляет UID, ext_resource и owner'ов — в отличие от
## ручного редактирования .tscn.
##
## Запуск: godot --headless --path <proj> --script scene_builder.gd -- --spec=<file.json>
## Результат печатается одной строкой: MCP_RESULT:{...json...}
##
## Всё типизировано явно: в проекте могут быть включены
## «warnings as errors», и тогда нетипизированный код не загрузится.
extends SceneTree

var _errors: PackedStringArray = PackedStringArray()
var _node_count: int = 0


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

	var root: Node = _build(spec["root"], null, null)
	if root == null or not _errors.is_empty():
		if root != null:
			root.free()
		_finish({}, "; ".join(_errors))
		return

	var packed: PackedScene = PackedScene.new()
	var pack_err: Error = packed.pack(root)
	if pack_err != OK:
		root.free()
		_finish({}, "PackedScene.pack failed: " + error_string(pack_err))
		return

	DirAccess.make_dir_recursive_absolute(out_path.get_base_dir())
	var save_err: Error = ResourceSaver.save(packed, out_path)
	root.free()
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
	_finish({"path": out_path, "uid": uid_text, "nodes": _node_count}, "")


## Рекурсивно создаёт узел по спецификации:
## {"type": "Sprite2D" | "scene": "res://x.tscn", "name": "...",
##  "script": "res://x.gd", "properties": {...}, "groups": [...], "children": [...]}
func _build(node_spec: Dictionary, parent: Node, owner_node: Node) -> Node:
	var node: Node = null
	var is_instance: bool = node_spec.has("scene")

	if is_instance:
		var scene_path: String = str(node_spec["scene"])
		var res: Resource = load(scene_path)
		if not (res is PackedScene):
			_errors.append("cannot load scene " + scene_path)
			return null
		node = (res as PackedScene).instantiate(PackedScene.GEN_EDIT_STATE_INSTANCE)
	else:
		var type_name: String = str(node_spec.get("type", "Node"))
		if not ClassDB.class_exists(type_name) or not ClassDB.is_parent_class(type_name, "Node"):
			_errors.append("unknown node type '%s'" % type_name)
			return null
		if not ClassDB.can_instantiate(type_name):
			_errors.append("node type '%s' is abstract" % type_name)
			return null
		node = ClassDB.instantiate(type_name) as Node

	node.name = str(node_spec.get("name", node.get_class()))
	_node_count += 1

	if parent != null:
		parent.add_child(node)
		# Только узлы с owner сохраняются в PackedScene. Детей инстанса не трогаем:
		# они принадлежат своей сцене.
		node.owner = owner_node

	if node_spec.has("script"):
		var script_path: String = str(node_spec["script"])
		var script: Resource = load(script_path)
		if not (script is Script):
			_errors.append("cannot load script " + script_path)
		else:
			node.set_script(script)

	var props: Variant = node_spec.get("properties", {})
	if typeof(props) == TYPE_DICTIONARY:
		for key: Variant in (props as Dictionary).keys():
			_set_property(node, str(key), (props as Dictionary)[key])

	var groups: Variant = node_spec.get("groups", [])
	if typeof(groups) == TYPE_ARRAY:
		for g: Variant in groups:
			node.add_to_group(str(g), true)

	var children: Variant = node_spec.get("children", [])
	if typeof(children) == TYPE_ARRAY:
		var child_owner: Node = node if owner_node == null else owner_node
		for child: Variant in children:
			if typeof(child) == TYPE_DICTIONARY:
				_build(child, node, child_owner)

	return node


## Приводит JSON-значение к типу свойства:
## - Object-свойства (texture, mesh...) получают load() по res://-пути;
## - String/StringName/NodePath берутся как есть;
## - остальное из строки разбирается как литерал Godot: "Vector2(10, 20)", "Color(1, 0, 0)".
func _set_property(node: Node, prop: String, raw: Variant) -> void:
	var prop_type: int = -1
	for info: Dictionary in node.get_property_list():
		if info["name"] == prop:
			prop_type = info["type"]
			break
	if prop_type == -1:
		_errors.append("%s has no property '%s'" % [node.name, prop])
		return

	var value: Variant = raw
	if typeof(raw) == TYPE_STRING:
		var s: String = raw
		match prop_type:
			TYPE_STRING, TYPE_STRING_NAME, TYPE_NODE_PATH:
				value = s
			TYPE_OBJECT:
				value = load(s) if (s.begins_with("res://") or s.begins_with("uid://")) else null
				if value == null:
					_errors.append("%s.%s: cannot load resource '%s'" % [node.name, prop, s])
					return
			_:
				value = str_to_var(s)
				if value == null:
					_errors.append("%s.%s: cannot parse '%s' as a Godot value" % [node.name, prop, s])
					return
	elif prop_type == TYPE_INT and typeof(raw) == TYPE_FLOAT:
		value = int(raw)  # JSON не различает int и float

	node.set(prop, value)


func _finish(result: Dictionary, error: String) -> void:
	var payload: Dictionary = result.duplicate()
	payload["ok"] = error.is_empty()
	if not error.is_empty():
		payload["error"] = error
	print("MCP_RESULT:" + JSON.stringify(payload))
	quit(0 if error.is_empty() else 1)

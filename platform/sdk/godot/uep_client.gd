# Universal Engagement Platform — Godot SDK (GDScript).
# Thin by design: the server owns every decision.
# Attach to an autoload node; call track() from anywhere.
extends Node

signal ingest_result(result: Dictionary)     # {event_id, status}
signal state_loaded(state: Dictionary)
signal trace_loaded(trace: Dictionary)

var base_url: String = ""
var api_key: String = ""
var project_id: String = ""
var environment_id: String = ""
var actor_id: String = ""

var _queue: Array = []
var _flushing := false
const MAX_BATCH := 100


func setup(p_base_url: String, p_api_key: String, p_project_id: String, p_environment_id: String, p_user_id: String) -> void:
	base_url = p_base_url
	api_key = p_api_key
	project_id = p_project_id
	environment_id = p_environment_id
	actor_id = p_user_id


func set_user_id(p_user_id: String) -> void:
	actor_id = p_user_id


func _scope() -> String:
	return "/v1/projects/%s/environments/%s" % [project_id, environment_id]


func track(event_type: String, payload: Dictionary = {}) -> void:
	_queue.append({
		"event_type": event_type,
		"actor_id": actor_id,
		"occurred_at": Time.get_datetime_string_from_system(true) + "Z",
		"idempotency_key": "idem_%s" % [str(randi()).sha1().substr(0, 16)],
		"payload": payload,
	})
	if _queue.size() >= MAX_BATCH:
		flush()


func flush() -> void:
	if _flushing or _queue.is_empty():
		return
	_flushing = true
	var batch := _queue.duplicate()
	_queue.clear()
	var body := JSON.stringify({"events": batch})
	_request_async(HTTPClient.METHOD_POST, _scope() + "/events", body, func(result: Array):
		_flushing = false
		var code: int = result[1]
		var text: String = result[3]
		if code < 200 or code >= 300:
			# 4xx (except 429) = permanent drop; else requeue for retry.
			if not (code >= 400 and code < 500 and code != 429):
				_queue = batch + _queue
			return
		var parsed = JSON.parse_string(text)
		if parsed and parsed.has("results"):
			for r in parsed["results"]:
				ingest_result.emit(r)
	)


func get_state(user_id: String = "") -> void:
	var who := user_id if user_id != "" else actor_id
	_request_async(HTTPClient.METHOD_GET, _scope() + "/users/%s/state" % who, "", func(result: Array):
		var parsed = JSON.parse_string(result[3])
		state_loaded.emit(parsed if parsed else {})
	)


func get_leaderboard(leaderboard_id: String, limit: int = 50) -> void:
	_request_async(HTTPClient.METHOD_GET, "%s/leaderboards/%s?me=%s&limit=%d" % [_scope(), leaderboard_id, actor_id, limit], "", func(result: Array):
		var parsed = JSON.parse_string(result[3])
		state_loaded.emit(parsed if parsed else {})
	)


func get_trace(event_id: String) -> void:
	_request_async(HTTPClient.METHOD_GET, _scope() + "/events/%s/trace" % event_id, "", func(result: Array):
		var parsed = JSON.parse_string(result[3])
		trace_loaded.emit(parsed if parsed else {})
	)


func _request_async(method: int, path: String, body: String, on_done: Callable) -> void:
	var http := HTTPRequest.new()
	add_child(http)
	http.request_completed.connect(func(result: int, code: int, headers: PackedStringArray, text: String):
		http.queue_free()
		on_done.call([result, code, headers, text])
	, CONNECT_ONE_SHOT)
	var headers := PackedStringArray([
		"Content-Type: application/json",
		"Authorization: Bearer " + api_key,
	])
	if method == HTTPClient.METHOD_POST:
		http.request(base_url + path, headers, method, body)
	else:
		http.request(base_url + path, headers, method)

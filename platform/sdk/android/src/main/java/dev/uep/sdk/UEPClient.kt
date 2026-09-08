package dev.uep.sdk

import java.net.URI
import java.net.http.HttpClient
import java.net.http.HttpRequest
import java.net.http.HttpResponse
import java.time.Duration
import java.util.UUID

/**
 * Universal Engagement Platform — Android/JVM SDK.
 * Thin by design: the server owns every decision. Events batch + flush with
 * a stable idempotency key per event.
 */
class UEPClient(
    baseUrl: String,
    apiKey: String,
    projectId: String,
    environmentId: String,
    userId: String,
) {
    private val scope = "/v1/projects/$projectId/environments/$environmentId"
    private val http: HttpClient = HttpClient.newBuilder()
        .connectTimeout(Duration.ofSeconds(10))
        .build()
    private val auth = "Bearer $apiKey"
    private var actorId: String = userId

    /** Track one event; returns the per-event ingest result. */
    fun track(eventType: String, payload: Map<String, Any> = emptyMap()): IngestResult {
        val body = mapOf(
            "events" to listOf(
                mapOf(
                    "event_type" to eventType,
                    "actor_id" to actorId,
                    "occurred_at" to java.time.Instant.now().toString(),
                    "idempotency_key" to "idem_${UUID.randomUUID()}",
                    "payload" to payload,
                )
            )
        )
        val res = post("$scope/events", body) as? Map<*, *> ?: emptyMap<Any, Any>()
        val results = res["results"] as? List<*> ?: emptyList<Any>()
        val first = results.firstOrNull() as? Map<*, *> ?: emptyMap<Any, Any>()
        return IngestResult(
            eventId = first["event_id"] as? String ?: "",
            status = first["status"] as? String ?: "inserted",
        )
    }

    /** Full player state. */
    fun getState(userId: String = actorId): Map<String, Any>? =
        get("$scope/users/$userId/state") as? Map<String, Any>

    /** Leaderboard page with the caller's row included. */
    fun getLeaderboard(leaderboardId: String, me: String = actorId, limit: Int = 50): Map<String, Any>? =
        get("$scope/leaderboards/$leaderboardId?me=$me&limit=$limit") as? Map<String, Any>

    /** Decision trace — why did this event produce this outcome? */
    fun getTrace(eventId: String): Map<String, Any>? =
        get("$scope/events/$eventId/trace") as? Map<String, Any>

    fun setUserId(userId: String) { actorId = userId }

    private fun get(path: String): Any? = request("GET", path, null)
    private fun post(path: String, body: Any?): Any? = request("POST", path, body)

    private fun request(method: String, path: String, body: Any?): Any? {
        val builder = HttpRequest.newBuilder()
            .uri(URI.create(baseUrl + path))
            .header("Authorization", auth)
            .header("Content-Type", "application/json")
        when (method) {
            "GET" -> builder.GET()
            "POST" -> builder.POST(HttpRequest.BodyPublishers.ofString(json(body ?: emptyMap<String, Any>())))
        }
        val resp = http.send(builder.build(), HttpResponse.BodyHandlers.ofString())
        return if (resp.statusCode() in 200..299) parse(resp.body()) else null
    }

    private fun json(v: Any): String = UEPJson.encode(v)
    private fun parse(s: String): Any? = UEPJson.decode(s)

    data class IngestResult(val eventId: String, val status: String)
}

/** Minimal JSON encoder/decoder (no external deps on Android). */
object UEPJson {
    fun encode(v: Any): String = StringBuilder().apply { enc(v, this) }.toString()

    private fun enc(v: Any?, sb: StringBuilder) {
        when (v) {
            null -> sb.append("null")
            is String -> sb.append('"').append(v.replace("\\", "\\\\").replace("\"", "\\\"")).append('"')
            is Boolean, is Int, is Long, is Double -> sb.append(v.toString())
            is Map<*, *> -> {
                sb.append('{')
                v.entries.forEachIndexed { i, (k, value) ->
                    if (i > 0) sb.append(',')
                    sb.append('"').append(k.toString()).append("\":")
                    enc(value, sb)
                }
                sb.append('}')
            }
            is Iterable<*> -> {
                sb.append('[')
                v.forEachIndexed { i, e ->
                    if (i > 0) sb.append(',')
                    enc(e, sb)
                }
                sb.append(']')
            }
            else -> sb.append('"').append(v.toString()).append('"')
        }
    }

    fun decode(s: String): Any? = Parser(s).parse()

    private class Parser(private val s: String) {
        private var i = 0
        fun parse(): Any? {
            skip()
            if (i >= s.length) return null
            return when (s[i]) {
                '{' -> obj()
                '[' -> arr()
                '"' -> str()
                't' -> { i += 4; true }
                'f' -> { i += 5; false }
                'n' -> { i += 4; null }
                else -> num()
            }
        }
        private fun obj(): Map<String, Any?> {
            val m = LinkedHashMap<String, Any?>()
            i++ // {
            skip()
            while (i < s.length && s[i] != '}') {
                skip()
                val k = str()
                skip(); i++ // :
                m[k] = parse()
                skip()
                if (i < s.length && s[i] == ',') i++
            }
            i++ // }
            return m
        }
        private fun arr(): List<Any?> {
            val l = ArrayList<Any?>()
            i++ // [
            skip()
            while (i < s.length && s[i] != ']') {
                l.add(parse())
                skip()
                if (i < s.length && s[i] == ',') i++
            }
            i++ // ]
            return l
        }
        private fun str(): String {
            i++ // "
            val sb = StringBuilder()
            while (i < s.length && s[i] != '"') {
                if (s[i] == '\\' && i + 1 < s.length) { i++; sb.append(s[i]) }
                else sb.append(s[i])
                i++
            }
            i++ // "
            return sb.toString()
        }
        private fun num(): Any {
            val start = i
            while (i < s.length && s[i] !in ",}] \n") i++
            val raw = s.substring(start, i)
            return raw.toLongOrNull() ?: raw.toDoubleOrNull() ?: raw
        }
        private fun skip() { while (i < s.length && s[i] in " \n\t\r") i++ }
    }
}

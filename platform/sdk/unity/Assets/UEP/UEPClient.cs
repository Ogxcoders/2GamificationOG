// Universal Engagement Platform — Unity SDK.
// Thin by design: the server owns every decision. Uses UnityWebRequest so
// calls work on all Unity targets; tracking is fire-and-buffer with an
// explicit Flush().
using System;
using System.Collections;
using System.Collections.Generic;
using System.Text;
using UnityEngine;
using UnityEngine.Networking;

namespace UEP
{
    public class IngestResult
    {
        public string eventId;
        public string status; // inserted | duplicate | rejected
    }

    public class UEPClient : MonoBehaviour
    {
        [Serializable]
        private class EventBody
        {
            public string event_type;
            public string actor_id;
            public string occurred_at;
            public string idempotency_key;
            public Dictionary<string, object> payload;
        }

        private string baseUrl;
        private string scope;
        private string auth;
        private string actorId;
        private readonly List<EventBody> queue = new List<EventBody>();
        private bool flushing;

        public void Init(string baseUrl, string apiKey, string projectId, string environmentId, string userId)
        {
            this.baseUrl = baseUrl;
            this.scope = $"/v1/projects/{projectId}/environments/{environmentId}";
            this.auth = $"Bearer {apiKey}";
            this.actorId = userId;
        }

        public void SetUserId(string userId) => actorId = userId;

        /// <summary>Buffer an event (auto-flush at 100 pending).</summary>
        public void Track(string eventType, Dictionary<string, object> payload = null)
        {
            queue.Add(new EventBody
            {
                event_type = eventType,
                actor_id = actorId,
                occurred_at = DateTime.UtcNow.ToString("o"),
                idempotency_key = $"idem_{Guid.NewGuid():N}",
                payload = payload ?? new Dictionary<string, object>(),
            });
            if (queue.Count >= 100) StartCoroutine(Flush());
        }

        /// <summary>Send buffered events. Duplicate results are reported truthfully.</summary>
        public IEnumerator Flush(Action<List<IngestResult>> onDone = null)
        {
            if (flushing || queue.Count == 0) { onDone?.Invoke(new List<IngestResult>()); yield break; }
            flushing = true;
            var batch = new List<EventBody>(queue);
            queue.Clear();
            var wrapper = new Dictionary<string, object> { { "events", batch } };
            using var req = new UnityWebRequest(baseUrl + scope + "/events", "POST")
            {
                uploadHandler = new UploadHandlerRaw(Encoding.UTF8.GetBytes(MiniJson.Serialize(wrapper))),
                downloadHandler = new DownloadHandlerBuffer(),
            };
            req.SetRequestHeader("Content-Type", "application/json");
            req.SetRequestHeader("Authorization", auth);
            yield return req.SendWebRequest();
            flushing = false;
            if (req.result != UnityWebRequest.Result.Success)
            {
                // 4xx = permanent → drop; 5xx/network → requeue.
                bool permanent = req.responseCode >= 400 && req.responseCode < 500 && req.responseCode != 429;
                if (!permanent) queue.InsertRange(0, batch);
                onDone?.Invoke(new List<IngestResult>());
                yield break;
            }
            var parsed = MiniJson.Deserialize(req.downloadHandler.text) as Dictionary<string, object>;
            var results = new List<IngestResult>();
            if (parsed != null && parsed.TryGetValue("results", out var raw) && raw is List<object> list)
            {
                foreach (var e in list)
                {
                    if (e is Dictionary<string, object> m)
                    {
                        results.Add(new IngestResult
                        {
                            eventId = m.TryGetValue("event_id", out var id) ? id as string : "",
                            status = m.TryGetValue("status", out var st) ? st as string : "inserted",
                        });
                    }
                }
            }
            onDone?.Invoke(results);
        }

        /// <summary>Full player state (JSON mirror of the REST shape).</summary>
        public IEnumerator GetState(Action<Dictionary<string, object>> onDone)
        {
            yield return GetJson($"{scope}/users/{actorId}/state", onDone);
        }

        public IEnumerator GetLeaderboard(string leaderboardId, int limit, Action<Dictionary<string, object>> onDone)
        {
            yield return GetJson($"{scope}/leaderboards/{leaderboardId}?me={actorId}&limit={limit}", onDone);
        }

        public IEnumerator GetTrace(string eventId, Action<Dictionary<string, object>> onDone)
        {
            yield return GetJson($"{scope}/events/{eventId}/trace", onDone);
        }

        private IEnumerator GetJson(string path, Action<Dictionary<string, object>> onDone)
        {
            using var req = UnityWebRequest.Get(baseUrl + path);
            req.SetRequestHeader("Authorization", auth);
            yield return req.SendWebRequest();
            if (req.result != UnityWebRequest.Result.Success) { onDone?.Invoke(null); yield break; }
            onDone?.Invoke(MiniJson.Deserialize(req.downloadHandler.text) as Dictionary<string, object>);
        }
    }

    /// <summary>Dependency-free JSON helper (subset sufficient for the SDK).</summary>
    public static class MiniJson
    {
        public static string Serialize(object v)
        {
            var sb = new StringBuilder();
            Ser(v, sb);
            return sb.ToString();
        }

        private static void Ser(object v, StringBuilder sb)
        {
            switch (v)
            {
                case null: sb.Append("null"); break;
                case bool b: sb.Append(b ? "true" : "false"); break;
                case string s:
                    sb.Append('"');
                    foreach (var c in s)
                    {
                        if (c == '"' || c == '\\') sb.Append('\\');
                        sb.Append(c);
                    }
                    sb.Append('"');
                    break;
                case int or long: sb.Append(v.ToString()); break;
                case double d: sb.Append(d.ToString("R", System.Globalization.CultureInfo.InvariantCulture)); break;
                case Dictionary<string, object> m:
                    sb.Append('{');
                    var first = true;
                    foreach (var kv in m)
                    {
                        if (!first) sb.Append(',');
                        first = false;
                        Ser(kv.Key, sb);
                        sb.Append(':');
                        Ser(kv.Value, sb);
                    }
                    sb.Append('}');
                    break;
                case List<object> l:
                    sb.Append('[');
                    var f2 = true;
                    foreach (var e in l)
                    {
                        if (!f2) sb.Append(',');
                        f2 = false;
                        Ser(e, sb);
                    }
                    sb.Append(']');
                    break;
                default: Ser(v.ToString(), sb); break;
            }
        }

        public static object Deserialize(string s)
        {
            int i = 0;
            return Parse(s, ref i);
        }

        private static object Parse(string s, ref int i)
        {
            Skip(s, ref i);
            if (i >= s.Length) return null;
            return s[i] switch
            {
                '{' => ParseObj(s, ref i),
                '[' => ParseArr(s, ref i),
                '"' => ParseStr(s, ref i),
                't' => ParseLit(s, ref i, "true"),
                'f' => ParseLit(s, ref i, "false"),
                'n' => ParseLit(s, ref i, "null"),
                _ => ParseNum(s, ref i),
            };
        }

        private static Dictionary<string, object> ParseObj(string s, ref int i)
        {
            var m = new Dictionary<string, object>();
            i++; Skip(s, ref i);
            while (i < s.Length && s[i] != '}')
            {
                var k = ParseStr(s, ref i);
                Skip(s, ref i); i++; // :
                m[k] = Parse(s, ref i);
                Skip(s, ref i);
                if (i < s.Length && s[i] == ',') i++;
                Skip(s, ref i);
            }
            i++;
            return m;
        }

        private static List<object> ParseArr(string s, ref int i)
        {
            var l = new List<object>();
            i++; Skip(s, ref i);
            while (i < s.Length && s[i] != ']')
            {
                l.Add(Parse(s, ref i));
                Skip(s, ref i);
                if (i < s.Length && s[i] == ',') i++;
                Skip(s, ref i);
            }
            i++;
            return l;
        }

        private static string ParseStr(string s, ref int i)
        {
            i++; // "
            var sb = new StringBuilder();
            while (i < s.Length && s[i] != '"')
            {
                if (s[i] == '\\' && i + 1 < s.Length) { i++; sb.Append(s[i]); }
                else sb.Append(s[i]);
                i++;
            }
            i++;
            return sb.ToString();
        }

        private static object ParseLit(string s, ref int i, string lit)
        {
            i += lit.Length;
            return lit switch { "true" => true, "false" => false, _ => null };
        }

        private static object ParseNum(string s, ref int i)
        {
            var start = i;
            while (i < s.Length && s[i] != ',' && s[i] != '}' && s[i] != ']') i++;
            var raw = s.Substring(start, i - start);
            if (long.TryParse(raw, out var l)) return l;
            if (double.TryParse(raw, System.Globalization.NumberStyles.Float,
                System.Globalization.CultureInfo.InvariantCulture, out var d)) return d;
            return raw;
        }

        private static void Skip(string s, ref int i)
        {
            while (i < s.Length && (s[i] == ' ' || s[i] == '\n' || s[i] == '\t' || s[i] == '\r')) i++;
        }
    }
}

import Foundation

/// Universal Engagement Platform — iOS/macOS SDK.
/// Thin by design: the server owns every decision.
public final class UEPClient {
    private let baseUrl: String
    private let scope: String
    private let auth: String
    private var actorId: String
    private let session: URLSession

    public init(baseUrl: String, apiKey: String, projectId: String, environmentId: String, userId: String) {
        self.baseUrl = baseUrl
        self.scope = "/v1/projects/\(projectId)/environments/\(environmentId)"
        self.auth = "Bearer \(apiKey)"
        self.actorId = userId
        self.session = .shared
    }

    public struct IngestResult: Decodable {
        public let event_id: String
        public let status: String
    }

    private struct IngestResponse: Decodable {
        let results: [IngestResult]
    }

    /// Track one event (idempotency key generated per call).
    public func track(eventType: String, payload: [String: Any] = [:], completion: @escaping (Result<IngestResult, Error>) -> Void) {
        let body: [String: Any] = [
            "events": [[
                "event_type": eventType,
                "actor_id": actorId,
                "occurred_at": ISO8601DateFormatter().string(from: Date()),
                "idempotency_key": "idem_\(UUID().uuidString.lowercased())",
                "payload": payload,
            ]],
        ]
        post(path: "\(scope)/events", body: body) { result in
            switch result {
            case .success(let data):
                guard let data, let decoded = try? JSONDecoder().decode(IngestResponse.self, from: data),
                      let first = decoded.results.first else {
                    completion(.success(IngestResult(event_id: "", status: "inserted")))
                    return
                }
                completion(.success(first))
            case .failure(let error):
                completion(.failure(error))
            }
        }
    }

    /// Full player state (typed as flexible JSON — mirror of the REST shape).
    public func getState(userId: String? = nil, completion: @escaping (Result<[String: Any], Error>) -> Void) {
        get(path: "\(scope)/users/\(userId ?? actorId)/state") { result in
            completion(result.flatMap { data -> Result<[String: Any], Error> in
                guard let data, let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else {
                    return .failure(UEPError.badResponse)
                }
                return .success(obj)
            })
        }
    }

    /// Leaderboard page including the caller's row.
    public func getLeaderboard(leaderboardId: String, me: String? = nil, limit: Int = 50, completion: @escaping (Result<[String: Any], Error>) -> Void) {
        let who = me ?? actorId
        get(path: "\(scope)/leaderboards/\(leaderboardId)?me=\(who)&limit=\(limit)") { result in
            completion(result.flatMap { data -> Result<[String: Any], Error> in
                guard let data, let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else {
                    return .failure(UEPError.badResponse)
                }
                return .success(obj)
            })
        }
    }

    /// Decision trace for one event.
    public func getTrace(eventId: String, completion: @escaping (Result<[String: Any], Error>) -> Void) {
        get(path: "\(scope)/events/\(eventId)/trace") { result in
            completion(result.flatMap { data -> Result<[String: Any], Error> in
                guard let data, let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else {
                    return .failure(UEPError.badResponse)
                }
                return .success(obj)
            })
        }
    }

    public func setUserId(_ userId: String) { actorId = userId }

    // MARK: - plumbing

    private enum UEPError: Error { case badResponse }

    private func get(path: String, completion: @escaping (Result<Data?, Error>) -> Void) {
        request(method: "GET", path: path, body: nil, completion: completion)
    }

    private func post(path: String, body: [String: Any], completion: @escaping (Result<Data?, Error>) -> Void) {
        request(method: "POST", path: path, body: body, completion: completion)
    }

    private func request(method: String, path: String, body: [String: Any]?, completion: @escaping (Result<Data?, Error>) -> Void) {
        guard let url = URL(string: baseUrl + path) else {
            completion(.failure(URLError(.badURL)))
            return
        }
        var req = URLRequest(url: url)
        req.httpMethod = method
        req.setValue(auth, forHTTPHeaderField: "Authorization")
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        if let body {
            req.httpBody = try? JSONSerialization.data(withJSONObject: body)
        }
        session.dataTask(with: req) { data, response, error in
            if let error { completion(.failure(error)); return }
            let status = (response as? HTTPURLResponse)?.statusCode ?? 0
            if (200..<300).contains(status) { completion(.success(data)) }
            else { completion(.failure(URLError(.badServerResponse))) }
        }.resume()
    }
}

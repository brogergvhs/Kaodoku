import Foundation
import Security

enum APIError: LocalizedError {
  case badURL
  case unauthorized
  case server(String)

  var errorDescription: String? {
    switch self {
    case .badURL: "Invalid server URL"
    case .unauthorized: "Signed out by the server — reconnect"
    case let .server(msg): msg
    }
  }
}

/// APIClient talks to one Kaodoku server. URLSession's URLCache honors the
/// server's ETag/Cache-Control, so page and cover images cache for free.
struct APIClient {
  var baseURL: URL
  var token: String?

  private static let redirectGuard = RedirectGuard()
  private static let session: URLSession = {
    let cfg = URLSessionConfiguration.default
    cfg.urlCache = URLCache(memoryCapacity: 64 << 20, diskCapacity: 512 << 20)
    cfg.requestCachePolicy = .useProtocolCachePolicy
    cfg.timeoutIntervalForRequest = 15
    return URLSession(configuration: cfg, delegate: redirectGuard, delegateQueue: nil)
  }()

  static func clearCache() {
    session.configuration.urlCache?.removeAllCachedResponses()
  }

  private static let decoder: JSONDecoder = {
    let d = JSONDecoder()
    d.keyDecodingStrategy = .convertFromSnakeCase
    return d
  }()

  private func request(_ method: String, _ path: String, body: (any Encodable)? = nil) throws -> URLRequest {
    guard let url = URL(string: path, relativeTo: baseURL),
          isAllowedServerURL(url) else { throw APIError.badURL }
    var req = URLRequest(url: url)
    req.httpMethod = method
    if let token, sameOrigin(url, baseURL) {
      req.setValue(token, forHTTPHeaderField: "X-API-Key")
    }
    if let body {
      req.setValue("application/json", forHTTPHeaderField: "Content-Type")
      let enc = JSONEncoder()
      enc.keyEncodingStrategy = .convertToSnakeCase
      enc.dateEncodingStrategy = .iso8601
      req.httpBody = try enc.encode(body)
    }
    return req
  }

  func data(_ method: String = "GET", _ path: String, body: (any Encodable)? = nil) async throws -> Data {
    let (data, resp) = try await Self.session.data(for: request(method, path, body: body))
    guard let http = resp as? HTTPURLResponse else { throw APIError.server("no response") }
    switch http.statusCode {
    case 200 ..< 300: return data
    case 401: throw APIError.unauthorized
    default:
      let msg = (try? Self.decoder.decode(APIErrorBody.self, from: data))?.error
      throw APIError.server(msg ?? "HTTP \(http.statusCode)")
    }
  }

  /// download streams a large CBZ to a temp file, reporting 0…1 progress
  /// against the response length (expectedBytes covers a stripped length).
  func download(_ path: String, expectedBytes: Int64 = 0,
                progress: @escaping @Sendable (Double) -> Void = { _ in }) async throws -> URL
  {
    let (bytes, resp) = try await Self.session.bytes(for: request("GET", path))
    guard let http = resp as? HTTPURLResponse else { throw APIError.server("no response") }
    switch http.statusCode {
    case 200 ..< 300: break
    case 401: throw APIError.unauthorized
    default: throw APIError.server("HTTP \(http.statusCode)")
    }
    let total = resp.expectedContentLength > 0 ? resp.expectedContentLength : expectedBytes
    let tmp = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    FileManager.default.createFile(atPath: tmp.path, contents: nil)
    let handle = try FileHandle(forWritingTo: tmp)
    defer { try? handle.close() }
    var buffer = Data(); buffer.reserveCapacity(1 << 17)
    var written: Int64 = 0
    for try await byte in bytes {
      buffer.append(byte)
      if buffer.count >= 1 << 17 {
        try handle.write(contentsOf: buffer)
        written += Int64(buffer.count)
        buffer.removeAll(keepingCapacity: true)
        if total > 0 {
          progress(Double(written) / Double(total))
        }
      }
    }
    try handle.write(contentsOf: buffer)
    progress(1)
    return tmp
  }

  func get<T: Decodable>(_ path: String) async throws -> T {
    try await Self.decoder.decode(T.self, from: data("GET", path))
  }

  func post<T: Decodable>(_ path: String, body: (any Encodable)? = nil) async throws -> T {
    try await Self.decoder.decode(T.self, from: data("POST", path, body: body))
  }

  func put<T: Decodable>(_ path: String, body: (any Encodable)? = nil) async throws -> T {
    try await Self.decoder.decode(T.self, from: data("PUT", path, body: body))
  }
}

/// sameOrigin compares scheme, host, and port — the token is bound to the exact
/// configured origin, so an http downgrade or a different port doesn't match.
func sameOrigin(_ a: URL, _ b: URL) -> Bool {
  a.scheme?.lowercased() == b.scheme?.lowercased()
    && a.host?.lowercased() == b.host?.lowercased()
    && (a.port ?? defaultPort(a.scheme)) == (b.port ?? defaultPort(b.scheme))
}

func isAllowedServerURL(_ url: URL) -> Bool {
  guard let scheme = url.scheme?.lowercased(), let host = url.host, !host.isEmpty else { return false }
  return scheme == "https" || (scheme == "http" && isPrivateHost(host))
}

func isPrivateHost(_ rawHost: String?) -> Bool {
  guard var host = rawHost?.lowercased(), !host.isEmpty else { return false }
  if host.first == "[", host.last == "]" {
    host = String(host.dropFirst().dropLast())
  }
  if let zone = host.firstIndex(of: "%") {
    host = String(host[..<zone])
  }
  if host.hasSuffix(".") {
    host.removeLast()
  }
  if host == "localhost" || host.hasSuffix(".local") || host == "::1" {
    return true
  }
  if host.hasPrefix("::ffff:") {
    return isPrivateHost(String(host.dropFirst(7)))
  }
  if host.contains(":") {
    return host.hasPrefix("fc") || host.hasPrefix("fd")
      || ["fe8", "fe9", "fea", "feb"].contains { host.hasPrefix($0) }
  }
  let parts = host.split(separator: ".", omittingEmptySubsequences: false)
  if parts.count == 4 {
    let octets = parts.compactMap { Int($0) }
    guard octets.count == 4, octets.allSatisfy((0 ... 255).contains) else { return false }
    switch (octets[0], octets[1]) {
    case (127, _), (10, _), (192, 168), (169, 254): return true
    case (172, 16 ... 31): return true
    default: return false
    }
  }
  return !host.contains(".")
}

private func defaultPort(_ scheme: String?) -> Int {
  scheme?.lowercased() == "https" ? 443 : 80
}

/// RedirectGuard drops the X-API-Key header when a redirect crosses to a
/// different origin, so a malicious/compromised server can't replay the token.
final class RedirectGuard: NSObject, URLSessionTaskDelegate {
  func urlSession(_: URLSession, task: URLSessionTask,
                  willPerformHTTPRedirection _: HTTPURLResponse, newRequest request: URLRequest,
                  completionHandler: @escaping (URLRequest?) -> Void)
  {
    guard let dest = request.url, isAllowedServerURL(dest) else {
      completionHandler(nil)
      return
    }
    guard let orig = task.originalRequest?.url, !sameOrigin(orig, dest) else {
      completionHandler(request)
      return
    }
    var stripped = request
    stripped.setValue(nil, forHTTPHeaderField: "X-API-Key")
    completionHandler(stripped)
  }
}

final class NoRedirect: NSObject, URLSessionTaskDelegate, Sendable {
  static let shared = NoRedirect()
  func urlSession(_: URLSession, task _: URLSessionTask,
                  willPerformHTTPRedirection _: HTTPURLResponse, newRequest _: URLRequest,
                  completionHandler: @escaping (URLRequest?) -> Void)
  {
    completionHandler(nil)
  }
}

/// Keychain stores the device API token.
enum Keychain {
  private static func query(_ account: String) -> [String: Any] {
    [kSecClass as String: kSecClassGenericPassword,
     kSecAttrService as String: "com.kaodoku.ios",
     kSecAttrAccount as String: account]
  }

  static func save(_ value: String, account: String) {
    var q = query(account)
    SecItemDelete(q as CFDictionary)
    q[kSecValueData as String] = Data(value.utf8)
    // ThisDeviceOnly keeps the token out of backups and iCloud Keychain.
    q[kSecAttrAccessible as String] = kSecAttrAccessibleWhenUnlockedThisDeviceOnly
    SecItemAdd(q as CFDictionary, nil)
  }

  static func load(account: String) -> String? {
    var q = query(account)
    q[kSecReturnData as String] = true
    q[kSecMatchLimit as String] = kSecMatchLimitOne
    var out: AnyObject?
    guard SecItemCopyMatching(q as CFDictionary, &out) == errSecSuccess,
          let data = out as? Data else { return nil }
    return String(data: data, encoding: .utf8)
  }

  static func delete(account: String) {
    SecItemDelete(query(account) as CFDictionary)
  }
}

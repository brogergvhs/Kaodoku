import Foundation
import Network
import Observation
import UIKit

/// SavedServer is a named connection the user can pick on the connect screen.
struct SavedServer: Codable, Identifiable, Hashable {
  var id = UUID()
  var name: String
  var localURL: URL?
  var publicURL: URL?
  var mode: ServerEndpoints.Mode = .auto
}

/// ServerEndpoints stores up to two addresses for the same server: a LAN one
/// and a public one. instanceID  proves they are the same installation.
struct ServerEndpoints: Codable, Equatable {
  enum Mode: String, Codable { case auto, local, external }
  var localURL: URL? = nil
  var publicURL: URL? = nil
  var instanceID: String
  var mode: Mode = .auto

  var verifiable: Bool {
    guard let l = localURL, let p = publicURL else { return false }
    return !sameOrigin(l, p)
  }

  var wantsProbe: Bool {
    verifiable && mode != .external
  }

  enum Slot { case local, external }

  func slot(of url: URL) -> Slot? {
    guard verifiable else { return nil }
    if let l = localURL, sameOrigin(url, l) {
      return .local
    }
    if let p = publicURL, sameOrigin(url, p) {
      return .external
    }
    return nil
  }

  func activeURL(localVerified: Bool) -> URL? {
    switch mode {
    case .external:
      return publicURL ?? localURL
    case .local:
      if verifiable, !localVerified {
        return publicURL
      }
      return localURL ?? publicURL
    case .auto:
      return localVerified ? (localURL ?? publicURL) : (publicURL ?? localURL)
    }
  }
}

/// AppState holds the connection to one server: URL in UserDefaults, token in
/// the Keychain. connected == usable client (token present or single-user).
@Observable @MainActor
final class AppState {
  enum LibraryNav: Equatable {
    case title(Int64)
    case root
  }

  var api: APIClient?
  var me: Me?
  var settings = UserSettings()
  var errorMessage: String?
  var tab = 0
  var libraryNav: LibraryNav?
  let store = LocalStore()
  var endpoints: ServerEndpoints? {
    didSet {
      if let endpoints {
        UserDefaults.standard.set(try? JSONEncoder().encode(endpoints), forKey: Self.endpointsKey)
      } else {
        UserDefaults.standard.removeObject(forKey: Self.endpointsKey)
      }
    }
  }

  private static let endpointsKey = "server_endpoints"
  private static let settingsKey = "user_settings"
  private static let modeOverridesKey = "reader_mode_overrides"
  private static let savedServersKey = "saved_servers"
  private static let readerWarmthKey = "reader_warmth"
  private static let readerDimKey = "reader_dim"
  private static let readerWhiteOnlyKey = "reader_white_only"
  private static let readerTrimKey = "reader_trim_borders"

  var savedServers: [SavedServer] =
    (UserDefaults.standard.data(forKey: savedServersKey))
      .flatMap { try? JSONDecoder().decode([SavedServer].self, from: $0) } ?? []

  func saveServer(_ server: SavedServer) {
    if let i = savedServers.firstIndex(where: { $0.id == server.id }) {
      savedServers[i] = server
    } else {
      savedServers.append(server)
    }
    persistServers()
  }

  func deleteServer(_ id: UUID) {
    savedServers.removeAll { $0.id == id }
    persistServers()
  }

  private func persistServers() {
    UserDefaults.standard.set(try? JSONEncoder().encode(savedServers), forKey: Self.savedServersKey)
  }

  private static let deviceIDKey = "device_id"

  /// Stable per-install id; the server keeps one active token per install.
  static var deviceID: String {
    if let id = UserDefaults.standard.string(forKey: deviceIDKey) {
      return id
    }
    let id = UUID().uuidString
    UserDefaults.standard.set(id, forKey: deviceIDKey)
    return id
  }

  /// Per-title reader mode.
  var readerModeOverrides: [String: String] =
    UserDefaults.standard.dictionary(forKey: modeOverridesKey) as? [String: String] ?? [:]

  func readerMode(forTitle id: Int64) -> String? {
    readerModeOverrides["\(id)"]
  }

  func setReaderMode(_ mode: String?, forTitle id: Int64) {
    if let mode {
      readerModeOverrides["\(id)"] = mode
    } else {
      readerModeOverrides.removeValue(forKey: "\(id)")
    }
    UserDefaults.standard.set(readerModeOverrides, forKey: Self.modeOverridesKey)
  }

  var readerWarmth = UserDefaults.standard.double(forKey: readerWarmthKey) {
    didSet { UserDefaults.standard.set(readerWarmth, forKey: Self.readerWarmthKey) }
  }

  var readerDim = UserDefaults.standard.double(forKey: readerDimKey) {
    didSet { UserDefaults.standard.set(readerDim, forKey: Self.readerDimKey) }
  }

  /// When on, warmth/dim only touch white/near-white pixels so colored pages
  /// keep their hues.
  var readerWhiteOnly = UserDefaults.standard.bool(forKey: readerWhiteOnlyKey) {
    didSet { UserDefaults.standard.set(readerWhiteOnly, forKey: Self.readerWhiteOnlyKey) }
  }

  /// Cut server-detected page borders in the paged reader.
  var readerTrimBorders = UserDefaults.standard.bool(forKey: readerTrimKey) {
    didSet { UserDefaults.standard.set(readerTrimBorders, forKey: Self.readerTrimKey) }
  }

  var readerTrimStrength =
    (UserDefaults.standard.object(forKey: "reader_trim_strength") as? Double) ?? 1
  {
    didSet { UserDefaults.standard.set(readerTrimStrength, forKey: "reader_trim_strength") }
  }

  var readerTrimMargin =
    (UserDefaults.standard.object(forKey: "reader_trim_margin") as? Double) ?? 0.01
  {
    didSet { UserDefaults.standard.set(readerTrimMargin, forKey: "reader_trim_margin") }
  }

  var readerComfort: ComfortParams {
    ComfortParams(warmth: readerWarmth, dim: readerDim, whiteOnly: readerWhiteOnly)
  }

  private static let tokenAccount = "api_token"
  private var settingsDirty = false
  private var settingsTask: Task<Void, Never>?
  private var reselectTask: Task<Void, Never>?
  private var flushTask: Task<Void, Never>?
  private let pathMonitor = NWPathMonitor()
  private let pathQueue = DispatchQueue(label: "kaodoku.netpath")

  private(set) var online = true

  var activeSlot: ServerEndpoints.Slot? {
    guard let api else { return nil }
    return endpoints?.slot(of: api.baseURL)
  }

  var connected: Bool {
    api != nil
  }

  init() {
    if let data = UserDefaults.standard.data(forKey: Self.endpointsKey),
       let stored = try? JSONDecoder().decode(ServerEndpoints.self, from: data)
    {
      endpoints = stored
    }
    if let url = endpoints?.activeURL(localVerified: false) {
      api = APIClient(baseURL: url, token: Keychain.load(account: Self.tokenAccount))
    }
    if let data = UserDefaults.standard.data(forKey: Self.settingsKey),
       let cached = try? JSONDecoder().decode(UserSettings.self, from: data)
    {
      settings = cached
    }
    pathMonitor.pathUpdateHandler = { [weak self] _ in
      Task { @MainActor in
        guard let self else { return }
        self.online = self.pathMonitor.currentPath.status == .satisfied
        self.scheduleReselect()
      }
    }
    pathMonitor.start(queue: pathQueue)
    Task { [weak self] in await self?.reselect() }
  }

  func scheduleReselect() {
    reselectTask?.cancel()
    reselectTask = Task {
      try? await Task.sleep(for: .seconds(1))
      guard !Task.isCancelled else { return }
      await reselect()
      scheduleFlush()
    }
  }

  /// scheduleFlush uploads queued reading progress shortly after a page is
  /// read, so online devices stay in step with the server within seconds.
  /// Marks recorded while the task sleeps ride along in the same batch.
  func scheduleFlush() {
    guard api != nil, online, flushTask == nil else { return }
    flushTask = Task {
      try? await Task.sleep(for: .seconds(1.5))
      flushTask = nil
      guard !Task.isCancelled, online, let api else { return }
      await store.flush(api)
    }
  }

  /// reselect probes the local address and switches the client.
  func reselect() async {
    guard let e = endpoints, api != nil else { return }
    var localVerified = false
    if e.wantsProbe, let local = e.localURL {
      localVerified = await Self.fetchInstanceID(local) == e.instanceID
    }
    guard !Task.isCancelled, e == endpoints else { return }
    guard let url = e.activeURL(localVerified: localVerified),
          let current = api, !sameOrigin(current.baseURL, url) else { return }
    api = APIClient(baseURL: url, token: current.token)
  }

  nonisolated static func fetchInstanceID(_ base: URL) async -> String? {
    guard let url = URL(string: "/api/v1/meta", relativeTo: base), isAllowedServerURL(url) else { return nil }
    var req = URLRequest(url: url)
    req.timeoutInterval = 2
    guard let (data, resp) = try? await URLSession.shared.data(for: req, delegate: NoRedirect.shared),
          let http = resp as? HTTPURLResponse, http.statusCode == 200,
          let respURL = http.url, sameOrigin(respURL, url) else { return nil }
    let decoder = JSONDecoder()
    decoder.keyDecodingStrategy = .convertFromSnakeCase
    return (try? decoder.decode(Meta.self, from: data))?.instanceId
  }

  func connect(server: String, username: String, password: String) async -> Bool {
    errorMessage = nil
    let server = server.trimmingCharacters(in: .whitespaces)
    guard var url = URL(string: server) else {
      errorMessage = APIError.badURL.localizedDescription
      return false
    }
    if url.scheme == nil {
      url = URL(string: "https://\(server)") ?? url
    }
    guard isAllowedServerURL(url) else {
      errorMessage = "Use HTTPS, or HTTP only for a private local server."
      return false
    }
    guard let id = await authenticate(url: url, username: username, password: password) else { return false }
    var e = endpoints?.instanceID == id ? endpoints! : ServerEndpoints(instanceID: id)
    if isPrivateHost(url.host) {
      e.localURL = url
    } else {
      e.publicURL = url
    }
    endpoints = e
    recordKnownServer(id, name: url.host ?? id)
    return true
  }

  func connect(saved: SavedServer, username: String, password: String) async -> Bool {
    errorMessage = nil
    let primary: URL? = if saved.mode == .external {
      saved.publicURL
    } else if let local = saved.localURL, await Self.fetchInstanceID(local) != nil {
      local
    } else {
      saved.publicURL ?? saved.localURL
    }
    guard let url = primary, isAllowedServerURL(url) else {
      errorMessage = "This server has no reachable address for the selected mode."
      return false
    }
    guard let id = await authenticate(url: url, username: username, password: password) else { return false }
    var local = saved.localURL
    var external = saved.publicURL
    for other in [saved.localURL, saved.publicURL] {
      guard let other, other != url, await Self.fetchInstanceID(other) != id else { continue }
      if other == saved.localURL {
        local = nil
      }
      if other == saved.publicURL {
        external = nil
      }
    }
    endpoints = ServerEndpoints(localURL: local, publicURL: external, instanceID: id, mode: saved.mode)
    recordKnownServer(id, name: saved.name)
    return true
  }

  private func authenticate(url: URL, username: String, password: String) async -> String? {
    do {
      var client = APIClient(baseURL: url, token: nil)
      let meta: Meta = try await client.get("/api/v1/meta")
      guard let id = meta.instanceId, !id.isEmpty else {
        errorMessage = "This server doesn't report an instance ID — update the server."
        return nil
      }
      if meta.authRequired {
        guard !username.isEmpty else {
          errorMessage = "This server requires a username and password"
          return nil
        }
        let login: LoginResponse = try await client.post("/api/v1/auth/login", body: [
          "username": username, "password": password,
          "device_name": "iOS app · \(UIDevice.current.model)",
          "device_id": Self.deviceID,
        ])
        client.token = login.token
        me = login.me
        Keychain.save(login.token, account: Self.tokenAccount)
      } else {
        me = try await client.get("/api/v1/me")
      }
      api = client
      return id
    } catch {
      errorMessage = error.localizedDescription
      return nil
    }
  }

  func loadSession() async {
    guard let api else { return }
    await store.activate(endpoints?.instanceID)
    do {
      if me == nil {
        me = try await api.get("/api/v1/me")
      }
      settings = try await api.get("/api/v1/me/settings")
      cacheSettings()
      await store.flush(api)
    } catch APIError.unauthorized {
      signOut()
    } catch {
      errorMessage = error.localizedDescription
    }
  }

  /// bootstrapStore loads the right server's downloads at launch.
  func bootstrapStore() async {
    await store.load(instance: endpoints?.instanceID)
    if !connected, store.titles.isEmpty, let alt = downloadedInstances().first {
      await store.activate(alt)
    }
  }

  var knownServers: [String: String] =
    UserDefaults.standard.dictionary(forKey: "known_servers") as? [String: String] ?? [:]

  func serverName(_ instance: String) -> String {
    knownServers[instance] ?? instance
  }

  private func recordKnownServer(_ instance: String, name: String) {
    knownServers[instance] = name
    UserDefaults.standard.set(knownServers, forKey: "known_servers")
  }

  func downloadedInstances() -> [String] {
    let files = FileManager.default
    let dirs = (try? files.contentsOfDirectory(at: LocalStore.root, includingPropertiesForKeys: [.isDirectoryKey])) ?? []
    return dirs.compactMap { url -> String? in
      let id = url.lastPathComponent
      guard (try? url.resourceValues(forKeys: [.isDirectoryKey]))?.isDirectory == true else { return nil }
      if files.fileExists(atPath: LocalStore.indexURL(id).path) {
        return id
      }
      let contents = (try? files.contentsOfDirectory(atPath: url.path)) ?? []
      return contents.contains { !$0.hasPrefix(".") } ? id : nil
    }
  }

  func saveSettings() {
    cacheSettings()
    guard let api else { return }
    settingsDirty = true
    guard settingsTask == nil else { return }
    settingsTask = Task {
      while settingsDirty, !Task.isCancelled {
        settingsDirty = false
        let snapshot = settings
        _ = try? await api.data("PUT", "/api/v1/me/settings", body: snapshot)
      }
      settingsTask = nil
    }
  }

  private func cacheSettings() {
    UserDefaults.standard.set(try? JSONEncoder().encode(settings), forKey: Self.settingsKey)
  }

  func signOut() {
    settingsDirty = false
    settingsTask?.cancel()
    reselectTask?.cancel()
    flushTask?.cancel()
    flushTask = nil
    if let api, api.token != nil {
      Task { _ = try? await api.data("DELETE", "/api/v1/auth/token") }
    }
    Keychain.delete(account: Self.tokenAccount)
    UserDefaults.standard.removeObject(forKey: Self.settingsKey)
    APIClient.clearCache()
    endpoints = nil
    api = nil
    me = nil
  }
}

import SwiftUI

/// SavedServersList renders the saved servers, marking the active one.
struct SavedServersList: View {
  @Environment(AppState.self) private var app
  var onSelect: ((SavedServer) -> Void)?
  @Binding var editing: SavedServer?

  var body: some View {
    if app.savedServers.isEmpty {
      Text("No saved servers yet.").foregroundStyle(.secondary)
    }
    ForEach(app.savedServers) { server in
      HStack {
        info(server)
          .contentShape(Rectangle())
          .onTapGesture { onSelect?(server) }
        Spacer()
        Button { editing = server } label: { Image(systemName: "pencil") }
          .buttonStyle(.borderless)
      }
      .swipeActions {
        Button("Delete", role: .destructive) { app.deleteServer(server.id) }
      }
    }
  }

  private func info(_ s: SavedServer) -> some View {
    let hosts = [s.localURL?.host, s.publicURL?.host].compactMap(\.self).joined(separator: " · ")
    return VStack(alignment: .leading, spacing: 2) {
      HStack(spacing: 6) {
        Text(s.name).foregroundStyle(.primary)
        if isActive(s) {
          Image(systemName: "checkmark.circle.fill").font(.caption).foregroundStyle(Theme.success)
        }
      }
      if !hosts.isEmpty {
        Text(hosts).font(.caption).foregroundStyle(.secondary)
      }
    }
  }

  private func isActive(_ s: SavedServer) -> Bool {
    (s.localURL != nil || s.publicURL != nil)
      && s.localURL == app.endpoints?.localURL && s.publicURL == app.endpoints?.publicURL
  }
}

struct ConnectView: View {
  @Environment(AppState.self) private var app
  @State private var loggingIn: SavedServer?
  @State private var editing: SavedServer?
  @State private var showManual = false

  var body: some View {
    NavigationStack {
      Form {
        Group {
          Section("Servers") {
            SavedServersList(onSelect: { loggingIn = $0 }, editing: $editing)
            Button("Add a server", systemImage: "plus") { editing = SavedServer(name: "") }
          }
          if let msg = app.errorMessage {
            Text(msg).foregroundStyle(Theme.error)
          }
          Section {
            DisclosureGroup("Connect manually", isExpanded: $showManual) {
              ManualConnectForm()
            }
          }
        }
        .nordRows()
      }
      .nordScreen()
      .navigationTitle("Kaodoku")
    }
    .sheet(item: $loggingIn) { ServerLoginSheet(server: $0) }
    .sheet(item: $editing) { ServerEditSheet(server: $0) }
  }
}

struct ServerLoginSheet: View {
  @Environment(AppState.self) private var app
  @Environment(\.dismiss) private var dismiss
  let server: SavedServer
  @State private var username = ""
  @State private var password = ""
  @State private var busy = false
  @State private var error: String?

  var body: some View {
    NavigationStack {
      Form {
        Group {
          Section("Account (if the server requires sign-in)") {
            TextField("Username", text: $username)
              .textInputAutocapitalization(.never).autocorrectionDisabled()
            SecureField("Password", text: $password)
          }
          if let error {
            Text(error).foregroundStyle(Theme.error)
          }
          Button {
            busy = true
            Task {
              let ok = await app.connect(saved: server, username: username, password: password)
              busy = false
              if ok {
                dismiss()
              } else {
                error = app.errorMessage; app.errorMessage = nil
              }
            }
          } label: {
            if busy {
              ProgressView()
            } else {
              Text("Connect")
            }
          }
          .disabled(busy)
        }
        .nordRows()
      }
      .nordScreen()
      .navigationTitle(server.name)
      .navigationBarTitleDisplayMode(.inline)
      .toolbar { Button("Cancel") { dismiss() } }
    }
  }
}

enum AddressCheck: Equatable { case unknown, checking, ok, bad }

struct ServerEditSheet: View {
  @Environment(AppState.self) private var app
  @Environment(\.dismiss) private var dismiss
  @State var server: SavedServer
  @State private var local = ""
  @State private var external = ""
  @State private var localCheck = AddressCheck.unknown
  @State private var externalCheck = AddressCheck.unknown
  @State private var error: String?
  @State private var verifying = false

  private var active: Bool {
    (server.localURL != nil || server.publicURL != nil)
      && server.localURL == app.endpoints?.localURL && server.publicURL == app.endpoints?.publicURL
  }

  var body: some View {
    NavigationStack {
      Form {
        Group {
          Section("Name") {
            TextField("Home mini", text: $server.name)
          }
          Section("Addresses") {
            addressRow("Local (http://192.168...)", text: $local, check: $localCheck)
            addressRow("Public (https://...)", text: $external, check: $externalCheck)
          }
          Section("Use") {
            Picker("Use", selection: $server.mode) {
              Text("Auto").tag(ServerEndpoints.Mode.auto)
              Text("Local").tag(ServerEndpoints.Mode.local)
              Text("Public").tag(ServerEndpoints.Mode.external)
            }
            .pickerStyle(.segmented)
            .listRowBackground(Color.clear)
            .listRowInsets(EdgeInsets())
          }
          if active {
            Button("Recheck now") { Task { await app.reselect() } }
          }
          if let error {
            Text(error).foregroundStyle(Theme.error)
          }
        }
        .nordRows()
      }
      .nordScreen()
      .navigationTitle("Server")
      .navigationBarTitleDisplayMode(.inline)
      .toolbar {
        ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
        ToolbarItem(placement: .confirmationAction) {
          Button(verifying ? "Checking..." : "Save") { save() }.disabled(verifying)
        }
      }
      .onAppear {
        local = server.localURL?.absoluteString ?? ""
        external = server.publicURL?.absoluteString ?? ""
      }
    }
  }

  private func addressRow(_ prompt: String, text: Binding<String>, check: Binding<AddressCheck>) -> some View {
    HStack {
      TextField(prompt, text: text)
        .keyboardType(.URL).textInputAutocapitalization(.never).autocorrectionDisabled()
        .onChange(of: text.wrappedValue) { check.wrappedValue = .unknown }
      switch check.wrappedValue {
      case .checking: ProgressView()
      case .ok: Image(systemName: "checkmark.circle.fill").foregroundStyle(Theme.success)
      case .bad: Image(systemName: "xmark.circle.fill").foregroundStyle(Theme.error)
      case .unknown: EmptyView()
      }
      Button("Check") {
        Task { check.wrappedValue = await verify(text.wrappedValue) }
      }
      .buttonStyle(.borderless)
      .disabled(text.wrappedValue.trimmingCharacters(in: .whitespaces).isEmpty)
    }
  }

  private func verify(_ raw: String) async -> AddressCheck {
    guard case let .some(url?) = parse(raw) else { return .bad }
    return await AppState.fetchInstanceID(url) != nil ? .ok : .bad
  }

  private func save() {
    guard !server.name.trimmingCharacters(in: .whitespaces).isEmpty else {
      error = "Give the server a name."
      return
    }
    guard let localURL = parse(local), let publicURL = parse(external) else {
      error = "Use HTTPS, or HTTP only for a private local address."
      return
    }
    guard localURL != nil || publicURL != nil else {
      error = "Add at least one address."
      return
    }
    if let publicURL, isPrivateHost(publicURL.host) {
      error = "The public address must be reachable from outside."
      return
    }
    let wasActive = active
    verifying = true
    Task {
      for url in [localURL, publicURL].compactMap(\.self)
        where await AppState.fetchInstanceID(url) == nil
      {
        verifying = false
        error = "\(url.host ?? "The address") is not a reachable Kaodoku server."
        return
      }
      verifying = false
      server.localURL = localURL
      server.publicURL = publicURL
      app.saveServer(server)
      if wasActive {
        app.endpoints?.mode = server.mode
        app.scheduleReselect()
      }
      dismiss()
    }
  }

  private func parse(_ raw: String) -> URL?? {
    let raw = raw.trimmingCharacters(in: .whitespaces)
    if raw.isEmpty {
      return .some(nil)
    }
    var url = URL(string: raw)
    if url?.scheme == nil {
      url = URL(string: "https://\(raw)")
    }
    guard let url, isAllowedServerURL(url) else { return nil }
    return url
  }
}

struct ManualConnectForm: View {
  @Environment(AppState.self) private var app
  @State private var server = ""
  @State private var username = ""
  @State private var password = ""
  @State private var busy = false

  var body: some View {
    TextField("http://192.168.1.10:8080", text: $server)
      .keyboardType(.URL).textInputAutocapitalization(.never).autocorrectionDisabled()
    TextField("Username", text: $username)
      .textInputAutocapitalization(.never).autocorrectionDisabled()
    SecureField("Password", text: $password)
    Button {
      busy = true
      Task {
        _ = await app.connect(server: server, username: username, password: password)
        busy = false
      }
    } label: {
      if busy {
        ProgressView()
      } else {
        Text("Connect")
      }
    }
    .disabled(server.isEmpty || busy)
  }
}

import SwiftUI

// SettingsView: reader preferences (synced to /me/settings, cached locally),
// account/server info, AniList status, device storage, and about.
struct SettingsView: View {
  @Environment(AppState.self) private var app
  @State private var anilist: AniListStatus?
  @State private var meta: Meta?
  @State private var confirmClear = false
  @State private var note: String?
  @State private var editingServer: SavedServer?

  var body: some View {
    @Bindable var app = app
    NavigationStack {
      List {
        Group {
          Section("Reader") {
            Picker("Layout", selection: Binding(
              get: { app.settings.readerMode ?? "paged" },
              set: { app.settings.readerMode = $0; app.saveSettings() }
            )) {
              Text("Paged").tag("paged")
              Text("Long strip").tag("strip")
            }
            Picker("Direction", selection: Binding(
              get: { app.settings.readerDir ?? "ltr" },
              set: { app.settings.readerDir = $0; app.saveSettings() }
            )) {
              Text("Left to right").tag("ltr")
              Text("Right to left").tag("rtl")
            }
          }
          Section("Account") {
            if let me = app.me {
              LabeledContent("User", value: me.user.username)
              LabeledContent("Role", value: me.user.role)
            }
            Button("Sign out", role: .destructive) { app.signOut() }
          }
          serverSection
          Section("AniList") {
            if let anilist {
              LabeledContent("Status", value: anilist.connected ? "Connected" : "Not connected")
              if anilist.connected {
                Button("Sync library now") { syncAniList() }
              } else {
                Text("Connect AniList from the web interface.")
                  .font(.caption).foregroundStyle(.secondary)
              }
            } else {
              LabeledContent("Status", value: "…")
            }
          }
          if app.me?.can("jobs.view") == true {
            Section("Management") {
              NavigationLink("Jobs") { JobsView() }
            }
          }
          Section("Device storage") {
            LabeledContent("Downloads",
                           value: "\(app.store.chapters.count) chapters · \(humanBytes(totalSize))")
            Button("Remove all downloads", role: .destructive) { confirmClear = true }
              .disabled(app.store.chapters.isEmpty)
          }
          Section("About") {
            if let meta {
              LabeledContent("Server version", value: meta.serverVersion)
            }
            LabeledContent("App version",
                           value: Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "dev")
          }
        }
        .nordRows()
      }
      .nordScreen()
      .navigationTitle("Settings")
      .navigationBarTitleDisplayMode(.inline)
      .connectionStatusToolbar()
      .task {
        guard let api = app.api else { return }
        anilist = try? await api.get("/api/v1/anilist")
        meta = try? await api.get("/api/v1/meta")
      }
      .sheet(item: $editingServer) { ServerEditSheet(server: $0) }
      .confirmationDialog("Remove all downloaded chapters from this device?",
                          isPresented: $confirmClear, titleVisibility: .visible)
      {
        Button("Remove all", role: .destructive) {
          for title in app.store.titles {
            app.store.deleteTitle(title.id)
          }
        }
      }
      .overlay(alignment: .bottom) {
        if let note {
          Text(note)
            .font(.footnote)
            .padding(.horizontal, 12).padding(.vertical, 8)
            .background(.thinMaterial, in: Capsule())
            .padding(.bottom, 12)
            .task { try? await Task.sleep(for: .seconds(4)); self.note = nil }
        }
      }
    }
  }

  private var serverSection: some View {
    Section("Servers") {
      NavigationLink("Sync status") { SyncStatusView() }
      SavedServersList(editing: $editingServer)
      Button("Add a server", systemImage: "plus") { editingServer = SavedServer(name: "") }
    }
  }

  private var totalSize: Int64 {
    app.store.titles.reduce(0) { $0 + $1.size }
  }

  private func syncAniList() {
    guard let api = app.api else { return }
    Task {
      do {
        _ = try await api.data("POST", "/api/v1/anilist/sync")
        note = "AniList sync started"
      } catch { note = error.localizedDescription }
    }
  }
}

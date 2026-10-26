import SwiftUI

// SettingsView: reader preferences (synced to /me/settings, cached locally),
// account/server info, tracker status, device storage, and about.
struct SettingsView: View {
  @Environment(AppState.self) private var app
  @State private var trackers: TrackerList?
  @State private var meta: Meta?
  @State private var confirmClear = false
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
          Section("Trackers") {
            if let trackers {
              ForEach(trackers.items) { t in
                let status = t.unavailable != nil ? "Unavailable" : t.connected ? t.name ?? "Connected" : "Not connected"
                if let settings = t.settings {
                  NavigationLink {
                    TrackerSettingsView(tracker: t, settings: settings) { self.trackers = $0 }
                  } label: {
                    LabeledContent(t.label, value: status)
                  }
                } else {
                  LabeledContent(t.label, value: status)
                }
              }
              if trackers.items.count(where: { $0.settings?.source == "tracker" }) >= 2 {
                NavigationLink("Precedence") {
                  TrackerPrecedenceView(trackers: trackers.items.filter(\.connected)) { self.trackers = $0 }
                }
              }
              if !trackers.items.contains(where: \.connected) {
                Text("Connect trackers from the web interface.")
                  .font(.caption).foregroundStyle(.secondary)
              }
            } else {
              LabeledContent("Status", value: "...")
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
        trackers = try? await api.get("/api/v1/trackers")
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
}

struct TrackerSettingsView: View {
  @Environment(AppState.self) private var app
  let tracker: TrackerList.Tracker
  @State var settings: TrackerSettings
  let saved: (TrackerList) -> Void
  @State private var note: String?

  var body: some View {
    List {
      Group {
        Section {
          Toggle("Link new titles automatically", isOn: $settings.autoLink)
          Toggle("Progressive sync for linked titles", isOn: $settings.autoProgressive)
          Toggle("Use its related manga", isOn: $settings.relations)
          Toggle("Use its recommendations", isOn: $settings.recommendations)
          Picker("Sync ratings", selection: $settings.ratingSync) {
            Text("Same as progressive sync").tag("")
            Text("Always").tag("on")
            Text("Never").tag("off")
          }
          Toggle("Kaodoku is the source of truth", isOn: Binding(
            get: { settings.source == "kaodoku" },
            set: { settings.source = $0 ? "kaodoku" : "tracker" }
          ))
        } footer: {
          Text(settings.source == "kaodoku"
            ? "Kaodoku sends progress, favourites and ratings to \(tracker.label)."
            : "Kaodoku takes progress, favourites and ratings from \(tracker.label).")
        }
        Section {
          Toggle("Only Kaodoku updates \(tracker.label)", isOn: Binding(
            get: { settings.exclusive ?? false },
            set: { settings.exclusive = $0 }
          ))
        } footer: {
          Text("Skips reading an entry from \(tracker.label) before each update. "
            + "Leave off if you also update \(tracker.label) from another app or its website.")
        }
        Section {
          Button("Sync \(tracker.label) now", action: sync).disabled(tracker.unavailable != nil)
        } footer: {
          if let note {
            Text(note)
          }
        }
      }
      .nordRows()
    }
    .nordScreen()
    .navigationTitle(tracker.label)
    .onChange(of: settings) {
      guard let api = app.api else { return }
      let body = settings
      Task {
        do {
          let list: TrackerList = try await api.put("/api/v1/trackers/\(tracker.provider)/settings", body: body)
          saved(list)
          note = nil
        } catch { note = error.localizedDescription }
      }
    }
  }

  private func sync() {
    guard let api = app.api else { return }
    Task {
      do {
        _ = try await api.data("POST", "/api/v1/trackers/\(tracker.provider)/sync")
        note = "\(tracker.label) sync started"
      } catch { note = error.localizedDescription }
    }
  }
}

struct TrackerPrecedenceView: View {
  @Environment(AppState.self) private var app
  @State var trackers: [TrackerList.Tracker]
  let saved: (TrackerList) -> Void
  @State private var error: String?

  var body: some View {
    List {
      Section {
        ForEach(trackers) { Text($0.label) }
          .onMove { from, to in
            trackers.move(fromOffsets: from, toOffset: to)
            save()
          }
      } footer: {
        Text(error ?? "When several trackers are the source of truth, the higher one wins.")
      }
      .nordRows()
    }
    .environment(\.editMode, .constant(.active))
    .nordScreen()
    .navigationTitle("Precedence")
  }

  private func save() {
    guard let api = app.api else { return }
    let body = ["providers": trackers.map(\.provider)]
    Task {
      do {
        let list: TrackerList = try await api.put("/api/v1/trackers/order", body: body)
        saved(list)
        error = nil
      } catch { self.error = error.localizedDescription }
    }
  }
}

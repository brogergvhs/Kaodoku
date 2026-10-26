import SwiftUI

/// SyncStatusView shows the connection (online/offline, local vs. remote
/// address) and the reading-progress queue.
struct SyncStatusView: View {
  @Environment(AppState.self) private var app

  var body: some View {
    List {
      Section("Connection") {
        LabeledContent("Network", value: app.online ? "Online" : "Offline")
        if let slot = app.activeSlot {
          LabeledContent("Address in use", value: slot == .local ? "Local" : "Remote")
        }
        if let url = app.api?.baseURL {
          LabeledContent("Server", value: url.absoluteString)
        } else {
          LabeledContent("Server", value: "Not connected")
        }
      }
      .nordRows()

      Section("Reading progress") {
        statusRow
        if let error = app.store.syncError {
          Text(error)
            .font(.caption)
            .foregroundStyle(Theme.error)
        }
        if app.connected {
          Button("Sync now") {
            Task { await app.store.flush(app.api) }
          }
          .disabled(app.store.flushing || app.store.pendingMarks == 0)
        }
      }
      .nordRows()

      if !app.store.queuedMarks.isEmpty {
        Section("Waiting to sync (\(app.store.pendingMarks))") {
          ForEach(app.store.queuedMarks.reversed(), id: \.self) { mark in
            HStack {
              VStack(alignment: .leading, spacing: 2) {
                Text(label(for: mark))
                Text("Page \(mark.page)\(mark.totalPages > 0 ? " of \(mark.totalPages)" : "")")
                  .font(.caption).foregroundStyle(.secondary)
              }
              Spacer()
              Text(mark.readAt, format: .relative(presentation: .named))
                .font(.caption).foregroundStyle(.secondary)
            }
          }
          .nordRows()
        }
      }
    }
    .nordScreen()
    .navigationTitle("Sync")
    .navigationBarTitleDisplayMode(.inline)
  }

  @ViewBuilder private var statusRow: some View {
    if app.store.flushing {
      HStack {
        ProgressView()
        Text("Syncing \(app.store.pendingMarks) marks...")
      }
    } else if app.store.pendingMarks == 0 {
      LabeledContent("Status", value: "Up to date")
      if let at = app.store.lastSyncAt {
        LabeledContent("Last synced") {
          Text(at, format: .relative(presentation: .named))
        }
      }
    } else if !app.online {
      LabeledContent("Status", value: "Offline — will sync when back online")
    } else if app.store.syncError != nil {
      LabeledContent("Status", value: "Sync failed — marks are kept")
    } else {
      LabeledContent("Status", value: "\(app.store.pendingMarks) marks queued")
    }
  }

  /// Downloaded entries carry title/chapter names; marks for chapters read
  /// online-only fall back to the raw id.
  private func label(for mark: LocalStore.QueuedMark) -> String {
    let volume = mark.volumeId != nil
    let id = mark.volumeId ?? mark.chapterId
    if let e = app.store.chapters[volume ? -id : id] {
      return "\(e.titleName) · \(volume ? "Vol" : "Ch") \(e.label)"
    }
    return "\(volume ? "Volume" : "Chapter") #\(id)"
  }
}

/// ConnectionBadge is the app-wide status dot in the navigation bar: green
/// online, amber when sync is stuck or queued, red offline. Tapping opens
/// the sync screen.
struct ConnectionBadge: View {
  @Environment(AppState.self) private var app
  @State private var showSync = false

  private var color: Color {
    if !app.online {
      return Theme.error
    }
    if app.store.syncError != nil || app.store.pendingMarks > 0 {
      return Theme.warning
    }
    return Theme.success
  }

  private var hint: String {
    var parts = [app.online ? "Online" : "Offline"]
    if let slot = app.activeSlot {
      parts.append(slot == .local ? "local address" : "remote address")
    }
    if app.store.pendingMarks > 0 {
      parts.append("\(app.store.pendingMarks) marks queued")
    }
    return parts.joined(separator: ", ")
  }

  var body: some View {
    Button {
      showSync = true
    } label: {
      Circle()
        .fill(color)
        .frame(width: 11, height: 11)
    }
    .accessibilityLabel("Sync status: \(hint)")
    .sheet(isPresented: $showSync) {
      NavigationStack { SyncStatusView() }
        .presentationDetents([.medium, .large])
    }
  }
}

extension View {
  /// connectionStatusToolbar puts the status dot in the navigation bar.
  /// Apply inside a NavigationStack, next to .navigationTitle.
  func connectionStatusToolbar() -> some View {
    toolbar {
      ToolbarItem(placement: .topBarTrailing) { ConnectionBadge() }
    }
  }
}

import SwiftUI
import UIKit

@MainActor func prewarmKeyboard() {
  guard let window = UIApplication.shared.connectedScenes
    .compactMap({ ($0 as? UIWindowScene)?.keyWindow }).first else { return }
  let field = UITextField()
  window.addSubview(field)
  field.becomeFirstResponder()
  field.resignFirstResponder()
  field.removeFromSuperview()
}

@main
struct KaodokuApp: App {
  @Environment(\.scenePhase) private var scenePhase
  @State private var app = AppState()
  @State private var storeReady = false

  var body: some Scene {
    WindowGroup {
      Group {
        if !storeReady {
          ProgressView("Loading downloads...")
            .task {
              await app.bootstrapStore()
              storeReady = true
            }
        } else if app.connected {
          @Bindable var app = app
          TabView(selection: $app.tab) {
            LibraryView().tabItem { Label("Library", systemImage: "books.vertical") }.tag(0)
            SearchView().tabItem { Label("Search", systemImage: "magnifyingglass") }.tag(1)
            DownloadsView().tabItem { Label("Downloads", systemImage: "arrow.down.circle") }.tag(2)
            SettingsView().tabItem { Label("Settings", systemImage: "gearshape") }.tag(3)
          }
          .task { await app.loadSession() }
          .task { prewarmKeyboard() }
        } else if !app.downloadedInstances().isEmpty {
          OfflineTabs()
        } else {
          ConnectView()
        }
      }
      .environment(app)
      .tint(Theme.primary)
      .preferredColorScheme(.dark)
      .onChange(of: scenePhase) { _, phase in
        if phase != .active {
          Task { await app.store.flush(app.online ? app.api : nil) }
        } else {
          app.scheduleReselect()
        }
      }
      .alert(app.store.requiresRecovery ? "Offline metadata is unreadable" : "Offline data wasn’t saved",
             isPresented: Binding(
               get: { app.store.persistenceError != nil },
               set: {
                 if !$0, !app.store.requiresRecovery {
                   app.store.clearPersistenceError()
                 }
               }
             )) {
        if app.store.requiresRecovery {
          Button("Retry") { Task { await app.bootstrapStore() } }
          Button("Reset Metadata", role: .destructive) {
            Task { await app.store.resetCorruptMetadata() }
          }
        } else {
          Button("Retry") { app.store.retryPersistence() }
          Button("Later", role: .cancel) {}
        }
      } message: {
        Text(app.store.persistenceError ?? "")
      }
    }
  }
}

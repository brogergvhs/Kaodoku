import SwiftUI

/// DownloadsGrid renders the active store's titles.
struct DownloadsGrid: View {
  @Environment(AppState.self) private var app

  var body: some View {
    let titles = app.store.titles
    ScrollView {
      if titles.isEmpty {
        Text("Nothing downloaded yet. Use a title's ⋯ menu to download chapters to this device.")
          .foregroundStyle(.secondary)
          .multilineTextAlignment(.center)
          .padding(.horizontal, 32)
          .padding(.top, 80)
      }
      LazyVGrid(columns: [GridItem(.adaptive(minimum: 110), spacing: 12)], spacing: 16) {
        ForEach(titles) { title in
          NavigationLink(value: title.id) {
            LocalTitleCard(title: title)
          }
          .buttonStyle(.plain)
        }
      }
      .padding(.horizontal)
    }
    .task { await app.store.prune() }
  }
}

/// DownloadsView mirrors the Library grid over local content: cover cards with
/// read progress of the downloaded chapters, sizes, then a title page identical
/// to the online one and the offline reader.
struct DownloadsView: View {
  var body: some View {
    NavigationStack {
      DownloadsGrid()
        .nordScreen()
        .navigationTitle("Downloads")
        .navigationBarTitleDisplayMode(.inline)
        .connectionStatusToolbar()
        .navigationDestination(for: Int64.self) { LocalTitleView(titleId: $0) }
    }
  }
}

/// OfflineTabs is the disconnected shell.
struct OfflineTabs: View {
  var body: some View {
    TabView {
      OfflineView().tabItem { Label("Downloads", systemImage: "arrow.down.circle") }
      ConnectView().tabItem { Label("Connect", systemImage: "link") }
    }
  }
}

struct OfflineServer: Hashable { let id: String }

struct OfflineView: View {
  @Environment(AppState.self) private var app

  var body: some View {
    let instances = app.downloadedInstances()
    NavigationStack {
      Group {
        if instances.count == 1 {
          DownloadsGrid()
            .task { await app.store.activate(instances[0]) }
            .navigationTitle(app.serverName(instances[0]))
        } else {
          List {
            Section("Downloaded servers") {
              ForEach(instances, id: \.self) { id in
                NavigationLink(value: OfflineServer(id: id)) {
                  Text(app.serverName(id))
                }
              }
            }
            .nordRows()
          }
          .navigationTitle("Offline")
        }
      }
      .nordScreen()
      .navigationBarTitleDisplayMode(.inline)
      .connectionStatusToolbar()
      .navigationDestination(for: Int64.self) { LocalTitleView(titleId: $0) }
      .navigationDestination(for: OfflineServer.self) { server in
        DownloadsGrid()
          .task { await app.store.activate(server.id) }
          .nordScreen()
          .navigationTitle(app.serverName(server.id))
          .navigationBarTitleDisplayMode(.inline)
      }
    }
  }
}

/// PendingRow is a queued/active device download and is never tappable: greyed
/// with an empty bar while queued, a filling bar + percentage while downloading.
struct PendingRow: View {
  let item: LocalStore.Pending
  let volumes: Bool
  let active: Bool
  let progress: Double

  private var name: String {
    volumes && Double(item.label) == nil ? item.label : "\(volumes ? "Volume" : "Chapter") \(item.label)"
  }

  var body: some View {
    HStack(spacing: 10) {
      if volumes {
        RoundedRectangle(cornerRadius: 6)
          .fill(.quaternary)
          .frame(width: 40, height: 56)
      }
      VStack(alignment: .leading, spacing: 5) {
        Text(name)
          .foregroundStyle(active ? .primary : .tertiary)
          .lineLimit(1)
        HStack(spacing: 8) {
          BarView(read: 0, full: active ? progress : 0)
          Text("\(Int((active ? progress : 0) * 100))%")
            .font(.caption2)
            .foregroundStyle(.secondary)
            .frame(width: 34, alignment: .trailing)
        }
      }
    }
  }
}

/// LocalTitleCard matches TitleCard's web-card layout. The bar covers only the
/// downloaded chapters: green = read share of what's on the device.
struct LocalTitleCard: View {
  let title: LocalStore.LocalTitle

  var body: some View {
    VStack(alignment: .leading, spacing: 6) {
      Cover(path: "", adult: title.info.isAdult, local: title.coverURL)
      Text(title.info.name)
        .font(.caption)
        .lineLimit(2, reservesSpace: true)
        .foregroundStyle(.primary)
      HStack(alignment: .firstTextBaseline) {
        Text(cardLine).lineLimit(1)
        Spacer(minLength: 8)
        Text(humanBytes(title.size))
      }
      .font(.caption2)
      .foregroundStyle(.secondary)
      BarView(read: barShare, full: 1)
    }
  }
}

extension LocalStore.LocalTitle {
  var readCount: Int {
    entries.count { $0.isRead }
  }
}

extension LocalTitleCard {
  var cardLine: String {
    if title.entries.isEmpty, !title.pending.isEmpty {
      return "downloading \(title.pending.count)…"
    }
    let ch = title.chapterEntries, vols = title.volumeEntries
    if !vols.isEmpty, !ch.isEmpty {
      return "\(ch.count) ch · \(vols.count) vols"
    }
    if !vols.isEmpty {
      return "\(vols.count { $0.isRead })/\(vols.count) vols read"
    }
    return "\(title.readCount)/\(title.entries.count) read"
  }

  var barShare: Double {
    let ch = title.chapterEntries
    let list = ch.isEmpty ? title.volumeEntries : ch
    return pct(Int64(list.count { $0.isRead }), Int64(list.count))
  }
}

// LocalTitleView is the offline title page, identical in layout to
// TitleDetailView: header, content header with the Read button, chapter rows.
struct LocalTitleView: View {
  @Environment(AppState.self) private var app
  @Environment(\.dismiss) private var dismiss
  let titleId: Int64
  @State private var reading: LocalStore.Entry?
  @State private var showRemoveRange = false
  @State private var volumesTab = false

  private var title: LocalStore.LocalTitle? {
    app.store.titles.first { $0.id == titleId }
  }

  private func rows(_ title: LocalStore.LocalTitle) -> [LocalStore.Entry] {
    volumesTab ? title.volumeEntries : title.chapterEntries
  }

  var body: some View {
    let title = title
    List {
      if let title {
        Section { header(title) }
          .nordRows()
        Section { contentHeader(title) }
          .listRowBackground(Color.clear)
          .listRowInsets(EdgeInsets())
        Section(volumesTab ? "Volumes" : "Chapters") {
          ForEach(rows(title)) { entry in
            Button {
              reading = entry
            } label: {
              ChapterRow(chapter: chapterProgress(entry), volumes: volumesTab, local: true,
                         localThumb: app.store.thumbURL(entry))
            }
          }
          .onDelete { offsets in
            let list = rows(title)
            for i in offsets {
              app.store.delete(list[i].id, volume: volumesTab)
            }
          }
          ForEach(volumesTab ? title.pendingVolumes : title.pendingChapters) { p in
            PendingRow(item: p, volumes: volumesTab,
                       active: app.store.isActive(p),
                       progress: app.store.isActive(p) ? app.store.downloadProgress : 0)
          }
        }
        .nordRows()
      }
    }
    .nordScreen()
    .onAppear {
      if let t = title, t.chapterEntries.isEmpty, t.pendingChapters.isEmpty,
         !(t.volumeEntries.isEmpty && t.pendingVolumes.isEmpty)
      {
        volumesTab = true
      }
    }
    .onChange(of: title == nil) { _, gone in
      if gone {
        dismiss()
      }
    }
    .navigationTitle(title?.info.name ?? "")
    .navigationBarTitleDisplayMode(.inline)
    .toolbar {
      Menu {
        Menu {
          Button("All chapters", role: .destructive) {
            app.store.deleteTitle(titleId)
            dismiss()
          }
          Button("Range…") { showRemoveRange = true }
        } label: {
          Label("Remove downloads", systemImage: "trash")
        }
      } label: {
        Image(systemName: "ellipsis.circle")
      }
    }
    .sheet(isPresented: $showRemoveRange) {
      RangeSheet(title: "Remove range", action: "Remove from device") { from, to in
        for e in title?.chapterEntries ?? [] {
          if let n = Double(e.label), Int(n) >= from, Int(n) <= to {
            app.store.delete(e.id)
          }
        }
      }
    }
    .fullScreenCover(item: $reading) { entry in
      ReaderView(titleID: titleId, startChapter: entry.id, volumes: volumesTab,
                 localChapters: title.map(rows) ?? [entry])
    }
  }

  private func header(_ title: LocalStore.LocalTitle) -> some View {
    VStack(alignment: .leading, spacing: 12) {
      HStack {
        Spacer()
        Cover(path: "", adult: title.info.isAdult, local: title.coverURL, targetWidth: 170)
          .frame(width: 170)
        Spacer()
      }
      Text(title.info.name).font(.title2.bold())
      VStack(alignment: .leading, spacing: 4) {
        let ch = title.chapterEntries, vols = title.volumeEntries
        if ch.isEmpty, !vols.isEmpty {
          BarView(read: pct(Int64(vols.count { $0.isRead }), Int64(vols.count)), full: 1)
        } else {
          BarView(read: pct(Int64(ch.count { $0.isRead }), Int64(ch.count)), full: 1)
        }
        Text(headerLine(title))
          .font(.caption).foregroundStyle(.secondary)
      }
      if let detail = title.info.detail {
        MangaDetailBlock(manga: detail)
      }
    }
  }

  private func contentHeader(_ title: LocalStore.LocalTitle) -> some View {
    VStack(spacing: 10) {
      let hasCh = !title.chapterEntries.isEmpty || !title.pendingChapters.isEmpty
      let hasVols = !title.volumeEntries.isEmpty || !title.pendingVolumes.isEmpty
      if hasCh, hasVols {
        Picker("Content", selection: $volumesTab) {
          Text("\(title.chapterEntries.count) Chapters").tag(false)
          Text("\(title.volumeEntries.count) Volumes").tag(true)
        }
        .pickerStyle(.segmented)
      }
      let list = rows(title)
      if let next = list.first(where: { !$0.isRead }) ?? list.last {
        Button {
          reading = next
        } label: {
          Text(list.contains { $0.readPages > 0 || $0.isRead } ? "Continue reading" : "Read")
            .font(.headline)
            .frame(maxWidth: .infinity)
            .padding(.vertical, 8)
        }
        .buttonStyle(.borderedProminent)
      }
    }
  }

  private func headerLine(_ title: LocalStore.LocalTitle) -> String {
    var parts: [String] = []
    let ch = title.chapterEntries, vols = title.volumeEntries
    if !ch.isEmpty {
      parts.append("\(ch.count { $0.isRead })/\(ch.count) read")
    }
    if !vols.isEmpty {
      parts.append("\(vols.count { $0.isRead })/\(vols.count) vols")
    }
    parts.append("\(humanBytes(title.size)) on device")
    return parts.joined(separator: " · ")
  }

  private func chapterProgress(_ e: LocalStore.Entry) -> ChapterProgress {
    ChapterProgress(id: e.id, titleId: e.titleId, label: e.label,
                    title: e.isVolume ? (e.name ?? e.label) : "",
                    displayName: e.isVolume ? nil : e.name, numberMain: 0,
                    downloaded: true, bytes: e.size, pages: e.pages, totalPages: e.pages,
                    readPages: e.readPages, completed: e.isRead, manual: false,
                    firstUnreadPage: 0, lastReadAt: nil)
  }
}

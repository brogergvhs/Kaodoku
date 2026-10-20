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

/// LocalTitleView is the offline title page, identical in layout to TitleDetailView.
struct LocalTitleView: View {
  @Environment(AppState.self) private var app
  @Environment(\.dismiss) private var dismiss
  let titleId: Int64
  @State private var reading: LocalStore.Entry?
  @State private var showRemoveRange = false
  @State private var volumesTab = false
  @State private var order = ChapterOrder.number
  @State private var ascending = false
  @State private var page = 0
  @State private var bannerRest: CGFloat?
  @State private var bannerShift: CGFloat = 0
  private let perPage = 25

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
        Section {
          TitleTop(cover: Cover(path: "", adult: title.info.isAdult, local: title.coverURL, targetWidth: 140),
                   name: title.info.name) { readButton(title) }
            .padding(.top, title.bannerURL == nil ? 0 : 30)
            .trackBanner(rest: $bannerRest, shift: $bannerShift)
        }
        .listRowBackground(Color.clear)
        .listRowInsets(EdgeInsets())
        Section { header(title) }
          .nordRows()
        if hasChapters(title), hasVolumes(title) {
          Section {
            Picker("Content", selection: $volumesTab) {
              Text("\(title.chapterEntries.count) Chapters").tag(false)
              Text("\(title.volumeEntries.count) Volumes").tag(true)
            }
            .pickerStyle(.segmented)
          }
          .listRowBackground(Color.clear)
          .listRowInsets(EdgeInsets())
        }
        let byID = Dictionary(uniqueKeysWithValues: rows(title).map { ($0.id, $0) })
        let sorted = order.sorted(rows(title).map(chapterProgress), ascending: ascending)
        let pages = max(1, (sorted.count + perPage - 1) / perPage)
        let current = min(page, pages - 1)
        let shown = Array(sorted.dropFirst(current * perPage).prefix(perPage))
        Section {
          ForEach(shown) { ch in
            Button {
              reading = byID[ch.id]
            } label: {
              ChapterRow(chapter: ch, volumes: volumesTab, local: true,
                         localThumb: byID[ch.id].flatMap(app.store.thumbURL))
            }
          }
          .onDelete { offsets in
            for i in offsets {
              app.store.delete(shown[i].id, volume: volumesTab)
            }
          }
          ForEach(volumesTab ? title.pendingVolumes : title.pendingChapters) { p in
            PendingRow(item: p, volumes: volumesTab,
                       active: app.store.isActive(p),
                       progress: app.store.isActive(p) ? app.store.downloadProgress : 0)
          }
          PagerRow(page: $page, current: current, pages: pages)
        } header: {
          ChapterListHeader(label: volumesTab ? "Volumes" : "Chapters", order: $order, ascending: $ascending)
        }
        .nordRows()
      }
    }
    .listSectionSpacing(.compact)
    .background(alignment: .top) {
      if let banner = title?.bannerURL {
        TitleBanner(shift: bannerShift, local: banner)
      }
    }
    .nordScreen()
    .onChange(of: volumesTab) { page = 0 }
    .onChange(of: order) { page = 0 }
    .onChange(of: ascending) { page = 0 }
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
      VStack(alignment: .leading, spacing: 4) {
        let ch = title.chapterEntries, vols = title.volumeEntries
        if ch.isEmpty, !vols.isEmpty {
          BarView(read: pct(Int64(vols.count { $0.isRead }), Int64(vols.count)), full: 1)
        } else {
          BarView(read: pct(Int64(ch.count { $0.isRead }), Int64(ch.count)), full: 1)
        }
        StatsLine(left: headerLine(title), right: "\(humanBytes(title.size)) on device")
      }
      if let detail = title.info.detail {
        MangaDetailBlock(manga: detail)
      }
    }
  }

  private func hasChapters(_ title: LocalStore.LocalTitle) -> Bool {
    !title.chapterEntries.isEmpty || !title.pendingChapters.isEmpty
  }

  private func hasVolumes(_ title: LocalStore.LocalTitle) -> Bool {
    !title.volumeEntries.isEmpty || !title.pendingVolumes.isEmpty
  }

  @ViewBuilder private func readButton(_ title: LocalStore.LocalTitle) -> some View {
    let list = rows(title)
    if let next = list.first(where: { !$0.isRead }) ?? list.last {
      ReadButton(label: list.contains { $0.readPages > 0 || $0.isRead } ? "Continue reading" : "Read") {
        reading = next
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

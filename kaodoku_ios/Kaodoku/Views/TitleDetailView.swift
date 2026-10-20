import SwiftUI

struct TitleDetailView: View {
  @Environment(AppState.self) private var app
  let titleID: Int64
  @State private var progress: TitleReadProgress?
  @State private var volumes = false
  @State private var readerChapter: ChapterProgress?
  @State private var note: String?
  @State private var downloading = false
  @State private var anilistConnected = false
  @State private var checkedAniList = false
  @State private var showRange = false
  @State private var showRemoveRange = false
  @State private var showSources = false
  @State private var showSettings = false
  @State private var showCollections = false
  @State private var showRemove = false
  @State private var autoTabbed = false
  @State private var activity: TitleActivity?
  @State private var pollGen = 0
  @State private var order = ChapterOrder.number
  @State private var ascending = false
  @State private var page = 0
  private let perPage = 25

  private var canManage: Bool {
    app.me?.can("library.manage") == true
  }

  var body: some View {
    List {
      if let p = progress {
        Section {
          VStack(spacing: 12) {
            bigCover(p)
            readButton(p)
          }
          .padding(.top, p.title.bannerImage == nil ? 0 : 70)
        }
        .listRowBackground(Color.clear)
        .listRowInsets(EdgeInsets())
        Section { header(p) }
          .nordRows()
        if activity?.busy == true || activity?.failed == true || (!p.title.linked && canManage) || p.title.volumeCount > 0 {
          Section { contentHeader(p) }
            .listRowBackground(Color.clear)
            .listRowInsets(EdgeInsets())
        }
        let sorted = ordered(p.chapters)
        let pages = max(1, (sorted.count + perPage - 1) / perPage)
        let current = min(page, pages - 1)
        Section {
          if p.chapters.isEmpty {
            Text(volumes ? "No volumes yet." : "No chapters yet.")
              .font(.caption).foregroundStyle(.secondary)
          }
          ForEach(sorted.dropFirst(current * perPage).prefix(perPage)) { ch in
            let local = app.store.isDownloaded(ch.id, volume: volumes)
            Button {
              readerChapter = ch
            } label: {
              ChapterRow(chapter: ch, volumes: volumes, local: local, thumb: volumes)
            }
            .disabled(!volumes && !ch.downloaded && !local)
            .swipeActions {
              if local {
                Button("Remove from device", systemImage: "iphone.slash", role: .destructive) {
                  app.store.delete(ch.id, volume: volumes)
                }
              }
            }
          }
          if pages > 1 {
            HStack {
              Button { page = current - 1 } label: { Image(systemName: "chevron.left") }
                .disabled(current == 0)
              Spacer()
              Text("\(current + 1) / \(pages)").font(.caption).foregroundStyle(.secondary)
              Spacer()
              Button { page = current + 1 } label: { Image(systemName: "chevron.right") }
                .disabled(current == pages - 1)
            }
            .buttonStyle(.borderless)
          }
        } header: {
          HStack {
            Text(volumes ? "Volumes" : "Chapters")
            Spacer()
            Picker("Sort by", selection: $order) {
              ForEach(ChapterOrder.allCases, id: \.self) { Text($0.rawValue) }
            }
            .pickerStyle(.menu)
            Button {
              ascending.toggle()
            } label: {
              Image(systemName: ascending ? "arrow.up" : "arrow.down")
            }
            .accessibilityLabel(ascending ? "Ascending" : "Descending")
          }
          .textCase(nil)
        }
        .nordRows()
      } else {
        ProgressView()
      }
    }
    .listSectionSpacing(.compact)
    .background(alignment: .top) {
      if let banner = progress?.title.bannerImage {
        Color.clear
          .frame(height: 320)
          .overlay { ServerImage(path: banner, maxPixelSize: CGSize(width: 1600, height: 700)) }
          .overlay(LinearGradient(colors: [.clear, Theme.bg], startPoint: .center, endPoint: .bottom))
          .clipped()
          .ignoresSafeArea(edges: .top)
      }
    }
    .nordScreen()
    .navigationTitle(progress?.title.displayTitle ?? "")
    .navigationBarTitleDisplayMode(.inline)
    .onChange(of: volumes) { pollGen += 1; page = 0 }
    .onChange(of: order) { page = 0 }
    .onChange(of: ascending) { page = 0 }
    .task(id: pollGen) {
      await load()
      while activity?.busy == true {
        do { try await Task.sleep(for: .seconds(2)) } catch { return }
        guard let api = app.api,
              let latest: TitleActivity = try? await api.get(
                "/api/v1/library/\(titleID)/activity"
              ) else { return }
        activity = latest
        if !latest.busy {
          await load()
          return
        }
      }
    }
    .fullScreenCover(item: $readerChapter, onDismiss: { Task { await load() } }) { ch in
      ReaderView(titleID: titleID, startChapter: ch.id, volumes: volumes)
    }
    .toolbar {
      if let p = progress {
        Button {
          toggleFavourite(p)
        } label: {
          Image(systemName: p.title.favourite ? "heart.fill" : "heart")
        }
        actionsMenu(p)
      }
    }
    .sheet(isPresented: $showRange) {
      RangeSheet(title: "Download range", action: "Download to device") { from, to in
        if let p = progress {
          downloadToDevice(p) { $0.numberMain >= from && $0.numberMain <= to }
        }
      }
    }
    .sheet(isPresented: $showSources) {
      TitleSourcesSheet(titleID: titleID) {
        note = "Source linked"
        pollGen += 1
      }
    }
    .sheet(isPresented: $showRemoveRange) {
      RangeSheet(title: "Remove range", action: "Remove from device") { from, to in
        guard let p = progress else { return }
        let todo = p.chapters.filter {
          app.store.isDownloaded($0.id) && $0.numberMain >= from && $0.numberMain <= to
        }
        for ch in todo {
          app.store.delete(ch.id)
        }

        note = "Removed \(todo.count) chapters from device"
      }
    }
    .sheet(isPresented: $showSettings) {
      if let p = progress {
        TitleSettingsSheet(interval: p.title.refreshInterval) { value in
          patch(["refresh_interval": value], done: "Settings saved")
        }
      }
    }
    .sheet(isPresented: $showCollections) {
      CollectionsSheet(titleID: titleID) { note = $0 }
    }
    .sheet(isPresented: $showRemove) {
      if let p = progress {
        RemoveTitleSheet(name: p.title.displayTitle, anilistConnected: anilistConnected) { files, anilist in
          removeTitle(deleteFiles: files, deleteAniList: anilist)
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
          .task { try? await Task.sleep(for: .seconds(2)); self.note = nil }
      }
    }
  }

  private func ordered(_ chs: [ChapterProgress]) -> [ChapterProgress] {
    let asc = chs.enumerated().sorted { a, b in
      order.less(a.element, b.element) ?? (a.offset < b.offset)
    }.map(\.element)
    return ascending ? asc : asc.reversed()
  }

  private func bigCover(_ p: TitleReadProgress) -> some View {
    HStack {
      Spacer()
      Cover(path: p.title.coverImage, adult: p.title.isAdult, targetWidth: 170).frame(width: 170)
      Spacer()
    }
  }

  private func header(_ p: TitleReadProgress) -> some View {
    VStack(alignment: .leading, spacing: 12) {
      Text(p.title.displayTitle).font(.title2.bold())
      if !p.title.monitored {
        Label("Not monitored", systemImage: "bell.slash").font(.caption).foregroundStyle(.secondary)
      }
      TitleProgressBlock(title: p.title)
      if let manga = p.manga {
        MangaDetailBlock(manga: manga)
      }
    }
  }

  @ViewBuilder private var activityBanner: some View {
    if let a = activity, a.busy {
      HStack(spacing: 8) {
        if !a.active.isEmpty {
          ProgressView().controlSize(.small)
          Text("\(a.active)…").font(.subheadline)
        }
        if !a.queued.isEmpty {
          Image(systemName: "clock").font(.caption).foregroundStyle(.secondary)
          Text("queued: \(a.queued.joined(separator: " · "))")
            .font(.subheadline).foregroundStyle(.secondary)
        }
        Spacer()
      }
      .padding(10)
      .background(Theme.surface, in: RoundedRectangle(cornerRadius: 10))
    } else if let a = activity, a.failed {
      Text("failed\(a.error.map { " — \($0)" } ?? "")")
        .font(.subheadline)
        .foregroundStyle(Theme.error)
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(10)
        .background(Theme.error.opacity(0.12), in: RoundedRectangle(cornerRadius: 10))
    }
  }

  private func contentHeader(_ p: TitleReadProgress) -> some View {
    VStack(spacing: 10) {
      activityBanner
      if !p.title.linked, canManage {
        Button {
          showSources = true
        } label: {
          Label("Link a source to fetch chapters", systemImage: "link")
            .font(.subheadline)
            .frame(maxWidth: .infinity)
            .padding(.vertical, 8)
        }
        .buttonStyle(.bordered)
        .tint(Theme.warning)
      }
      if p.title.volumeCount > 0 {
        Picker("Content", selection: $volumes) {
          Text("\(p.title.discoveredCount) Chapters").tag(false)
          Text("\(p.title.volumeCount) Volumes").tag(true)
        }
        .pickerStyle(.segmented)
      }
    }
  }

  @ViewBuilder private func readButton(_ p: TitleReadProgress) -> some View {
    if volumes ? p.title.volumeCount > 0 : p.title.completedCount > 0,
       let next = p.chapters.first(where: { $0.id == p.nextChapterId })
       ?? p.chapters.first(where: { volumes || $0.downloaded })
    {
      Button {
        readerChapter = next
      } label: {
        Text(readLabel(p))
          .font(.headline)
          .frame(maxWidth: .infinity)
          .padding(.vertical, 8)
      }
      .buttonStyle(.borderedProminent)
    }
  }

  private func readLabel(_ p: TitleReadProgress) -> String {
    let started = volumes
      ? p.chapters.contains { $0.readPages > 0 || $0.completed }
      : p.readPages > 0
    return started ? "Continue reading" : "Read"
  }

  private func actionsMenu(_ p: TitleReadProgress) -> some View {
    Menu {
      Menu {
        Button(volumes ? "All volumes" : "All chapters") { downloadToDevice(p) { _ in true } }
        Button("Unread only") { downloadToDevice(p) { !$0.completed } }
        if !volumes {
          Button("Range…") { showRange = true }
        }
      } label: {
        Label("Download to device", systemImage: "arrow.down.to.line")
      }
      .disabled(downloading || !p.chapters.contains { (volumes || $0.downloaded) && !app.store.isDownloaded($0.id, volume: volumes) })
      if app.store.entries(titleId: titleID).contains(where: { $0.isVolume == volumes }) {
        Menu {
          Button(volumes ? "All volumes" : "All chapters", role: .destructive) {
            for e in app.store.entries(titleId: titleID) where e.isVolume == volumes {
              app.store.delete(e.id, volume: volumes)
            }
            note = "Device downloads removed"
          }
          if !volumes {
            Button("Range…") { showRemoveRange = true }
          }
        } label: {
          Label("Remove device downloads", systemImage: "trash")
        }
        .disabled(downloading)
      }
      if canManage {
        Divider()
        if p.title.linked, p.title.missingCount > 0 || p.title.failedCount > 0 {
          Button("Download missing" + (p.title.failedCount > 0 ? " · \(p.title.failedCount) failed" : ""),
                 systemImage: "arrow.down.circle")
          {
            enqueue("download_missing", done: "Download queued")
          }
        }
        if p.title.linked {
          Button("Refresh chapters", systemImage: "arrow.clockwise") {
            enqueue("refresh_title", done: "Refresh queued")
          }
        }
        Button("Scan files", systemImage: "internaldrive") {
          enqueue("scan_downloads", done: "Scan queued")
        }
        Button("Sources…", systemImage: "link") { showSources = true }
        if anilistConnected {
          Button("Sync AniList", systemImage: "arrow.triangle.2.circlepath") {
            post("/api/v1/anilist/sync", body: ["title_id": titleID], done: "AniList synced")
          }
        }
        Button(p.title.monitored ? "Stop monitoring" : "Monitor",
               systemImage: p.title.monitored ? "bell.slash" : "bell")
        {
          patch(["monitored": JSONValue.bool(!p.title.monitored)],
                done: p.title.monitored ? "Monitoring off" : "Monitoring on")
        }
        if p.title.linked {
          Button("Title settings…", systemImage: "gearshape") { showSettings = true }
        }
        Button("Add to collection…", systemImage: "square.stack.3d.up") { showCollections = true }
        Button("Remove title…", systemImage: "trash", role: .destructive) { showRemove = true }
      }
    } label: {
      Image(systemName: "ellipsis.circle")
    }
  }

  /// downloadToDevice pulls every server-downloaded chapter matching the
  /// scope that isn't on the device yet, then snapshots the title card.
  private func downloadToDevice(_ p: TitleReadProgress, scope: @escaping (ChapterProgress) -> Bool) {
    guard let api = app.api, !downloading else { return }
    let volumes = volumes
    let todo = p.chapters.filter {
      (volumes || $0.downloaded) && !app.store.isDownloaded($0.id, volume: volumes) && scope($0)
    }
    guard !todo.isEmpty else { note = "Nothing to download"; return }
    downloading = true
    Task {
      defer {
        downloading = false
        app.store.clearPending(titleId: titleID)
      }
      let cover = try? await api.data("GET", p.title.coverImage)
      app.store.saveTitle(LocalStore.TitleInfo(
        id: titleID, name: p.title.displayTitle, isAdult: p.title.isAdult, detail: p.manga,
        readCount: p.title.readCount, completedCount: p.title.completedCount,
        discoveredCount: p.title.discoveredCount, missingCount: p.title.missingCount
      ), coverData: cover)
      app.store.beginPending(todo.map {
        LocalStore.Pending(id: $0.id, titleId: titleID,
                           label: volumes && !$0.title.isEmpty ? $0.title : $0.label,
                           pages: $0.totalPages, volume: volumes)
      })
      for (i, ch) in todo.enumerated() {
        let kind = volumes ? "volumes" : "chapters"
        let name = volumes && !ch.title.isEmpty ? ch.title : ch.label
        guard app.store.canStore(bytes: ch.bytes) else {
          note = "Not enough device storage for \(name)"
          return
        }
        note = "Downloading \(i + 1)/\(todo.count)…"
        app.store.markActive(ch.id, volume: volumes)
        let store = app.store
        do {
          let tmp = try await api.download("/api/v1/reader/\(kind)/\(ch.id)/archive",
                                           expectedBytes: ch.bytes)
          { fraction in
            Task { @MainActor in store.setProgress(fraction) }
          }
          let display = ch.displayName ?? ch.title
          try await app.store.save(file: tmp, chapterID: ch.id, titleId: titleID,
                                   titleName: p.title.displayTitle, label: name,
                                   name: display.isEmpty ? nil : display,
                                   readPages: ch.readPages, completed: ch.completed, volume: volumes)
        } catch {
          note = error.localizedDescription
          return
        }
      }
      note = "Downloaded \(todo.count) \(volumes ? "volumes" : "chapters")"
    }
  }

  private func load() async {
    guard let api = app.api else { return }
    await app.store.flush(app.online ? api : nil)
    let mode = volumes ? "?mode=volumes" : ""
    progress = try? await api.get("/api/v1/reader/titles/\(titleID)\(mode)")
    activity = try? await api.get("/api/v1/library/\(titleID)/activity")
    if let p = progress, !autoTabbed {
      autoTabbed = true
      if p.title.discoveredCount == 0, p.title.volumeCount > 0 {
        volumes = true
        return
      }
    }
    if let p = progress {
      app.store.syncRead(p.chapters, volumes: volumes)
    }
    if canManage, !checkedAniList {
      checkedAniList = true
      if let status: AniListStatus = try? await api.get("/api/v1/anilist") {
        anilistConnected = status.connected
      }
    }
  }

  private func toggleFavourite(_ p: TitleReadProgress) {
    guard let api = app.api else { return }
    let method = p.title.favourite ? "DELETE" : "PUT"
    Task {
      _ = try? await api.data(method, "/api/v1/library/\(titleID)/favourite")
      await load()
    }
  }

  private func removeTitle(deleteFiles: Bool, deleteAniList: Bool) {
    guard let api = app.api else { return }
    Task {
      do {
        _ = try await api.data("DELETE",
                               "/api/v1/library/\(titleID)?delete_files=\(deleteFiles ? 1 : 0)&delete_anilist=\(deleteAniList ? 1 : 0)")
        app.tab = 0
        app.libraryNav = .root
      } catch { note = error.localizedDescription }
    }
  }

  private func patch(_ body: [String: some Encodable], done: String) {
    guard let api = app.api else { return }
    Task {
      do {
        _ = try await api.data("PATCH", "/api/v1/library/\(titleID)", body: body)
        note = done
        await load()
      } catch { note = error.localizedDescription }
    }
  }

  private func enqueue(_ type: String, done: String) {
    let body: [String: JSONValue] = ["type": .string(type), "title_id": .int(titleID)]
    post("/api/v1/jobs/enqueue", body: body, done: done)
  }

  private func post(_ path: String, body: some Encodable, done: String) {
    guard let api = app.api else { return }
    Task {
      do {
        _ = try await api.data("POST", path, body: body)
        note = done
        pollGen += 1
      } catch { note = error.localizedDescription }
    }
  }
}

/// RangeSheet mirrors the web bulk from/to dropdowns (download, delete, …).
struct RangeSheet: View {
  @Environment(\.dismiss) private var dismiss
  let title: String
  let action: String
  var onSubmit: (Int, Int) -> Void
  @State private var from = ""
  @State private var to = ""

  var body: some View {
    NavigationStack {
      Form {
        Section {
          TextField("From chapter", text: $from).keyboardType(.numberPad)
          TextField("To chapter", text: $to).keyboardType(.numberPad)
        } footer: {
          Text("Whole chapter numbers, like the web's bulk actions.")
        }
        .nordRows()
        Button(action) {
          if let f = Int(from), let t = Int(to), f <= t {
            onSubmit(f, t)
            dismiss()
          }
        }
        .disabled(Int(from) == nil || Int(to) == nil)
        .nordRows()
      }
      .nordScreen()
      .navigationTitle(title)
      .navigationBarTitleDisplayMode(.inline)
      .toolbar { Button("Cancel") { dismiss() } }
    }
    .presentationDetents([.height(300)])
  }
}

/// TitleSettingsSheet mirrors the web title-settings modal (refresh interval).
struct TitleSettingsSheet: View {
  @Environment(\.dismiss) private var dismiss
  @State var interval: String
  var onSave: (String) -> Void

  var body: some View {
    NavigationStack {
      Form {
        Section("Check for new chapters every") {
          TextField("6h, 30m — blank for the global default", text: $interval)
            .autocorrectionDisabled()
            .textInputAutocapitalization(.never)
        }
        .nordRows()
        Button("Save") { onSave(interval); dismiss() }
          .nordRows()
      }
      .nordScreen()
      .navigationTitle("Title settings")
      .navigationBarTitleDisplayMode(.inline)
      .toolbar { Button("Cancel") { dismiss() } }
    }
    .presentationDetents([.height(260)])
  }
}

struct CollectionsSheet: View {
  @Environment(AppState.self) private var app
  @Environment(\.dismiss) private var dismiss
  let titleID: Int64
  var onDone: (String) -> Void
  @State private var groups: CollectionGroups?

  private var customs: [CollectionEntry] {
    (groups?.custom ?? []).filter { !$0.titleIds.contains(titleID) }
  }

  private var smarts: [CollectionEntry] {
    (groups?.smart ?? []).filter { !$0.titleIds.contains(titleID) }
  }

  var body: some View {
    NavigationStack {
      List {
        if groups != nil {
          if customs.isEmpty, smarts.isEmpty {
            Text("No collections to add to.").foregroundStyle(.secondary).nordRows()
          }
          if !customs.isEmpty {
            Section("Collections") {
              ForEach(customs, id: \.uid) { col in
                Button(col.name) { add(col) }
              }
            }
            .nordRows()
          }
          if !smarts.isEmpty {
            Section("Pin to series") {
              ForEach(smarts, id: \.uid) { col in
                Button(col.name) { pin(col) }
              }
            }
            .nordRows()
          }
        } else {
          ProgressView()
        }
      }
      .nordScreen()
      .navigationTitle("Add to collection")
      .navigationBarTitleDisplayMode(.inline)
      .toolbar { Button("Close") { dismiss() } }
      .task {
        guard let api = app.api else { return }
        do {
          groups = try await api.get("/api/v1/collections")
        } catch {
          onDone(error.localizedDescription)
          dismiss()
        }
      }
    }
  }

  private func add(_ col: CollectionEntry) {
    guard let id = col.id else { return }
    send("/api/v1/collections/\(id)/titles/\(titleID)", note: "Added to \(col.name)")
  }

  private func pin(_ col: CollectionEntry) {
    guard let key = col.key?.addingPercentEncoding(withAllowedCharacters: .urlPathAllowed) else { return }
    send("/api/v1/collections/smart/\(key)/pins/\(titleID)", note: "Pinned to \(col.name)")
  }

  private func send(_ path: String, note: String) {
    guard let api = app.api else { return }
    Task {
      do {
        _ = try await api.data("PUT", path)
        onDone(note)
      } catch { onDone(error.localizedDescription) }
      dismiss()
    }
  }
}

/// RemoveTitleSheet mirrors the web remove-title modal with both checkboxes.
struct RemoveTitleSheet: View {
  @Environment(\.dismiss) private var dismiss
  let name: String
  let anilistConnected: Bool
  var onRemove: (Bool, Bool) -> Void
  @State private var deleteFiles = false
  @State private var deleteAniList = false

  var body: some View {
    NavigationStack {
      Form {
        Section {
          Toggle(isOn: $deleteFiles) {
            VStack(alignment: .leading, spacing: 2) {
              Text("Also delete downloaded files from disk")
              Text("Leave off to keep the folder — it shows up on the Import page for re-importing later.")
                .font(.caption).foregroundStyle(.secondary)
            }
          }
          if anilistConnected {
            Toggle(isOn: $deleteAniList) {
              VStack(alignment: .leading, spacing: 2) {
                Text("Also delete from AniList")
                Text("Removes this manga from your AniList list entirely. Left off, it is set to Dropped instead.")
                  .font(.caption).foregroundStyle(.secondary)
              }
            }
          }
        } header: {
          Text("Tracking, chapters and reading progress are removed from the library.")
        }
        .nordRows()
        Button("Remove title", role: .destructive) {
          onRemove(deleteFiles, deleteAniList)
          dismiss()
        }
        .nordRows()
      }
      .nordScreen()
      .navigationTitle("Remove \(name)?")
      .navigationBarTitleDisplayMode(.inline)
      .toolbar { Button("Cancel") { dismiss() } }
    }
    .presentationDetents([.medium])
  }
}

/// TitleProgressBlock is the web progressBar partial: two-color bar plus the
/// "X/Y read · N missing · size · vols" line (volumes fallback included).
struct TitleProgressBlock: View {
  let title: Title

  var body: some View {
    VStack(alignment: .leading, spacing: 4) {
      if title.discoveredCount > 0 {
        BarView(read: pct(title.readCount, title.discoveredCount),
                full: pct(title.completedCount, title.discoveredCount))
        Text(line).font(.caption).foregroundStyle(.secondary)
      } else if title.volumeCount > 0 {
        BarView(read: pct(title.volumeReadCount, title.volumeCount), full: 1)
        Text("\(title.volumeReadCount)/\(title.volumeCount) volumes read · \(humanBytes(title.volumeBytes))")
          .font(.caption).foregroundStyle(.secondary)
      }
    }
  }

  private var line: String {
    var parts = ["\(title.readCount)/\(title.discoveredCount) read"]
    if title.missingCount > 0 {
      parts.append("\(title.missingCount) missing")
    }
    if title.failedCount > 0 {
      parts.append("\(title.failedCount) failed")
    }
    parts.append(humanBytes(title.sizeBytes))
    if title.volumeCount > 0 {
      parts.append("\(title.volumeReadCount)/\(title.volumeCount) vols")
    }
    return parts.joined(separator: " · ")
  }
}

/// MangaDetailBlock is the web mangaDetail cell: badge row, description,
/// authors/AniList-counts line, genre chips.
struct MangaDetailBlock: View {
  let manga: MangaDetail

  var body: some View {
    VStack(alignment: .leading, spacing: 8) {
      WrapLayout {
        if let s = manga.status, !s.isEmpty {
          Badge(text: s, style: .soft)
        }
        if let f = manga.format, !f.isEmpty {
          Badge(text: f)
        }
        if let sc = manga.averageScore, sc > 0 {
          Badge(text: "★ \(sc)%", style: .warning)
        }
        if let y = manga.year, y > 0 {
          Badge(text: String(y))
        }
      }
      if let d = manga.description, !d.isEmpty {
        Text(d.strippedHTML).font(.subheadline).foregroundStyle(.secondary)
      }
      if !metaLine.isEmpty {
        Text(metaLine).font(.caption).foregroundStyle(.tertiary)
      }
      if let genres = manga.genres, !genres.isEmpty {
        WrapLayout {
          ForEach(genres, id: \.self) { Badge(text: $0, style: .outline) }
        }
      }
    }
  }

  private var metaLine: String {
    var parts: [String] = []
    if let a = manga.authors, !a.isEmpty {
      parts.append("By \(a.joined(separator: ", "))")
    }
    if let c = manga.chapters, c > 0 {
      parts.append("\(c) chapters (AniList)")
    }
    if let v = manga.volumes, v > 0 {
      parts.append("\(v) volumes (AniList)")
    }
    return parts.joined(separator: " · ")
  }
}

/// JSONValue lets small mixed-type bodies stay one-liners.
enum JSONValue: Encodable {
  case string(String)
  case int(Int64)
  case bool(Bool)

  func encode(to encoder: Encoder) throws {
    var c = encoder.singleValueContainer()
    switch self {
    case let .string(v): try c.encode(v)
    case let .int(v): try c.encode(v)
    case let .bool(v): try c.encode(v)
    }
  }
}

struct ChapterRow: View {
  @Environment(\.displayScale) private var displayScale
  let chapter: ChapterProgress
  var volumes = false
  var local = false
  var thumb = false

  private var volumeName: String {
    chapter.title.isEmpty ? "Volume \(chapter.label)" : chapter.title
  }

  var localThumb: URL?

  var body: some View {
    let width = min(40 * displayScale, 512)
    let maxPixelSize = CGSize(width: width, height: width * 7 / 5)
    HStack(spacing: 10) {
      if volumes, let localThumb {
        Color.clear
          .frame(width: 40, height: 56)
          .overlay(LocalImage(url: localThumb, maxPixelSize: maxPixelSize))
          .clipShape(RoundedRectangle(cornerRadius: 6))
      } else if volumes, thumb {
        Color.clear
          .frame(width: 40, height: 56)
          .overlay(ServerImage(path: "/api/v1/volumes/\(chapter.id)/cover",
                               maxPixelSize: maxPixelSize))
          .clipShape(RoundedRectangle(cornerRadius: 6))
      } else if volumes, local {
        RoundedRectangle(cornerRadius: 6)
          .fill(.quaternary)
          .frame(width: 40, height: 56)
      }
      VStack(alignment: .leading, spacing: 2) {
        if volumes {
          Text(volumeName).foregroundStyle(.primary)
          Text("\(chapter.totalPages) pages · \(humanBytes(chapter.bytes))")
            .font(.caption).foregroundStyle(.secondary)
        } else {
          let name = chapter.displayName ?? chapter.title
          Text(name.isEmpty ? "Chapter \(chapter.label)" : "\(chapter.label): \(name)")
            .foregroundStyle(chapter.downloaded || local ? .primary : .secondary)
          if let vol = chapter.displayVolume, !vol.isEmpty {
            Text(vol).font(.caption).foregroundStyle(.secondary)
          }
        }
      }
      Spacer()
      if !volumes, chapter.bytes > 0 {
        Text(humanBytes(chapter.bytes)).font(.caption2).foregroundStyle(.tertiary)
      }
      if local {
        Image(systemName: "iphone").font(.caption).foregroundStyle(.secondary)
      }
      if chapter.completed {
        Image(systemName: "checkmark.circle.fill").foregroundStyle(Theme.success)
      } else if chapter.readPages > 0 {
        Text("\(chapter.readPages)/\(chapter.totalPages)")
          .font(.caption).foregroundStyle(.secondary)
      } else if !volumes, !chapter.downloaded {
        Image(systemName: "icloud.slash").foregroundStyle(.secondary)
      }
    }
  }
}

enum ChapterOrder: String, CaseIterable {
  case number = "Number", name = "Name", read = "Read", missing = "Missing"

  func less(_ a: ChapterProgress, _ b: ChapterProgress) -> Bool? {
    switch self {
    case .number:
      return nil
    case .name:
      let r = a.label.localizedStandardCompare(b.label)
      return r == .orderedSame ? nil : r == .orderedAscending
    case .read:
      let x = a.readShare, y = b.readShare
      return x == y ? nil : x < y
    case .missing:
      return a.downloaded == b.downloaded ? nil : !a.downloaded
    }
  }
}

private extension ChapterProgress {
  var readShare: Double {
    completed ? 1 : Double(readPages) / Double(max(totalPages, 1))
  }
}

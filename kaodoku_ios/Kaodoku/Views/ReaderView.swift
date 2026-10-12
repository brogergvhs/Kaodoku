import SwiftUI

/// ReaderView shows pages in paged (LTR/RTL) or vertical strip mode. Online it
/// follows the server manifest window; with localChapters it reads device CBZs
/// fully offline. Pages downloaded to the device always load locally first.
struct ReaderView: View {
  @Environment(AppState.self) private var app
  @Environment(\.dismiss) private var dismiss
  @Environment(\.displayScale) private var displayScale
  let titleID: Int64
  let startChapter: Int64
  var volumes = false
  var localChapters: [LocalStore.Entry]?

  struct PageRef: Hashable, Sendable {
    var chapterID: Int64
    var label: String
    var page: Int
    var total: Int
    var url: String
    var localURL: URL?
    var crop: [Double]? // normalized [x, y, w, h] content box, paged trim
    var volume = false
    var transition = false // synthetic "end of chapter" cell
  }

  /// Per-page width/height ratios, so the strip reader knows each page's exact
  /// height up front. Seeded from downloads on load, cleared per session.
  @MainActor static var aspects: [String: CGFloat] = [:]

  struct DisplayPage: Hashable {
    enum Half { case leading, trailing }
    var ref: PageRef
    var half: Half?
  }

  @State private var pages: [PageRef] = []
  @State private var index = 0
  @State private var extendBase = ""
  @State private var lastChapterID: Int64 = 0
  @State private var noMore = false
  @State private var showBar = true
  @State private var showSettings = false
  @State private var scrollID: Int?
  @State private var loadFailed = false
  @State private var extendChapter: Int64?
  @State private var stripJump: StripReader.Jump?
  @State private var commandID = 0
  @State private var showChapters = false
  @State private var chapterRows: [ReaderChapterRow]?
  @State private var detectionDone = false

  /// stripLoader fetches a page for the UICollectionView reader: the device
  /// CBZ first, then the network, downsampled off the main thread.
  private var stripLoader: (PageRef, CGSize) async -> UIImage? {
    let api = app.api
    let enhanced = app.settings.readerImageQuality == "enhanced"
    return { ref, size in
      if let local = ref.localURL,
         let img = await LocalStore.pageImage(at: local, page: ref.page, maxPixelSize: size, enhanced: enhanced)
      {
        return img
      }
      guard let api, !ref.url.isEmpty, let data = try? await api.data("GET", ref.url) else { return nil }
      return await Task.detached(priority: .userInitiated) {
        UIImage.downsampled(data, maxPixelSize: size, enhanced: enhanced)
      }.value
    }
  }

  private var paged: Bool {
    (app.readerMode(forTitle: titleID) ?? app.settings.readerMode ?? "paged") != "strip"
  }

  private var rtl: Bool {
    app.settings.readerDir == "rtl"
  }

  @State private var isLandscape = false

  private var doubleActive: Bool {
    guard paged else { return false }
    switch app.settings.readerPageLayout ?? "single" {
    case "double": return true
    case "auto": return isLandscape
    default: return false
    }
  }

  private var splitWide: Bool {
    !doubleActive && (app.settings.readerSplitWide ?? false)
  }

  /// units chunks pages into paged display slots.
  private var units: [[DisplayPage]] {
    let dbl = doubleActive
    let split = splitWide
    var out: [[DisplayPage]] = []
    var pending: DisplayPage?
    var pendingChapter: Int64 = 0
    func flush() {
      if let p = pending {
        out.append([p])
        pending = nil
      }
    }
    func place(_ dp: DisplayPage, chapter: Int64) {
      guard dbl else {
        out.append([dp])
        return
      }
      if let p = pending, pendingChapter == chapter {
        out.append([p, dp])
        pending = nil
      } else {
        flush()
        pending = dp
        pendingChapter = chapter
      }
    }
    for ref in pages {
      if ref.transition {
        flush()
        out.append([DisplayPage(ref: ref)])
        continue
      }
      let known = Self.aspects["\(ref.volume ? "v" : "c")\(ref.chapterID)-\(ref.page)"]
      if let known, known > 1 {
        if split {
          place(DisplayPage(ref: ref, half: .leading), chapter: ref.chapterID)
          place(DisplayPage(ref: ref, half: .trailing), chapter: ref.chapterID)
        } else {
          flush()
          out.append([DisplayPage(ref: ref)])
        }
      } else {
        place(DisplayPage(ref: ref), chapter: ref.chapterID)
      }
    }
    flush()
    return out
  }

  private func unitIndex(forPage i: Int) -> Int {
    guard pages.indices.contains(i) else { return 0 }
    let ref = pages[i]
    return units.firstIndex { $0.contains { $0.ref == ref } } ?? 0
  }

  var body: some View {
    GeometryReader { geo in
      let pixels = CGSize(width: max(1, geo.size.width * displayScale),
                          height: max(1, geo.size.height * displayScale))
      ZStack {
        Color.black.ignoresSafeArea()
        if loadFailed {
          Label("This chapter can't be opened", systemImage: "exclamationmark.triangle")
            .foregroundStyle(.white)
        } else if pages.isEmpty {
          ProgressView().tint(.white)
        } else if paged {
          let us = units
          ScrollViewReader { proxy in
            ScrollView(.horizontal, showsIndicators: false) {
              LazyHStack(spacing: 0) {
                ForEach(us.indices, id: \.self) { u in
                  ReaderPage(unit: us[u], maxPixelSize: pixels,
                             active: abs(u - (scrollID ?? 0)) <= 1,
                             onImage: u == 0 ? { autoDetect(aspect: $0) } : nil)
                    .containerRelativeFrame([.horizontal, .vertical])
                }
              }
              .scrollTargetLayout()
            }
            .scrollTargetBehavior(.paging)
            .scrollPosition(id: $scrollID)
            .environment(\.layoutDirection, rtl ? .rightToLeft : .leftToRight)
            .ignoresSafeArea()
            .onChange(of: scrollID) {
              if let u = scrollID, us.indices.contains(u) {
                onSettleUnit(us[u])
              }
            }
            .onChange(of: geo.size.width) { repin(proxy) }
            .onChange(of: doubleActive) { repin(proxy) }
            .onChange(of: splitWide) { repin(proxy) }
          }
        } else {
          StripReader(
            pages: pages,
            startIndex: index,
            enhanced: app.settings.readerImageQuality == "enhanced",
            maxPixelSize: CGSize(width: pixels.width, height: 8192),
            estimateAspect: geo.size.width / max(geo.size.height, 1),
            jump: stripJump,
            comfort: app.readerComfort,
            loadImage: stripLoader,
            onPage: onSettle,
            onFinished: { i in
              if pages.indices.contains(i), !pages[i].transition {
                mark(pages[i])
              }
            }
          )
          .ignoresSafeArea()
        }
        if showBar {
          bar
        }
      }
      .statusBarHidden(!showBar)
      .onTapGesture { point in
        handleTap(point, width: geo.size.width)
      }
      .onChange(of: geo.size.width > geo.size.height, initial: true) { _, v in
        isLandscape = v
      }
      .onChange(of: paged) {
        stripJump = nil
        if paged {
          scrollID = unitIndex(forPage: index)
        }
      }
    }
    .task { await load(chapter: startChapter, resume: true) }
    .task(id: extendChapter) {
      guard let last = extendChapter, let api = app.api else { return }
      defer { extendChapter = nil }
      guard let m: Manifest = try? await api.get(extendBase + String(last)) else { return }
      guard !Task.isCancelled else { return }
      append(m.chapters)
    }
    .onDisappear {
      Self.aspects.removeAll()
      Task {
        await app.store.flush(app.online ? app.api : nil)
        await LocalStore.clearPageCache()
      }
    }
    .sheet(isPresented: $showSettings) {
      ReaderSettingsSheet(titleID: titleID, cropsAvailable: cropsAvailable)
    }
    .sheet(isPresented: $showChapters) {
      ReaderChapterSheet(rows: chapterRows,
                         currentID: pages.indices.contains(index) ? pages[index].chapterID : 0,
                         volumes: volumes,
                         onSelect: jumpToChapter)
        .task { await loadChapterRows() }
    }
  }

  private var bar: some View {
    VStack {
      HStack {
        Button("Close") { dismiss() }
        Spacer()
        if pages.indices.contains(index) {
          let p = pages[index]
          if p.transition {
            Text(p.label).lineLimit(1)
          } else {
            let name = volumes && Double(p.label) == nil ? p.label : "\(volumes ? "Vol" : "Ch") \(p.label)"
            Text("\(name) · \(p.page)/\(p.total)").lineLimit(1)
          }
        }
        Spacer()
        Button {
          showChapters = true
        } label: {
          Image(systemName: "list.bullet")
        }
        Button {
          showSettings = true
        } label: {
          Image(systemName: "gearshape")
        }
      }
      .padding()
      .background(.black.opacity(0.6))
      .foregroundStyle(.white)
      Spacer()
      bottomBar
    }
  }

  /// bottomBar scrubs within the current chapter.
  private var bottomBar: some View {
    let range = chapterIndices
    let pos = range.firstIndex(of: index) ?? 0
    return HStack(spacing: 12) {
      Text("\(pos + 1)").monospacedDigit()
      Slider(value: Binding(
        get: { Double(pos) },
        set: { jump(toPageIndex: range[min(max(Int($0.rounded()), 0), range.count - 1)]) }
      ), in: 0 ... Double(max(range.count - 1, 1)), step: 1)
        .disabled(range.count <= 1)
      Text("\(range.count)").monospacedDigit()
    }
    .font(.footnote)
    .padding(.horizontal)
    .padding(.vertical, 10)
    .background(.black.opacity(0.6))
    .foregroundStyle(.white)
    .environment(\.layoutDirection, paged && rtl ? .rightToLeft : .leftToRight)
  }

  private var chapterIndices: [Int] {
    guard pages.indices.contains(index) else { return [] }
    let cid = pages[index].chapterID
    return pages.indices.filter { pages[$0].chapterID == cid && !pages[$0].transition }
  }

  private func handleTap(_ point: CGPoint, width: CGFloat) {
    guard paged, !pages.isEmpty, width > 0 else {
      showBar.toggle()
      return
    }
    let leftStep = rtl ? 1 : -1
    switch point.x {
    case ..<(width / 3): step(by: leftStep)
    case (width * 2 / 3)...: step(by: -leftStep)
    default: showBar.toggle()
    }
  }

  /// repin forces the pager back onto the current page after rotation or a
  /// unit reshape.
  private func repin(_ proxy: ScrollViewProxy) {
    let target = unitIndex(forPage: index)
    scrollID = target
    Task { @MainActor in proxy.scrollTo(target, anchor: .center) }
  }

  private func step(by delta: Int) {
    let count = units.count
    let cur = scrollID ?? unitIndex(forPage: index)
    let target = min(max(cur + delta, 0), count - 1)
    guard target != cur else { return }
    withAnimation { scrollID = target }
  }

  private func onSettleUnit(_ unit: [DisplayPage]) {
    guard let last = unit.last else { return }
    if let i = pages.firstIndex(of: last.ref) {
      index = i
    }
    for ref in Set(unit.map(\.ref)) where !ref.transition {
      mark(ref)
    }
    extendIfNeeded()
  }

  private func jump(toPageIndex target: Int) {
    guard pages.indices.contains(target) else { return }
    index = target
    if paged {
      scrollID = unitIndex(forPage: target)
    } else {
      commandID += 1
      stripJump = StripReader.Jump(id: commandID, index: target)
    }
  }

  private func jumpToChapter(_ id: Int64) {
    if let i = pages.firstIndex(where: { $0.chapterID == id && !$0.transition }) {
      jump(toPageIndex: i)
      return
    }
    extendChapter = nil
    stripJump = nil
    pages = []
    noMore = false
    lastChapterID = 0
    loadFailed = false
    index = 0
    scrollID = 0
    Task { await load(chapter: id, resume: false) }
  }

  private func loadChapterRows() async {
    if let localChapters {
      chapterRows = localChapters.map {
        ReaderChapterRow(id: $0.id, label: $0.label, read: $0.isRead, available: true)
      }
      return
    }
    if !volumes, let api = app.api,
       let p: TitleReadProgress = try? await api.get("/api/v1/reader/titles/\(titleID)")
    {
      chapterRows = p.chapters.map {
        ReaderChapterRow(id: $0.id, label: $0.label, read: $0.completed, available: $0.downloaded)
      }
      return
    }
    var seen = Set<Int64>()
    chapterRows = pages.compactMap {
      guard !$0.transition, seen.insert($0.chapterID).inserted else { return nil }
      return ReaderChapterRow(id: $0.chapterID, label: $0.label, read: false, available: true)
    }
  }

  /// autoDetect flips a title to strip mode when the first page is very tall.
  private func autoDetect(aspect: CGFloat) {
    guard !detectionDone, aspect > 0 else { return }
    detectionDone = true
    guard aspect < 0.55, app.readerMode(forTitle: titleID) == nil,
          (app.settings.readerMode ?? "paged") != "strip" else { return }
    app.setReaderMode("strip", forTitle: titleID)
  }

  private func onSettle(_ i: Int) {
    guard pages.indices.contains(i) else { return }
    index = i
    if !pages[i].transition {
      mark(pages[i])
    }
    extendIfNeeded()
  }

  private func load(chapter: Int64, resume: Bool) async {
    if let localChapters {
      noMore = true
      let volumeMode = volumes
      let build = Task.detached(priority: .userInitiated) {
        var refs: [PageRef] = []
        var aspects: [String: CGFloat] = [:]
        let kind = volumeMode ? "Vol" : "Ch"
        for e in localChapters where e.pages > 0 {
          guard !Task.isCancelled else { break }
          let local = LocalStore.root.appendingPathComponent(e.path)
          guard FileManager.default.fileExists(atPath: local.path) else { continue }
          for (pi, aspect) in e.pageAspects.enumerated() where aspect > 0 {
            aspects["\(volumeMode ? "v" : "c")\(e.id)-\(pi + 1)"] = aspect
          }
          if let prev = refs.last {
            refs.append(PageRef(chapterID: prev.chapterID,
                                label: "End of \(kind) \(prev.label)  ·  Next: \(kind) \(e.label)",
                                page: 0, total: 0, url: "", localURL: nil,
                                volume: volumeMode, transition: true))
          }
          for page in 1 ... e.pages {
            let crop = e.pageCrops.flatMap { $0.indices.contains(page - 1) ? $0[page - 1] : nil }
            refs.append(PageRef(chapterID: e.id, label: e.label, page: page,
                                total: e.pages, url: "", localURL: local,
                                crop: crop, volume: volumeMode))
          }
        }
        if let prev = refs.last {
          refs.append(PageRef(chapterID: prev.chapterID,
                              label: "No more \(volumeMode ? "volumes" : "chapters")",
                              page: 0, total: 0, url: "", localURL: nil,
                              volume: volumeMode, transition: true))
        }
        return (refs, aspects)
      }
      let loaded = await withTaskCancellationHandler {
        await build.value
      } onCancel: {
        build.cancel()
      }
      guard !Task.isCancelled else { return }
      Self.aspects.merge(loaded.1) { _, new in new }
      pages = loaded.0
      if let a = localChapters.lazy.flatMap(\.pageAspects).first(where: { $0 > 0 }) {
        autoDetect(aspect: CGFloat(a))
      }
      let entry = localChapters.first { $0.id == chapter }
      let resumePage = entry.map { $0.isRead ? 1 : min($0.readPages + 1, max($0.pages, 1)) } ?? 1
      guard let i = pages.firstIndex(where: { $0.chapterID == chapter && $0.page == resumePage }) else {
        loadFailed = true
        return
      }
      index = i
      scrollID = unitIndex(forPage: i)
      return
    }
    guard let api = app.api else { return }
    let mode = volumes ? "&mode=volumes" : ""
    do {
      let m: Manifest = try await api.get("/api/v1/reader/titles/\(titleID)/manifest?chapter=\(chapter)\(mode)")
      extendBase = m.extendBase
      append(m.chapters)
      if resume {
        if let i = pages.firstIndex(where: { $0.chapterID == m.resumeChapterId && $0.page == m.resumePage }) {
          index = i
        }
        scrollID = unitIndex(forPage: index)
      }
    } catch {
      app.errorMessage = error.localizedDescription
    }
  }

  private func append(_ chapters: [Manifest.Chapter]) {
    var added = false
    for ch in chapters where !pages.contains(where: { $0.chapterID == ch.id && !$0.transition }) {
      let local = app.store.url(for: ch.id, volume: volumes)
      if let prev = pages.last(where: { !$0.transition }) {
        pages.append(transitionRef(after: prev, nextLabel: ch.label))
      }
      pages.append(contentsOf: ch.pages.map {
        PageRef(chapterID: ch.id, label: ch.label, page: $0.page, total: ch.pageCount,
                url: $0.url, localURL: local, crop: $0.crop, volume: volumes)
      })
      lastChapterID = ch.id
      added = true
    }
    if !added {
      if !noMore, let prev = pages.last(where: { !$0.transition }) {
        pages.append(transitionRef(after: prev, nextLabel: nil))
      }
      noMore = true
    }
  }

  private func transitionRef(after prev: PageRef, nextLabel: String?) -> PageRef {
    let kind = volumes ? "Vol" : "Ch"
    let text = nextLabel.map { "End of \(kind) \(prev.label)  ·  Next: \(kind) \($0)" }
      ?? "No more \(volumes ? "volumes" : "chapters")"
    return PageRef(chapterID: prev.chapterID, label: text,
                   page: 0, total: 0, url: "", localURL: nil,
                   volume: volumes, transition: true)
  }

  private func extendIfNeeded() {
    guard extendChapter == nil, !noMore, pages.count - index < 5, lastChapterID > 0 else { return }
    extendChapter = lastChapterID
  }

  private func mark(_ p: PageRef) {
    app.store.recordMark(id: p.chapterID, volume: volumes, page: p.page, totalPages: p.total)
    app.scheduleFlush()
  }

  private var cropsAvailable: Bool {
    guard pages.indices.contains(index) else { return false }
    let cid = pages[index].chapterID
    return pages.contains { $0.chapterID == cid && $0.crop != nil }
  }
}

/// ReaderChapterRow feeds the in-reader chapter list.
struct ReaderChapterRow: Identifiable {
  var id: Int64
  var label: String
  var read: Bool
  var available: Bool
}

struct ReaderChapterSheet: View {
  let rows: [ReaderChapterRow]?
  let currentID: Int64
  let volumes: Bool
  var onSelect: (Int64) -> Void
  @Environment(\.dismiss) private var dismiss

  var body: some View {
    NavigationStack {
      Group {
        if let rows {
          List(rows) { row in
            Button {
              onSelect(row.id)
              dismiss()
            } label: {
              HStack {
                Text("\(volumes ? "Vol" : "Ch") \(row.label)")
                  .foregroundStyle(row.available ? Color.primary : .secondary)
                Spacer()
                if row.id == currentID {
                  Image(systemName: "eye").foregroundStyle(Theme.primary)
                } else if row.read {
                  Image(systemName: "checkmark").foregroundStyle(Theme.success)
                }
              }
            }
            .disabled(!row.available)
            .nordRows()
          }
        } else {
          ProgressView()
        }
      }
      .nordScreen()
      .navigationTitle(volumes ? "Volumes" : "Chapters")
      .navigationBarTitleDisplayMode(.inline)
    }
    .presentationDetents([.medium, .large])
  }
}

struct ReaderSettingsSheet: View {
  @Environment(AppState.self) private var app
  let titleID: Int64
  var cropsAvailable = true
  @State private var cropsProcessing = false
  @State private var trimStrength = 1.0

  private var stripActive: Bool {
    (app.readerMode(forTitle: titleID) ?? app.settings.readerMode ?? "paged") == "strip"
  }

  private var marginLabel: String {
    String(format: "%g%%", app.readerTrimMargin * 100)
  }

  var body: some View {
    @Bindable var app = app
    NavigationStack {
      Form {
        Picker("Layout", selection: Binding(
          get: { app.settings.readerMode ?? "paged" },
          set: { app.settings.readerMode = $0; app.saveSettings() }
        )) {
          Text("Paged").tag("paged")
          Text("Long strip").tag("strip")
        }
        Picker("This title", selection: Binding(
          get: { app.readerMode(forTitle: titleID) ?? "default" },
          set: { app.setReaderMode($0 == "default" ? nil : $0, forTitle: titleID) }
        )) {
          Text("Default").tag("default")
          Text("Paged").tag("paged")
          Text("Long strip").tag("strip")
        }
        Group {
          Picker("Direction", selection: Binding(
            get: { app.settings.readerDir ?? "ltr" },
            set: { app.settings.readerDir = $0; app.saveSettings() }
          )) {
            Text("Left to right").tag("ltr")
            Text("Right to left").tag("rtl")
          }
          Picker("Pages", selection: Binding(
            get: { app.settings.readerPageLayout ?? "single" },
            set: { app.settings.readerPageLayout = $0; app.saveSettings() }
          )) {
            Text("Single").tag("single")
            Text("Double").tag("double")
            Text("Double in landscape").tag("auto")
          }
          Toggle("Split wide pages", isOn: Binding(
            get: { app.settings.readerSplitWide ?? false },
            set: { app.settings.readerSplitWide = $0; app.saveSettings() }
          ))
          Toggle("Trim page borders", isOn: $app.readerTrimBorders)
            .disabled(!cropsAvailable) // no crop data for this chapter yet
          if !cropsAvailable, cropsProcessing {
            Text("Page borders are being detected for this title — trimming will be available soon.")
              .font(.caption).foregroundStyle(.secondary)
          }
          if app.readerTrimBorders, cropsAvailable {
            VStack(alignment: .leading, spacing: 4) {
              Text("Trim strength").font(.caption).foregroundStyle(.secondary)
              Slider(value: $trimStrength, in: 0 ... 1) { editing in
                if !editing {
                  app.readerTrimStrength = trimStrength
                }
              }
            }
            Stepper("Margin: \(marginLabel)", value: Binding(
              get: { app.readerTrimMargin },
              set: { app.readerTrimMargin = min(max($0, 0), 0.05) }
            ), in: 0 ... 0.05, step: 0.005)
          }
        }
        .disabled(stripActive) // paged-only controls
        Picker("Quality", selection: Binding(
          get: { app.settings.readerImageQuality ?? "standard" },
          set: { app.settings.readerImageQuality = $0; app.saveSettings() }
        )) {
          Text("Standard").tag("standard")
          Text("Enhanced").tag("enhanced")
        }
        Section("Eye comfort") {
          VStack(alignment: .leading, spacing: 4) {
            Text("Warmth").font(.caption).foregroundStyle(.secondary)
            HStack {
              Image(systemName: "sun.max")
              Slider(value: $app.readerWarmth, in: 0 ... 1)
              Image(systemName: "flame.fill")
            }
            .foregroundStyle(.secondary)
          }
          VStack(alignment: .leading, spacing: 4) {
            Text("Dimming").font(.caption).foregroundStyle(.secondary)
            HStack {
              Image(systemName: "circle.lefthalf.filled")
              Slider(value: $app.readerDim, in: 0 ... 1)
              Image(systemName: "moon.fill")
            }
            .foregroundStyle(.secondary)
          }
          Toggle("White pages only", isOn: $app.readerWhiteOnly)
          Text("Keeps colored pages true — only white/near-white areas are tinted and dimmed.")
            .font(.caption).foregroundStyle(.secondary)
        }
      }
      .navigationTitle("Reader")
      .navigationBarTitleDisplayMode(.inline)
      .task {
        trimStrength = app.readerTrimStrength
        guard !cropsAvailable, let api = app.api,
              let activity: TitleActivity = try? await api.get("/api/v1/library/\(titleID)/activity")
        else { return }
        cropsProcessing = activity.isActive("detect_crops", verb: "detecting crops")
      }
    }
    .presentationDetents([.height(660)])
  }
}

/// ReaderPage renders one paged display unit.
struct ReaderPage: View {
  @Environment(AppState.self) private var app
  @Environment(\.layoutDirection) private var layoutDirection
  let unit: [ReaderView.DisplayPage]
  let maxPixelSize: CGSize
  var active = true
  var onImage: ((CGFloat) -> Void)?
  @State private var zoom: CGFloat = 1
  @State private var images: [Int: UIImage] = [:]
  @State private var loadedDecode = DecodeParams(width: 0, trim: false, strength: -1, margin: -1)
  @State private var failed = false

  private var transition: Bool {
    unit.first?.ref.transition == true
  }

  private struct DecodeParams: Hashable {
    let width: CGFloat
    let trim: Bool
    let strength: Double
    let margin: Double
  }

  private struct LoadKey: Hashable {
    let active: Bool
    let unit: [ReaderView.DisplayPage]
    let enhanced: Bool
    let decode: DecodeParams
  }

  private var enhanced: Bool {
    app.settings.readerImageQuality == "enhanced"
  }

  private var decodeParams: DecodeParams {
    DecodeParams(width: maxPixelSize.width, trim: app.readerTrimBorders,
                 strength: app.readerTrimStrength, margin: app.readerTrimMargin)
  }

  var body: some View {
    content
      .onChange(of: active) { _, a in
        if !a {
          images = [:]; failed = false; zoom = 1
        }
      }
      .onChange(of: unit) {
        images = [:]
        failed = false
        zoom = 1
      }
      .onChange(of: enhanced) {
        images = [:]
        failed = false
      }
      .task(id: LoadKey(active: active, unit: unit, enhanced: enhanced, decode: decodeParams)) {
        await loadIfNeeded()
      }
  }

  @ViewBuilder private var content: some View {
    if transition {
      Text(unit.first?.ref.label ?? "")
        .font(.callout)
        .foregroundStyle(.secondary)
    } else if images.count == unit.count {
      ZoomablePage(slots: slots, zoom: $zoom, params: app.readerComfort)
    } else if failed {
      Label("Page failed to load", systemImage: "exclamationmark.triangle").foregroundStyle(.white)
    } else {
      ProgressView().tint(.white)
    }
  }

  /// slots maps the reading-order unit into visual left-to-right slots for
  /// the UIKit zoom view.
  private var slots: [ZoomablePage.Slot] {
    let rtl = layoutDirection == .rightToLeft
    let ordered = rtl ? Array(unit.enumerated().reversed()) : Array(unit.enumerated())
    return ordered.compactMap { i, dp in
      guard let img = images[i] else { return nil }
      let rect = switch dp.half {
      case nil:
        CGRect(x: 0, y: 0, width: 1, height: 1)
      case .leading:
        CGRect(x: rtl ? 0.5 : 0, y: 0, width: 0.5, height: 1)
      case .trailing:
        CGRect(x: rtl ? 0 : 0.5, y: 0, width: 0.5, height: 1)
      }
      return ZoomablePage.Slot(image: img, contentsRect: rect)
    }
  }

  private func loadIfNeeded() async {
    guard active, !transition else { return }
    let params = decodeParams
    let reload = !images.isEmpty && loadedDecode != params
    guard reload || images.count < unit.count else { return }
    for (i, dp) in unit.enumerated() where reload || images[i] == nil {
      guard let raw = await load(dp) else {
        if !Task.isCancelled {
          failed = true
        }
        return
      }
      guard !Task.isCancelled else { return }
      var img = raw
      if params.trim, let crop = dp.ref.crop,
         let cut = raw.cropped(normalized: crop, strength: params.strength, margin: params.margin)
      {
        img = cut
      }
      images[i] = img
      if i == 0 {
        onImage?(raw.size.width / max(raw.size.height, 1))
      }
    }
    loadedDecode = params
  }

  private func load(_ dp: ReaderView.DisplayPage) async -> UIImage? {
    let size = dp.half == nil ? maxPixelSize
      : CGSize(width: maxPixelSize.width * 2, height: maxPixelSize.height)
    let enhanced = enhanced
    let ref = dp.ref
    if let local = ref.localURL,
       let img = await LocalStore.pageImage(at: local, page: ref.page, maxPixelSize: size, enhanced: enhanced)
    {
      return img
    }
    guard let api = app.api, !ref.url.isEmpty, let data = try? await api.data("GET", ref.url) else {
      return nil
    }
    let decode = Task.detached(priority: .userInitiated) {
      UIImage.downsampled(data, maxPixelSize: size, enhanced: enhanced)
    }
    return await withTaskCancellationHandler { await decode.value } onCancel: { decode.cancel() }
  }
}

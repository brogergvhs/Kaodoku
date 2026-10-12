import CoreImage.CIFilterBuiltins
import Foundation
import ImageIO
import UIKit

private actor PageArchiveCache {
  private var archives: [URL: ZipArchive] = [:]

  func archive(at url: URL) -> ZipArchive? {
    if let archive = archives[url] {
      return archive
    }
    guard let archive = try? ZipArchive(url: url) else { return nil }
    // Cache the last 8 opened archives (adjacent-chapter navigation).
    if archives.count >= 8 {
      archives.removeAll(keepingCapacity: true)
    }
    archives[url] = archive
    return archive
  }

  func clear() {
    archives.removeAll()
  }
}

/// LocalStore owns offline content in Documents (visible in the Files app as
/// the "Kaodoku" folder): one directory per title with CBZs inside, an index
/// at .index.json, and a queue of progress marks made while offline.
@Observable @MainActor
final class LocalStore {
  struct Entry: Codable, Identifiable, Sendable {
    var id: Int64
    var titleId: Int64
    var titleName: String
    var label: String
    var path: String
    var pages: Int
    var size: Int64
    var readPages: Int
    var completed: Bool
    var pageAspects: [Double]
    var pageCrops: [[Double]]?
    var volume: Bool

    var isRead: Bool {
      completed
    }

    var isVolume: Bool {
      volume
    }
  }

  /// TitleInfo snapshots the server title at download time so the Downloads
  /// grid and offline title page can render the web card/detail layout.
  struct TitleInfo: Codable, Identifiable, Sendable {
    var id: Int64
    var name: String
    var cover: String?
    var isAdult: Bool = false
    var detail: MangaDetail?
    var readCount: Int64 = 0
    var completedCount: Int64 = 0
    var discoveredCount: Int64 = 0
    var missingCount: Int64 = 0
  }

  private struct Index: Codable, Sendable {
    var titles: [TitleInfo] = []
    var chapters: [Entry] = []
  }

  private struct Loaded: Sendable {
    let chapters: [Int64: Entry]
    let titles: [Int64: TitleInfo]
    let queue: [QueuedMark]
    let needsPersist: Bool
    let corruptIndex: Bool
    let corruptQueue: Bool
  }

  private struct PreparedArchive: Sendable {
    let pages: Int
    let size: Int64
    let pageAspects: [Double]
    let pageCrops: [[Double]]?
    let thumbnail: Data?
  }

  struct QueuedMark: Codable, Hashable, Sendable {
    var chapterId: Int64
    var volumeId: Int64?
    var page: Int
    var totalPages: Int
    var readAt: Date
  }

  struct Pending: Identifiable {
    var id: Int64
    var titleId: Int64
    var label: String
    var pages: Int
    var volume: Bool
  }

  private(set) var chapters: [Int64: Entry] = [:]
  private(set) var titleInfo: [Int64: TitleInfo] = [:]
  private(set) var pending: [Int64: Pending] = [:]
  private(set) var activeDownload: Int64?
  private(set) var downloadProgress: Double = 0
  private(set) var persistenceError: String?
  private(set) var syncError: String?
  private(set) var flushing = false
  private(set) var lastSyncAt: Date?

  var pendingMarks: Int {
    queue.count
  }

  var queuedMarks: [QueuedMark] {
    queue
  }

  private var corruptIndex = false
  private var corruptQueue = false
  private var queue: [QueuedMark] = []
  private var progressDirty = false
  private var progressSaveTask: Task<Void, Never>?
  private var persistenceTask: Task<Void, Never>?

  private(set) var instance: String?

  nonisolated static let root = FileManager.default.urls(for: .documentDirectory, in: .userDomainMask)[0]
  nonisolated static func indexURL(_ instance: String) -> URL {
    root.appendingPathComponent(instance).appendingPathComponent(".index.json")
  }

  nonisolated static func queueURL(_ instance: String) -> URL {
    root.appendingPathComponent(instance).appendingPathComponent(".marks.json")
  }

  private nonisolated static let pageArchives = PageArchiveCache()

  var requiresRecovery: Bool {
    corruptIndex || corruptQueue
  }

  init() {
    try? (Self.root as NSURL).setResourceValue(true, forKey: .isExcludedFromBackupKey)
  }

  func load(instance: String?) async {
    self.instance = instance
    guard let instance else {
      chapters = [:]; titleInfo = [:]; queue = []
      corruptIndex = false; corruptQueue = false; persistenceError = nil
      return
    }
    let loaded = await Task.detached(priority: .utility) { Self.loadFiles(instance) }.value
    corruptIndex = loaded.corruptIndex
    corruptQueue = loaded.corruptQueue
    persistenceError = recoveryMessage
    chapters = loaded.chapters
    titleInfo = loaded.titles
    queue = loaded.queue
    if loaded.needsPersist {
      persistIndex()
    }
  }

  /// activate swaps the store to another server's downloads, persisting the
  /// current server's queue first.
  func activate(_ id: String?) async {
    guard id != instance else { return }
    await flush(nil)
    await load(instance: id)
  }

  private nonisolated static func loadFiles(_ instance: String) -> Loaded {
    let files = FileManager.default
    let indexURL = indexURL(instance)
    let queueURL = queueURL(instance)
    var entries: [Entry] = []
    var titles: [TitleInfo] = []
    var corruptIndex = false
    if let data = try? Data(contentsOf: indexURL) {
      if let idx = try? JSONDecoder().decode(Index.self, from: data) {
        entries = idx.chapters
        titles = idx.titles
      } else {
        corruptIndex = true
      }
    } else if files.fileExists(atPath: indexURL.path) {
      corruptIndex = true
    }
    let entryCount = entries.count
    entries.removeAll {
      $0.pages == 0 || !files.fileExists(atPath: Self.root.appendingPathComponent($0.path).path)
    }
    let chapters = Dictionary(entries.map { ($0.isVolume ? -$0.id : $0.id, $0) },
                              uniquingKeysWith: { a, _ in a })
    let titleIDs = Set(chapters.values.map(\.titleId))
    let titleCount = titles.count
    titles.removeAll { !titleIDs.contains($0.id) }
    let info = Dictionary(titles.map { ($0.id, $0) }, uniquingKeysWith: { a, _ in a })
    var queue: [QueuedMark] = []
    var corruptQueue = false
    if let data = try? Data(contentsOf: queueURL) {
      if let decoded = try? JSONDecoder().decode([QueuedMark].self, from: data) {
        queue = decoded
      } else {
        corruptQueue = true
      }
    } else if files.fileExists(atPath: queueURL.path) {
      corruptQueue = true
    }
    return Loaded(
      chapters: chapters,
      titles: info,
      queue: queue,
      needsPersist: !corruptIndex && (entries.count != entryCount || titles.count != titleCount
        || chapters.count != entries.count || info.count != titles.count),
      corruptIndex: corruptIndex,
      corruptQueue: corruptQueue
    )
  }

  /// prune drops index entries whose file was deleted through the Files app,
  /// and title records left without chapters.
  func prune() async {
    let entries = Array(chapters.values)
    let scan: Task<[Entry], Never> = Task.detached(priority: .utility) {
      var dead: [Entry] = []
      for entry in entries {
        guard !Task.isCancelled else { break }
        if entry.pages == 0
          || !FileManager.default.fileExists(atPath: Self.root.appendingPathComponent(entry.path).path)
        {
          dead.append(entry)
        }
      }
      return dead
    }
    let dead = await withTaskCancellationHandler {
      await scan.value
    } onCancel: {
      scan.cancel()
    }
    guard !Task.isCancelled else { return }
    var changed = false
    for entry in dead {
      let k = key(entry.id, volume: entry.isVolume)
      guard let current = chapters[k], current.path == entry.path,
            current.pages == 0
            || !FileManager.default.fileExists(
              atPath: Self.root.appendingPathComponent(current.path).path
            ) else { continue }
      chapters.removeValue(forKey: k)
      changed = true
    }
    let liveTitles = Set(chapters.values.map(\.titleId)).union(pending.values.map(\.titleId))
    let orphans = titleInfo.keys.filter { !liveTitles.contains($0) }
    for id in orphans {
      titleInfo.removeValue(forKey: id)
    }
    if changed || !orphans.isEmpty {
      persistIndex()
    }
  }

  private func key(_ id: Int64, volume: Bool) -> Int64 {
    volume ? -id : id
  }

  func isDownloaded(_ id: Int64, volume: Bool = false) -> Bool {
    url(for: id, volume: volume) != nil
  }

  func canStore(bytes: Int64) -> Bool {
    guard bytes > 0,
          let available = try? Self.root.resourceValues(
            forKeys: [.volumeAvailableCapacityForImportantUsageKey]
          ).volumeAvailableCapacityForImportantUsage else { return true }
    let headroom: Int64 = 100 << 20
    return available > headroom && bytes <= available - headroom
  }

  /// url returns the local CBZ if the entry exists and the file survived
  /// (users can delete files through the Files app).
  func url(for id: Int64, volume: Bool = false) -> URL? {
    guard let e = chapters[key(id, volume: volume)] else { return nil }
    let u = Self.root.appendingPathComponent(e.path)
    return FileManager.default.fileExists(atPath: u.path) ? u : nil
  }

  func entries(titleId: Int64) -> [Entry] {
    chapters.values.filter { $0.titleId == titleId }
      .sorted { $0.label.localizedStandardCompare($1.label) == .orderedAscending }
  }

  struct LocalTitle: Identifiable {
    var id: Int64
    var info: TitleInfo
    var entries: [Entry]
    var pending: [Pending] = []
    var chapterEntries: [Entry] {
      entries.filter { !$0.isVolume }
    }

    var volumeEntries: [Entry] {
      entries.filter(\.isVolume)
    }

    var pendingChapters: [Pending] {
      pending.filter { !$0.volume }
    }

    var pendingVolumes: [Pending] {
      pending.filter(\.volume)
    }

    var size: Int64 {
      entries.reduce(0) { $0 + $1.size }
    }

    var coverURL: URL? {
      info.cover.map(LocalStore.root.appendingPathComponent).flatMap {
        FileManager.default.fileExists(atPath: $0.path) ? $0 : nil
      }
    }
  }

  var titles: [LocalTitle] {
    var groups = Dictionary(grouping: chapters.values, by: \.titleId)
    let pendingGroups = Dictionary(grouping: pending.values, by: \.titleId)
    for id in titleInfo.keys where groups[id] == nil {
      groups[id] = []
    }
    for id in pendingGroups.keys where groups[id] == nil {
      groups[id] = []
    }
    return groups
      .map { id, list in
        LocalTitle(id: id,
                   info: titleInfo[id] ?? TitleInfo(id: id, name: list.first?.titleName ?? "…"),
                   entries: list.sorted { $0.label.localizedStandardCompare($1.label) == .orderedAscending },
                   pending: (pendingGroups[id] ?? [])
                     .sorted { $0.label.localizedStandardCompare($1.label) == .orderedAscending })
      }
      .sorted { $0.info.name.localizedStandardCompare($1.info.name) == .orderedAscending }
  }

  func beginPending(_ items: [Pending]) {
    for p in items {
      pending[key(p.id, volume: p.volume)] = p
    }
  }

  func markActive(_ id: Int64, volume: Bool) {
    activeDownload = key(id, volume: volume)
    downloadProgress = 0
  }

  func isActive(_ p: Pending) -> Bool {
    activeDownload == key(p.id, volume: p.volume)
  }

  /// setProgress records the active item's fraction, stepping by 1% to avoid
  /// churning the UI on every byte.
  func setProgress(_ fraction: Double) {
    guard fraction >= 1 || abs(fraction - downloadProgress) >= 0.01 else { return }
    downloadProgress = fraction
  }

  func clearPending(titleId: Int64) {
    pending = pending.filter { $0.value.titleId != titleId }
    activeDownload = nil
    if !chapters.values.contains(where: { $0.titleId == titleId }) {
      titleInfo.removeValue(forKey: titleId)
      persistIndex()
    }
  }

  func clearSyncError() {
    syncError = nil
  }

  func clearPersistenceError() {
    persistenceError = nil
  }

  func retryPersistence() {
    persistenceError = nil
    persistIndex()
    if let instance {
      persist(queue, to: Self.queueURL(instance))
    }
  }

  func resetCorruptMetadata() async {
    guard let instance else { return }
    do {
      for (corrupt, url) in [(corruptIndex, Self.indexURL(instance)), (corruptQueue, Self.queueURL(instance))]
        where corrupt && FileManager.default.fileExists(atPath: url.path)
      {
        try FileManager.default.moveItem(
          at: url,
          to: url.appendingPathExtension("corrupt-\(UUID().uuidString)")
        )
      }
      await load(instance: instance)
    } catch {
      persistenceError = "Metadata could not be reset. Check Files access, then retry. \(error.localizedDescription)"
    }
  }

  /// saveTitle snapshots the title card data and writes the cover image next
  /// to the chapters, so Downloads renders the web layout fully offline.
  func saveTitle(_ info: TitleInfo, coverData: Data?) {
    var info = info
    let dir = dirName(titleId: info.id, titleName: info.name)
    if let coverData {
      let path = "\(dir)/cover.jpg"
      try? FileManager.default.createDirectory(at: Self.root.appendingPathComponent(dir, isDirectory: true),
                                               withIntermediateDirectories: true)
      if (try? coverData.write(to: Self.root.appendingPathComponent(path), options: .atomic)) != nil {
        info.cover = path
      }
    }
    if info.cover == nil {
      info.cover = titleInfo[info.id]?.cover
    }
    titleInfo[info.id] = info
    persistIndex()
  }

  /// syncRead refreshes local read state from the server's chapter list.
  /// Chapters with queued (unflushed) offline marks are skipped — the local
  /// state is newer than what the server knows.
  func syncRead(_ list: [ChapterProgress], volumes: Bool = false) {
    var changed = false
    let pending = volumes ? Set(queue.compactMap(\.volumeId)) : Set(queue.map(\.chapterId))
    for ch in list {
      guard !pending.contains(ch.id) else { continue }
      let k = key(ch.id, volume: volumes)
      guard var e = chapters[k], e.readPages != ch.readPages || e.isRead != ch.completed else { continue }
      e.readPages = ch.readPages
      e.completed = ch.completed
      chapters[k] = e
      changed = true
    }
    if changed {
      persistIndex()
    }
  }

  /// save moves a downloaded temp file into place; sanitized names that would
  /// collide across titles or chapters get an id suffix.
  func save(file src: URL, chapterID: Int64, titleId: Int64, titleName: String, label: String,
            readPages: Int = 0, completed: Bool = false, volume: Bool = false) async throws
  {
    defer { try? FileManager.default.removeItem(at: src) }
    let prepared = try await Task.detached(priority: .utility) {
      try Self.prepareArchive(src, thumbnail: volume)
    }.value
    let dirName = dirName(titleId: titleId, titleName: titleName)
    try FileManager.default.createDirectory(at: Self.root.appendingPathComponent(dirName, isDirectory: true),
                                            withIntermediateDirectories: true)
    let base = volume
      ? (Double(label) == nil ? Self.safe(label) : "Volume \(Self.safe(label))")
      : "Chapter \(Self.safe(label))"
    var path = "\(dirName)/\(base).cbz"
    if chapters.values.contains(where: { $0.path == path && ($0.id != chapterID || $0.isVolume != volume) }) {
      path = "\(dirName)/\(base) (\(chapterID)).cbz"
    }
    let dest = Self.root.appendingPathComponent(path)
    if FileManager.default.fileExists(atPath: dest.path) {
      _ = try FileManager.default.replaceItemAt(dest, withItemAt: src)
    } else {
      try FileManager.default.moveItem(at: src, to: dest)
    }
    if let thumbnail = prepared.thumbnail {
      try? thumbnail.write(to: dest.appendingPathExtension("thumb.jpg"), options: .atomic)
    }
    chapters[key(chapterID, volume: volume)] = Entry(
      id: chapterID, titleId: titleId, titleName: titleName,
      label: label, path: path, pages: prepared.pages, size: prepared.size,
      readPages: readPages, completed: completed, pageAspects: prepared.pageAspects,
      pageCrops: prepared.pageCrops, volume: volume
    )
    pending.removeValue(forKey: key(chapterID, volume: volume))
    persistIndex()
  }

  /// pageAspects reads each page's width/height from the CBZ headers (no full
  /// decode) so the strip reader knows exact page heights.
  nonisolated static func pageAspects(at src: URL) -> [Double] {
    guard let zip = try? ZipArchive(url: src) else { return [] }
    return zip.imageEntries.map { entry -> Double in
      guard let data = zip.data(for: entry),
            let image = CGImageSourceCreateWithData(
              data as CFData, [kCGImageSourceShouldCache: false] as CFDictionary
            ),
            let props = CGImageSourceCopyPropertiesAtIndex(image, 0, nil) as? [CFString: Any],
            var w = props[kCGImagePropertyPixelWidth] as? Double,
            var h = props[kCGImagePropertyPixelHeight] as? Double, w > 0, h > 0 else { return 0 }
      let orientation = (props[kCGImagePropertyOrientation] as? NSNumber)?.intValue ?? 1
      if orientation >= 5, orientation <= 8 {
        swap(&w, &h)
      }
      return w / h
    }
  }

  private nonisolated static func prepareArchive(_ src: URL, thumbnail: Bool) throws -> PreparedArchive {
    let zip = try ZipArchive(url: src)
    let images = zip.imageEntries
    guard !images.isEmpty else { throw ZipArchive.ArchiveError.invalid }
    let aspects = pageAspects(at: src)
    let jpeg = thumbnail
      ? zip.data(for: images[0]).flatMap {
        UIImage.downsampled($0, maxPixelSize: CGSize(width: 240, height: 240))
      }
      .flatMap { $0.jpegData(compressionQuality: 0.7) }
      : nil
    let size = (try? FileManager.default.attributesOfItem(atPath: src.path))?[.size] as? Int64 ?? 0
    return PreparedArchive(pages: images.count, size: size, pageAspects: aspects,
                           pageCrops: pageCrops(in: zip), thumbnail: jpeg)
  }

  /// pageCrops reads the server-written kaodoku-crops.json from the archive.
  private nonisolated static func pageCrops(in zip: ZipArchive) -> [[Double]]? {
    struct CropsFile: Decodable {
      var v: Int
      var pages: [[Double]]
    }
    guard let entry = zip.entries.first(where: { $0.name == "kaodoku-crops.json" }),
          let data = zip.data(for: entry),
          let file = try? JSONDecoder().decode(CropsFile.self, from: data),
          (1 ... 2).contains(file.v) else { return nil } // v2 = tight boxes
    return file.pages
  }

  private func dirName(titleId: Int64, titleName: String) -> String {
    if let e = chapters.values.first(where: { $0.titleId == titleId }) {
      return e.path.split(separator: "/").dropLast().joined(separator: "/")
    }
    let prefix = instance.map { "\($0)/" } ?? ""
    let base = prefix + Self.safe(titleName)
    let taken = chapters.values.contains { $0.titleId != titleId && $0.path.hasPrefix(base + "/") }
    return taken ? "\(base) (\(titleId))" : base
  }

  func thumbURL(_ e: Entry) -> URL? {
    let u = Self.root.appendingPathComponent(e.path).appendingPathExtension("thumb.jpg")
    return FileManager.default.fileExists(atPath: u.path) ? u : nil
  }

  func delete(_ id: Int64, volume: Bool = false) {
    guard let e = chapters.removeValue(forKey: key(id, volume: volume)) else { return }
    let file = Self.root.appendingPathComponent(e.path)
    try? FileManager.default.removeItem(at: file)
    try? FileManager.default.removeItem(at: file.appendingPathExtension("thumb.jpg"))
    if !chapters.values.contains(where: { $0.titleId == e.titleId }) {
      titleInfo.removeValue(forKey: e.titleId)
      try? FileManager.default.removeItem(at: file.deletingLastPathComponent())
    }
    persistIndex()
  }

  func deleteTitle(_ titleId: Int64) {
    for e in chapters.values where e.titleId == titleId {
      delete(e.id, volume: e.isVolume)
    }
  }

  /// recordMark batches server progress and throttles durable local progress
  /// writes while keeping every distinct chapter page used by read metrics.
  func recordMark(id: Int64, volume: Bool, page: Int, totalPages: Int) {
    var changed = false
    if var e = chapters[key(id, volume: volume)] {
      let read = max(e.readPages, page)
      let done = totalPages > 0 && read >= totalPages
      if read != e.readPages || done != e.isRead {
        e.readPages = read
        e.completed = done
        chapters[key(id, volume: volume)] = e
        changed = true
      }
    }
    let mark = QueuedMark(chapterId: volume ? 0 : id, volumeId: volume ? id : nil,
                          page: page, totalPages: totalPages, readAt: Date())
    if volume, let i = queue.firstIndex(where: { $0.volumeId == id }) {
      if page > queue[i].page {
        queue[i] = mark
        changed = true
      }
    } else if !queue.contains(where: { $0.chapterId == mark.chapterId && $0.volumeId == mark.volumeId && $0.page == page }) {
      queue.append(mark)
      changed = true
    }
    guard changed else { return }
    progressDirty = true
    guard progressSaveTask == nil else { return }
    // Throttle persistence; process death loses at most 2 seconds of marks.
    progressSaveTask = Task {
      try? await Task.sleep(for: .seconds(2))
      guard !Task.isCancelled else { return }
      progressSaveTask = nil
      persistProgress()
    }
  }

  /// flush sends progress through the batch endpoint and keeps it on failure.
  /// Marks queued during the request survive, and concurrent flushes are skipped.
  func flush(_ api: APIClient?) async {
    progressSaveTask?.cancel()
    progressSaveTask = nil
    persistProgress()
    let saved = persistenceTask
    guard let api, !queue.isEmpty, !flushing else {
      await saved?.value
      return
    }
    flushing = true
    defer { flushing = false }
    struct Batch: Encodable { var entries: [QueuedMark] }
    struct Receipt: Decodable {
      var applied: Int
      var skipped: [String: Int]?
    }
    await saved?.value
    while !queue.isEmpty {
      let sent = Array(queue.prefix(200))
      do {
        let data = try await api.data("POST", "/api/v1/reader/progress/batch",
                                      body: Batch(entries: sent))
        let receipt = try? JSONDecoder().decode(Receipt.self, from: data)
        let applied = receipt?.applied ?? sent.count
        if applied < sent.count {
          let why = (receipt?.skipped ?? [:])
            .sorted { $0.value > $1.value }
            .map { "\($0.key): \($0.value)" }
            .joined(separator: ", ")
          syncError = "The server accepted \(applied) of \(sent.count) reading marks. "
            + "Refused — \(why). \(queue.count) still queued."
        } else {
          syncError = nil
        }
        lastSyncAt = Date()
        let batch = Set(sent)
        queue.removeAll { batch.contains($0) }
      } catch {
        syncError = "Reading progress couldn't be synced. \(error.localizedDescription)"
        break
      }
    }
    if let instance {
      persist(queue, to: Self.queueURL(instance))
    }
  }

  /// pageImage extracts one page (1-based) from a local CBZ off the main actor.
  nonisolated static func pageImage(at url: URL, page: Int, maxPixelSize: CGSize, enhanced: Bool = false) async -> UIImage? {
    let load: Task<UIImage?, Never> = Task.detached(priority: .userInitiated) {
      guard let zip = await pageArchives.archive(at: url), !Task.isCancelled else { return nil }
      let images = zip.imageEntries
      guard images.indices.contains(page - 1),
            let data = zip.data(for: images[page - 1]),
            !Task.isCancelled else { return nil }
      return UIImage.downsampled(data, maxPixelSize: maxPixelSize, enhanced: enhanced)
    }
    let image = await withTaskCancellationHandler {
      await load.value
    } onCancel: {
      load.cancel()
    }
    return Task.isCancelled ? nil : image
  }

  nonisolated static func clearPageCache() async {
    await pageArchives.clear()
  }

  private func persistIndex() {
    guard let instance else { return }
    persist(Index(titles: Array(titleInfo.values), chapters: Array(chapters.values)),
            to: Self.indexURL(instance))
  }

  private func persistProgress() {
    guard progressDirty, let instance else { return }
    progressDirty = false
    persistIndex()
    persist(queue, to: Self.queueURL(instance))
  }

  private func persist(_ value: some Encodable & Sendable, to url: URL) {
    guard !requiresRecovery else { return }
    let previous = persistenceTask
    persistenceTask = Task.detached(priority: .utility) { [weak self] in
      await previous?.value
      do {
        try FileManager.default.createDirectory(at: url.deletingLastPathComponent(),
                                                withIntermediateDirectories: true)
        try JSONEncoder().encode(value).write(to: url, options: .atomic)
      } catch {
        await self?.reportPersistenceError(error.localizedDescription)
      }
    }
  }

  private func reportPersistenceError(_ detail: String) {
    persistenceError = "Check available device storage, then retry. \(detail)"
  }

  private var recoveryMessage: String? {
    guard requiresRecovery else { return nil }
    let subject = corruptIndex && corruptQueue
      ? "Offline library metadata and queued progress are"
      : corruptIndex ? "Offline library metadata is" : "Queued offline progress is"
    return "\(subject) unreadable. Retry after restoring the file, or reset metadata. Downloaded CBZ files will remain in Files."
  }

  private nonisolated static func safe(_ name: String) -> String {
    let cleaned = name.components(separatedBy: CharacterSet(charactersIn: "/\\:?*\"<>|")).joined(separator: " ")
      .trimmingCharacters(in: .whitespaces)
    return cleaned.isEmpty ? "Untitled" : String(cleaned.prefix(100))
  }
}

extension UIImage {
  nonisolated func cropped(normalized c: [Double], strength: Double = 1, margin: Double = 0) -> UIImage? {
    guard c.count == 4, let cg = cgImage else { return nil }
    let s = min(max(strength, 0), 1)
    let left = max(0, c[0] * s - margin)
    let top = max(0, c[1] * s - margin)
    let right = max(0, (1 - c[0] - c[2]) * s - margin)
    let bottom = max(0, (1 - c[1] - c[3]) * s - margin)
    let w = CGFloat(cg.width), h = CGFloat(cg.height)
    let rect = CGRect(x: left * w, y: top * h,
                      width: max(0, 1 - left - right) * w,
                      height: max(0, 1 - top - bottom) * h).integral
    guard rect.width >= 8, rect.height >= 8,
          rect.width < w || rect.height < h,
          let cut = cg.cropping(to: rect) else { return nil }
    return UIImage(cgImage: cut, scale: scale, orientation: imageOrientation)
  }

  /// Decode only the pixels that can be displayed, with a GPU-safe long side.
  /// enhanced doubles the decode budget and adds a GPU clarity pass.
  nonisolated static func downsampled(_ data: Data, maxPixelSize: CGSize, enhanced: Bool = false) -> UIImage? {
    let budget = enhanced
      ? CGSize(width: maxPixelSize.width * 2, height: maxPixelSize.height * 2)
      : maxPixelSize
    guard let src = CGImageSourceCreateWithData(data as CFData, [kCGImageSourceShouldCache: false] as CFDictionary) else {
      return nil
    }
    var maxPixel = min(2600, max(budget.width, budget.height))
    if let props = CGImageSourceCopyPropertiesAtIndex(src, 0, nil) as? [CFString: Any],
       let w = props[kCGImagePropertyPixelWidth] as? CGFloat,
       let h = props[kCGImagePropertyPixelHeight] as? CGFloat, w > 0, h > 0
    {
      let long = max(w, h)
      let scale = min(1, min(budget.width / w, min(budget.height / h, 16000 / long)))
      maxPixel = long * scale
    }
    guard let cg = CGImageSourceCreateThumbnailAtIndex(src, 0, [
      kCGImageSourceCreateThumbnailFromImageAlways: true,
      kCGImageSourceShouldCacheImmediately: true,
      kCGImageSourceCreateThumbnailWithTransform: true,
      kCGImageSourceThumbnailMaxPixelSize: maxPixel,
    ] as CFDictionary) else { return nil }
    if enhanced, let sharp = ImageEnhancer.enhance(cg, targetLong: maxPixel) {
      return UIImage(cgImage: sharp)
    }
    return UIImage(cgImage: cg)
  }
}

/// Metal-backed clarity pass: Lanczos-upscale sources smaller than the decode
/// budget, then a subtle unsharp mask.
enum ImageEnhancer {
  nonisolated(unsafe) static let context = CIContext()

  nonisolated static func enhance(_ cg: CGImage, targetLong: CGFloat) -> CGImage? {
    var image = CIImage(cgImage: cg)
    let long = CGFloat(max(cg.width, cg.height))
    let scale = min(2, targetLong / long)
    if scale > 1.05 {
      let up = CIFilter.lanczosScaleTransform()
      up.inputImage = image
      up.scale = Float(scale)
      up.aspectRatio = 1
      image = up.outputImage ?? image
    }
    let sharpen = CIFilter.unsharpMask()
    sharpen.inputImage = image
    sharpen.radius = 1.2
    sharpen.intensity = 0.35
    image = sharpen.outputImage ?? image
    return context.createCGImage(image, from: image.extent)
  }
}

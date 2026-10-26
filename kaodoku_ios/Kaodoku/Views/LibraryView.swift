import SwiftUI

enum CoverCache {
  nonisolated(unsafe) static let shared: NSCache<NSString, UIImage> = {
    let c = NSCache<NSString, UIImage>()
    c.countLimit = 300
    return c
  }()

  static func key(_ id: String, _ size: CGSize) -> NSString {
    "\(id)|\(Int(size.width))x\(Int(size.height))" as NSString
  }
}

/// ServerImage loads an image through the authed client
/// (AsyncImage can't send the X-API-Key header).
struct ServerImage: View {
  @Environment(AppState.self) private var app
  let path: String
  let maxPixelSize: CGSize
  @State private var image: UIImage?

  var body: some View {
    Group {
      if let image {
        Image(uiImage: image).resizable().scaledToFill()
      } else {
        Rectangle().fill(.quaternary)
      }
    }
    .task(id: path) {
      guard !path.isEmpty, image == nil else { return }
      let cacheKey = CoverCache.key(path, maxPixelSize)
      if let cached = CoverCache.shared.object(forKey: cacheKey) {
        image = cached
        return
      }
      guard let api = app.api, let data = try? await api.data("GET", path) else { return }
      let decoded = await Task.detached(priority: .utility) {
        UIImage.downsampled(data, maxPixelSize: maxPixelSize)
      }.value
      guard !Task.isCancelled, let decoded else { return }
      CoverCache.shared.setObject(decoded, forKey: cacheKey)
      image = decoded
    }
  }
}

/// Cover renders a uniform 5:7 cover cell (the web card's aspect) regardless
/// of the image's own size: the clear frame owns the layout, the image only
/// fills and clips. Adult titles get the web's red outline.
struct Cover: View {
  @Environment(\.displayScale) private var displayScale
  let path: String
  var adult = false
  var local: URL?
  var targetWidth: CGFloat = 128

  var body: some View {
    let width = min(targetWidth * displayScale, 512)
    let maxPixelSize = CGSize(width: width, height: width * 7 / 5)
    Color.clear
      .aspectRatio(5 / 7, contentMode: .fit)
      .overlay {
        if let local {
          LocalImage(url: local, maxPixelSize: maxPixelSize)
        } else {
          ServerImage(path: path, maxPixelSize: maxPixelSize)
        }
      }
      .clipShape(RoundedRectangle(cornerRadius: 8))
      .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(Theme.error, lineWidth: adult ? 2 : 0))
  }
}

/// LocalImage loads a file-backed image (offline covers).
struct LocalImage: View {
  let url: URL
  let maxPixelSize: CGSize
  @State private var image: UIImage?

  var body: some View {
    Group {
      if let image {
        Image(uiImage: image).resizable().scaledToFill()
      } else {
        Rectangle().fill(.quaternary)
      }
    }
    .task(id: url) {
      let cacheKey = CoverCache.key(url.path, maxPixelSize)
      if let cached = CoverCache.shared.object(forKey: cacheKey) {
        image = cached
        return
      }
      let decoded = await Task.detached(priority: .utility) {
        guard let data = try? Data(contentsOf: url, options: .mappedIfSafe) else { return UIImage?.none }
        return UIImage.downsampled(data, maxPixelSize: maxPixelSize)
      }.value
      guard !Task.isCancelled, let decoded else { return }
      CoverCache.shared.setObject(decoded, forKey: cacheKey)
      image = decoded
    }
  }
}

func humanBytes(_ n: Int64) -> String {
  n > 0 ? ByteCountFormatter.string(fromByteCount: n, countStyle: .file) : "0 B"
}

/// BarView is the web "bar" partial: green read share, primary completed share.
struct BarView: View {
  let read: Double
  let full: Double

  var body: some View {
    GeometryReader { geo in
      HStack(spacing: 0) {
        Rectangle().fill(Theme.success).frame(width: geo.size.width * min(read, 1))
        Rectangle().fill(Theme.primary).frame(width: geo.size.width * max(min(full, 1) - min(read, 1), 0))
        Color.clear
      }
    }
    .frame(height: 6)
    .background(Theme.neutral)
    .clipShape(Capsule())
  }
}

/// Badge mirrors the web badge variants (soft / ghost / outline / warning).
struct Badge: View {
  enum Style { case soft, ghost, outline, warning }
  let text: String
  var style: Style = .ghost

  var body: some View {
    Text(text)
      .font(.caption2)
      .padding(.horizontal, 8)
      .padding(.vertical, 3)
      .background {
        switch style {
        case .soft: Capsule().fill(Theme.primary.opacity(0.18))
        case .ghost: Capsule().fill(Theme.neutral)
        case .warning: Capsule().fill(Theme.warning.opacity(0.2))
        case .outline: Capsule().strokeBorder(Theme.line)
        }
      }
      .foregroundStyle(style == .soft ? Theme.primary : style == .warning ? Theme.warning : .primary)
  }
}

/// WrapLayout flows badge chips onto multiple lines like the web's flex-wrap.
struct WrapLayout: Layout {
  var spacing: CGFloat = 6

  func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache _: inout ()) -> CGSize {
    rows(in: proposal.width ?? .infinity, subviews: subviews).size
  }

  func placeSubviews(in bounds: CGRect, proposal _: ProposedViewSize, subviews: Subviews, cache _: inout ()) {
    for (i, origin) in rows(in: bounds.width, subviews: subviews).origins.enumerated() {
      subviews[i].place(at: CGPoint(x: bounds.minX + origin.x, y: bounds.minY + origin.y), proposal: .unspecified)
    }
  }

  private func rows(in width: CGFloat, subviews: Subviews) -> (origins: [CGPoint], size: CGSize) {
    var origins: [CGPoint] = []
    var x: CGFloat = 0, y: CGFloat = 0, rowHeight: CGFloat = 0, maxX: CGFloat = 0
    for sub in subviews {
      let size = sub.sizeThatFits(.unspecified)
      if x > 0, x + size.width > width {
        x = 0
        y += rowHeight + spacing
        rowHeight = 0
      }
      origins.append(CGPoint(x: x, y: y))
      x += size.width + spacing
      maxX = max(maxX, x - spacing)
      rowHeight = max(rowHeight, size.height)
    }
    return (origins, CGSize(width: width.isFinite ? width : maxX, height: y + rowHeight))
  }
}

/// TitleStats is the web card footer: counts + size line, then the bar.
struct TitleStats: View {
  let title: Title

  var body: some View {
    VStack(alignment: .leading, spacing: 4) {
      HStack(alignment: .firstTextBaseline) {
        if volumesOnly {
          Text("\(title.volumeReadCount)/\(title.volumeCount) vols").lineLimit(1)
          Spacer(minLength: 8)
          Text(humanBytes(title.volumeBytes))
        } else {
          Text("\(title.completedCount)/\(title.discoveredCount)").lineLimit(1)
          Spacer(minLength: 8)
          Text((title.missingCount > 0 ? "\(title.missingCount) missing · " : "") + humanBytes(title.sizeBytes))
        }
      }
      .font(.caption2)
      .foregroundStyle(.secondary)
      if volumesOnly {
        BarView(read: pct(title.volumeReadCount, title.volumeCount), full: 1)
      } else {
        BarView(read: pct(title.readCount, title.discoveredCount),
                full: pct(title.completedCount, title.discoveredCount))
      }
    }
  }

  private var volumesOnly: Bool {
    title.discoveredCount == 0 && title.volumeCount > 0
  }
}

func pct(_ a: Int64, _ b: Int64) -> Double {
  b > 0 ? Double(a) / Double(b) : 0
}

/// LibraryView mirrors the web library: server-side search, the web's filter
/// panel (favourites/monitored/progress/content/tags), the web's sort options,
/// and the web default order (last added, newest first).
struct LibraryView: View {
  @Environment(AppState.self) private var app
  @State private var titles: [Title] = []
  @State private var query = ""
  @State private var loading = true
  @State private var nextCursor = ""
  @State private var sort = "added"
  @State private var dir = "desc"
  @State private var fav = false
  @State private var monitored = ""
  @State private var progress = "all"
  @State private var content = "all"
  @State private var includeTags: Set<String> = []
  @State private var excludeTags: Set<String> = []
  @State private var showFilters = false
  @State private var path = NavigationPath()

  private var filtersActive: Bool {
    fav || !monitored.isEmpty || progress != "all" || content != "all"
      || !includeTags.isEmpty || !excludeTags.isEmpty || sort != "added" || dir != "desc"
  }

  var body: some View {
    NavigationStack(path: $path) {
      ScrollView {
        if loading {
          ProgressView().padding(.top, 80)
        } else if titles.isEmpty {
          Text(query.isEmpty && !filtersActive ? "No titles in the library yet." : "No matches.")
            .foregroundStyle(.secondary)
            .padding(.top, 80)
        }
        LibraryGrid(titles: titles).equatable()
        if !nextCursor.isEmpty, !loading {
          Button("Load more") { Task { await load(more: true) } }
            .buttonStyle(.bordered)
            .padding()
        }
      }
      .ignoresSafeArea(.keyboard, edges: .bottom)
      .nordScreen()
      .navigationTitle("Library")
      .navigationBarTitleDisplayMode(.inline)
      .connectionStatusToolbar()
      .navigationDestination(for: Int64.self) { TitleDetailView(titleID: $0) }
      .navigationDestination(for: CollectionsRoute.self) { _ in CollectionsView() }
      .navigationDestination(for: CollectionEntry.self) { CollectionMembersView(entry: $0) }
      .onChange(of: app.libraryNav) { _, nav in handleNav(nav) }
      .onAppear { handleNav(app.libraryNav) }
      .searchable(text: $query)
      .refreshable { await load() }
      .task(id: query) {
        if !query.isEmpty {
          do { try await Task.sleep(for: .milliseconds(400)) } catch { return }
        }
        await load()
      }
      .toolbar {
        Button {
          path.append(CollectionsRoute())
        } label: {
          Image(systemName: "square.stack.3d.up")
        }
        Button {
          showFilters = true
        } label: {
          Image(systemName: filtersActive
            ? "line.3.horizontal.decrease.circle.fill" : "line.3.horizontal.decrease.circle")
        }
      }
      .sheet(isPresented: $showFilters, onDismiss: { Task { await load() } }) {
        LibraryFiltersSheet(sort: $sort, dir: $dir, fav: $fav, monitored: $monitored,
                            progress: $progress, content: $content,
                            includeTags: $includeTags, excludeTags: $excludeTags)
      }
    }
  }

  private func handleNav(_ nav: AppState.LibraryNav?) {
    guard let nav else { return }
    app.libraryNav = nil
    switch nav {
    case let .title(id): path = NavigationPath([id])
    case .root: path = NavigationPath()
    }
    Task { await load() }
  }

  private func load(more: Bool = false) async {
    guard let api = app.api else { return }
    var params = ["limit=100", "sort=\(sort)", "dir=\(dir)"]
    if more, !nextCursor.isEmpty {
      params.append("cursor=\(nextCursor)")
    }
    if let q = query.addingPercentEncoding(withAllowedCharacters: .urlQueryAllowed), !query.isEmpty {
      params.append("q=\(q)")
    }
    if fav {
      params.append("favourite=1")
    }
    if !monitored.isEmpty {
      params.append("monitored=\(monitored)")
    }
    if progress != "all" {
      params.append("progress=\(progress)")
    }
    if content != "all" {
      params.append("content=\(content)")
    }
    if !includeTags.isEmpty {
      params.append("include_tags=\(csvTags(includeTags))")
    }
    if !excludeTags.isEmpty {
      params.append("exclude_tags=\(csvTags(excludeTags))")
    }
    do {
      let page: TitlePage = try await api.get("/api/v1/library?" + params.joined(separator: "&"))
      guard !Task.isCancelled else { return }
      titles = more ? titles + page.items : page.items
      nextCursor = page.nextCursor
    } catch APIError.unauthorized {
      app.signOut()
    } catch {
      guard !Task.isCancelled else { return }
      app.errorMessage = error.localizedDescription
    }
    loading = false
  }
}

func csvTags(_ set: Set<String>) -> String {
  set.sorted().joined(separator: ",").addingPercentEncoding(withAllowedCharacters: .urlQueryAllowed) ?? ""
}

/// LibraryFiltersSheet is the web library filter panel.
struct LibraryFiltersSheet: View {
  @Environment(AppState.self) private var app
  @Environment(\.dismiss) private var dismiss
  @Binding var sort: String
  @Binding var dir: String
  @Binding var fav: Bool
  @Binding var monitored: String
  @Binding var progress: String
  @Binding var content: String
  @Binding var includeTags: Set<String>
  @Binding var excludeTags: Set<String>
  @State private var options: [TagOption] = []

  var body: some View {
    NavigationStack {
      List {
        Group {
          Section("Sort") {
            Picker("Sort by", selection: $sort) {
              Text("Last added").tag("added")
              Text("Title").tag("title")
              Text("Missing chapters").tag("missing")
              Text("Failed chapters").tag("failed")
              Text("Total chapters").tag("chapters")
              Text("Total volumes").tag("volumes")
              Text("Size (total)").tag("size")
              Text("Size (chapters)").tag("size-chapters")
              Text("Size (volumes)").tag("size-volumes")
              Text("Rating").tag("rating")
              Text("Updated").tag("updated")
            }
            Picker("Direction", selection: $dir) {
              Text("Descending").tag("desc")
              Text("Ascending").tag("asc")
            }
            .pickerStyle(.segmented)
          }
          Section("Filters") {
            Toggle("Favourites only", isOn: $fav)
            Picker("Monitored", selection: $monitored) {
              Text("All").tag("")
              Text("Monitored").tag("1")
              Text("Not monitored").tag("0")
            }
            Picker("Progress", selection: $progress) {
              Text("All").tag("all")
              Text("Missing").tag("missing")
              Text("Complete").tag("complete")
              Text("No chapters").tag("empty")
            }
            Picker("Content", selection: $content) {
              Text("All").tag("all")
              Text("With chapters").tag("chapters")
              Text("Without chapters").tag("no-chapters")
              Text("With volumes").tag("volumes")
              Text("Without volumes").tag("no-volumes")
            }
          }
          TagFilterSection(options: options, include: $includeTags, exclude: $excludeTags)
        }
        .nordRows()
      }
      .nordScreen()
      .navigationTitle("Library filters")
      .navigationBarTitleDisplayMode(.inline)
      .toolbar {
        ToolbarItem(placement: .cancellationAction) {
          Button("Reset") {
            sort = "added"; dir = "desc"; fav = false; monitored = ""
            progress = "all"; content = "all"; includeTags = []; excludeTags = []
          }
        }
        ToolbarItem(placement: .confirmationAction) { Button("Done") { dismiss() } }
      }
      .task {
        guard options.isEmpty, let api = app.api else { return }
        struct TagList: Decodable { var items: [TagOption] }
        options = await ((try? api.get("/api/v1/tags") as TagList) ?? TagList(items: [])).items
      }
    }
  }
}

struct LibraryGrid: View, Equatable {
  let titles: [Title]

  var body: some View {
    LazyVGrid(columns: [GridItem(.adaptive(minimum: 110), spacing: 12)], spacing: 16) {
      ForEach(titles) { title in
        NavigationLink(value: title.id) {
          TitleCard(title: title)
        }
        .buttonStyle(.plain)
      }
    }
    .padding(.horizontal)
  }
}

/// TitleCard is the web libraryCard: cover, 2-line title, counts + size, bar.
struct TitleCard: View {
  let title: Title

  var body: some View {
    VStack(alignment: .leading, spacing: 6) {
      Cover(path: title.coverImage, adult: title.isAdult)
      Text(title.displayTitle)
        .font(.caption)
        .lineLimit(2, reservesSpace: true)
        .foregroundStyle(.primary)
      TitleStats(title: title)
    }
  }
}

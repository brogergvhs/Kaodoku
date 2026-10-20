import SwiftUI

/// SearchView mirrors the web search page: personalized suggestions when the
/// query is empty, 400ms-debounced live search, tag include/exclude filters,
/// sort, one-tap add, and load-more paging.
struct SearchView: View {
  @Environment(AppState.self) private var app
  @State private var query = ""
  @State private var items: [Manga] = []
  @State private var trendingCache: [Manga] = []
  @State private var hasMore = false
  @State private var page = 1
  @State private var busy = false
  @State private var includeTags: Set<String> = []
  @State private var excludeTags: Set<String> = []
  @State private var sort = ""
  @State private var dir = "desc"
  @State private var showFilters = false
  @State private var selected: Manga?
  @State private var searchTask: Task<Void, Never>?

  private var browsing: Bool {
    query.isEmpty && includeTags.isEmpty && excludeTags.isEmpty && sort.isEmpty
  }

  var body: some View {
    NavigationStack {
      ScrollView {
        SearchResultsGrid(items: items) { selected = $0 }
          .equatable()
        if busy {
          ProgressView().padding()
        } else if items.isEmpty {
          Text("Nothing found.").foregroundStyle(.secondary).padding(.top, 60)
        } else if hasMore, !browsing {
          Button("Load more") { startSearch(page: page + 1) }
            .buttonStyle(.bordered)
            .padding()
        }
      }
      .ignoresSafeArea(.keyboard, edges: .bottom)
      .nordScreen()
      .navigationTitle(browsing ? "For you" : "Search")
      .navigationBarTitleDisplayMode(.inline)
      .connectionStatusToolbar()
      .searchable(text: $query, prompt: "Search AniList")
      .toolbar {
        Button {
          showFilters = true
        } label: {
          Image(systemName: (includeTags.isEmpty && excludeTags.isEmpty && sort.isEmpty)
            ? "line.3.horizontal.decrease.circle" : "line.3.horizontal.decrease.circle.fill")
        }
      }
      .onChange(of: query) { debounceSearch() }
      .task { startSearch(page: 1) }
      .onDisappear { searchTask?.cancel() }
      .sheet(item: $selected) { manga in
        MangaDetailSheet(manga: manga) { updated in
          if let i = items.firstIndex(where: { $0.id == updated.id }) {
            items[i] = updated
          }
          if let i = trendingCache.firstIndex(where: { $0.id == updated.id }) {
            trendingCache[i] = updated
          }
        }
      }
      .sheet(isPresented: $showFilters, onDismiss: { debounceSearch(immediate: true) }) {
        SearchFiltersSheet(includeTags: $includeTags, excludeTags: $excludeTags, sort: $sort, dir: $dir)
      }
    }
  }

  private func loadTrending() async {
    guard let api = app.api else { return }
    if trendingCache.isEmpty {
      let list: SearchPage? = try? await api.get("/api/v1/wanted/trending")
      guard !Task.isCancelled else { return }
      trendingCache = list?.items ?? []
    }
    items = trendingCache
    hasMore = false
  }

  private func debounceSearch(immediate: Bool = false) {
    startSearch(page: 1, delayed: !immediate)
  }

  private func startSearch(page: Int, delayed: Bool = false) {
    searchTask?.cancel()
    searchTask = Task {
      if delayed {
        try? await Task.sleep(for: .milliseconds(400))
      }
      guard !Task.isCancelled else { return }
      busy = true
      if browsing {
        await loadTrending()
      } else {
        await search(page: page)
      }
      if !Task.isCancelled {
        busy = false
      }
    }
  }

  private func search(page requested: Int) async {
    guard let api = app.api else { return }
    let q = query
    let included = includeTags
    let excluded = excludeTags
    let ordering = sort
    let direction = dir
    var params = ["page=\(requested)"]
    if let encoded = q.addingPercentEncoding(withAllowedCharacters: .urlQueryAllowed), !q.isEmpty {
      params.append("q=\(encoded)")
    }
    if !included.isEmpty {
      params.append("include_tags=\(csvTags(included))")
    }
    if !excluded.isEmpty {
      params.append("exclude_tags=\(csvTags(excluded))")
    }
    if !ordering.isEmpty {
      params.append("sort=\(ordering)&dir=\(direction)")
    }
    guard let result: SearchPage = try? await api.get("/api/v1/wanted/search?" + params.joined(separator: "&")),
          !Task.isCancelled, q == query, included == includeTags, excluded == excludeTags,
          ordering == sort, direction == dir else { return }
    items = requested > 1 ? items + result.items : result.items
    hasMore = result.hasMore ?? false
    page = result.page ?? requested
  }
}

struct SearchResultsGrid: View, Equatable {
  let items: [Manga]
  let onSelect: (Manga) -> Void

  nonisolated static func == (lhs: SearchResultsGrid, rhs: SearchResultsGrid) -> Bool {
    lhs.items == rhs.items
  }

  var body: some View {
    LazyVGrid(columns: [GridItem(.adaptive(minimum: 110), spacing: 12)], spacing: 16) {
      ForEach(items) { manga in
        MangaCard(manga: manga) { onSelect(manga) }
      }
    }
    .padding(.horizontal)
  }
}

/// MangaCard matches the web card: uniform cover, adult outline, in-library
/// check, title, and the FORMAT · STATUS · ch · ★ caption.
struct MangaCard: View {
  let manga: Manga
  var onTap: () -> Void

  var body: some View {
    Button(action: onTap) {
      VStack(alignment: .leading, spacing: 6) {
        Cover(path: manga.coverImage, adult: manga.isAdult)
          .overlay(alignment: .topTrailing) {
            if manga.titleId != nil {
              Image(systemName: "checkmark.circle.fill")
                .foregroundStyle(.white, Theme.success)
                .padding(6)
            }
          }
        Text(manga.name)
          .font(.caption)
          .lineLimit(2, reservesSpace: true)
          .foregroundStyle(.primary)
        Text(manga.caption)
          .font(.caption2)
          .foregroundStyle(.secondary)
          .lineLimit(1)
      }
    }
    .buttonStyle(.plain)
  }
}

/// MangaDetailSheet mirrors the web detail: description plus the web's one-tap
/// add; the manual source picker stays available as the advanced path.
struct MangaDetailSheet: View {
  @Environment(AppState.self) private var app
  @Environment(\.dismiss) private var dismiss
  @State var manga: Manga
  var onUpdate: (Manga) -> Void = { _ in }
  @State private var matches: [Match]?
  @State private var busy = false
  @State private var error: String?

  private var canAdd: Bool {
    app.me?.can("library.add") == true
  }

  var body: some View {
    NavigationStack {
      List {
        Section {
          HStack(alignment: .top, spacing: 12) {
            Cover(path: manga.coverImage, targetWidth: 90).frame(width: 90)
            VStack(alignment: .leading, spacing: 4) {
              Text(manga.name).font(.headline)
              Text(manga.caption).font(.caption).foregroundStyle(.secondary)
              if manga.titleId != nil {
                Label("In library", systemImage: "checkmark.circle.fill")
                  .font(.caption).foregroundStyle(Theme.success)
              } else if canAdd {
                Button(busy ? "Adding…" : "Add to library") { add() }
                  .buttonStyle(.borderedProminent)
                  .disabled(busy)
              }
            }
          }
        }
        if let error {
          Text(error).foregroundStyle(.red)
        }
        if let desc = manga.description, !desc.isEmpty {
          Section("About") {
            Text(desc.strippedHTML).font(.subheadline)
          }
        }
        if manga.titleId == nil, canAdd {
          Section("Advanced") {
            if let matches {
              if matches.isEmpty {
                Text("No sources found.").foregroundStyle(.secondary)
              }
              ForEach(matches) { match in
                Button { track(match) } label: {
                  VStack(alignment: .leading) {
                    Text(match.title.isEmpty ? match.sourceId : match.title)
                    Text("\(match.sourceId) · \(match.chaptersFound) chapters · \(Int(match.confidence * 100))%")
                      .font(.caption).foregroundStyle(.secondary)
                  }
                }
                .disabled(match.chaptersFound == 0 || busy)
              }
            } else {
              Button("Choose source manually…") { findMatches() }
                .disabled(busy)
            }
          }
        }
      }
      .navigationTitle("Details")
      .navigationBarTitleDisplayMode(.inline)
      .toolbar { Button("Close") { dismiss() } }
    }
  }

  private func add() {
    guard let api = app.api, let pid = Int64(manga.providerId) else { return }
    busy = true
    Task {
      do {
        let title: Title = try await api.post("/api/v1/library/add", body: ["provider_id": pid])
        manga.titleId = title.id
        onUpdate(manga)
        openInLibrary(title.id)
      } catch { self.error = error.localizedDescription }
      busy = false
    }
  }

  private func openInLibrary(_ id: Int64) {
    dismiss()
    app.tab = 0
    app.libraryNav = .title(id)
  }

  private func findMatches() {
    guard let api = app.api else { return }
    busy = true
    Task {
      do {
        if manga.catalogId == 0, let pid = Int64(manga.providerId) {
          let cached: Manga = try await api.post("/api/v1/wanted", body: ["anilist_id": pid])
          manga.catalogId = cached.catalogId
        }
        let found: MatchList = try await api.post("/api/v1/wanted/matches", body: ["catalog_id": manga.catalogId])
        matches = found.items.sorted { $0.confidence > $1.confidence }
      } catch { self.error = error.localizedDescription }
      busy = false
    }
  }

  private func track(_ match: Match) {
    guard let api = app.api else { return }
    busy = true
    Task {
      do {
        let title: Title = try await api.post("/api/v1/wanted/track", body: ["match_id": match.id])
        manga.titleId = title.id
        onUpdate(manga)
        openInLibrary(title.id)
      } catch { self.error = error.localizedDescription }
      busy = false
    }
  }
}

// SearchFiltersSheet: include/exclude vocabulary pickers + sort, like the web
// filter panel.
struct SearchFiltersSheet: View {
  @Environment(AppState.self) private var app
  @Environment(\.dismiss) private var dismiss
  @Binding var includeTags: Set<String>
  @Binding var excludeTags: Set<String>
  @Binding var sort: String
  @Binding var dir: String
  @State private var options: [TagOption] = []

  var body: some View {
    NavigationStack {
      List {
        Group {
          Section("Sort") {
            Picker("Sort by", selection: $sort) {
              Text("Relevance").tag("")
              Text("Rating").tag("rating")
              Text("Title").tag("title")
              Text("Year").tag("year")
              Text("Chapters").tag("chapters")
            }
            if !sort.isEmpty {
              Picker("Direction", selection: $dir) {
                Text("Descending").tag("desc")
                Text("Ascending").tag("asc")
              }
              .pickerStyle(.segmented)
            }
          }
          TagFilterSection(options: options, include: $includeTags, exclude: $excludeTags)
        }
        .nordRows()
      }
      .nordScreen()
      .navigationTitle("Filters")
      .navigationBarTitleDisplayMode(.inline)
      .toolbar {
        ToolbarItem(placement: .cancellationAction) {
          Button("Reset") { includeTags = []; excludeTags = []; sort = ""; dir = "desc" }
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

struct TagFilterSection: View {
  let options: [TagOption]
  @Binding var include: Set<String>
  @Binding var exclude: Set<String>
  @State private var filter = ""

  private var groups: [(title: String, tags: [String])] {
    var order: [String] = []
    var byGroup: [String: [String]] = [:]
    for o in options {
      let g = o.group ?? (o.kind == "genre" ? "Genres" : "Other")
      if byGroup[g] == nil {
        order.append(g)
      }
      byGroup[g, default: []].append(o.name)
    }
    if let i = order.firstIndex(of: "Genres") {
      order.insert(order.remove(at: i), at: 0)
    }
    var out = order.map { (title: $0, tags: byGroup[$0] ?? []) }
    let custom = include.union(exclude).subtracting(options.map(\.name)).sorted()
    if !custom.isEmpty {
      out.append((title: "Custom", tags: custom))
    }
    let q = filter.trimmingCharacters(in: .whitespaces)
    guard !q.isEmpty else { return out }
    return out.compactMap { g in
      if g.title.localizedCaseInsensitiveContains(q) {
        return g
      }
      let tags = g.tags.filter { $0.localizedCaseInsensitiveContains(q) }
      return tags.isEmpty ? nil : (title: g.title, tags: tags)
    }
  }

  private var summary: String {
    var parts: [String] = []
    if !include.isEmpty {
      parts.append("\(include.count) included")
    }
    if !exclude.isEmpty {
      parts.append("\(exclude.count) excluded")
    }
    return parts.isEmpty ? "Any tags" : parts.joined(separator: " · ")
  }

  var body: some View {
    Section {
      HStack {
        TextField("Filter tags…", text: $filter)
        Button("Reset") { include = []; exclude = [] }
          .buttonStyle(.borderless)
          .disabled(include.isEmpty && exclude.isEmpty)
      }
      let shown = groups
      if shown.isEmpty {
        Text("Nothing found.").foregroundStyle(.secondary)
          .frame(maxWidth: .infinity)
      }
      ForEach(shown, id: \.title) { group in
        VStack(alignment: .leading, spacing: 10) {
          HStack(spacing: 8) {
            Text(group.title).font(.headline)
            Rectangle().fill(Theme.line).frame(height: 1)
          }
          WrapLayout(spacing: 8) {
            ForEach(group.tags, id: \.self) { TagChip(name: $0, include: $include, exclude: $exclude) }
          }
        }
        .padding(.vertical, 6)
      }
    } header: {
      HStack {
        Text("Tags & genres")
        Spacer()
        Text(summary).textCase(nil)
      }
    }
  }
}

struct TagChip: View {
  let name: String
  @Binding var include: Set<String>
  @Binding var exclude: Set<String>

  var body: some View {
    let tint: Color? = include.contains(name) ? Theme.success : exclude.contains(name) ? Theme.error : nil
    Button {
      if include.contains(name) {
        include.remove(name); exclude.insert(name)
      } else if exclude.contains(name) {
        exclude.remove(name)
      } else {
        include.insert(name)
      }
    } label: {
      Text(name)
        .padding(.horizontal, 14)
        .frame(minHeight: 40)
        .foregroundStyle(tint ?? .primary)
        .background((tint ?? .clear).opacity(0.12), in: RoundedRectangle(cornerRadius: 10))
        .overlay(RoundedRectangle(cornerRadius: 10).strokeBorder(tint ?? Theme.line))
        .contentShape(RoundedRectangle(cornerRadius: 10))
    }
    .buttonStyle(.plain)
  }
}

extension String {
  /// strippedHTML flattens AniList's HTML descriptions to plain text.
  var strippedHTML: String {
    replacingOccurrences(of: "<br>", with: "\n")
      .replacingOccurrences(of: "<[^>]+>", with: "", options: .regularExpression)
  }
}

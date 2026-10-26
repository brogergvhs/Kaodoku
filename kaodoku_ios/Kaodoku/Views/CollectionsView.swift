import SwiftUI

// Value-routed: view-destination links break in a path-driven stack.
struct CollectionsRoute: Hashable {}

struct CollectionsView: View {
  @Environment(AppState.self) private var app
  @State private var groups: CollectionGroups?
  @State private var mode = "author"
  @State private var showCreate = false
  @State private var newName = ""
  @State private var renaming: CollectionEntry?
  @State private var renameText = ""
  @State private var deleting: CollectionEntry?
  @State private var error: String?

  private var entries: [CollectionEntry]? {
    guard let groups else { return nil }
    switch mode {
    case "smart": return groups.smart
    case "custom": return groups.custom
    default: return groups.author
    }
  }

  var body: some View {
    VStack(spacing: 12) {
      picker
      ScrollView {
        content
      }
      .refreshable { await load() }
    }
    .nordScreen()
    .navigationBarTitleDisplayMode(.inline)
    .toolbar {
      if mode == "custom" {
        Button {
          newName = ""
          showCreate = true
        } label: {
          Image(systemName: "plus")
        }
      }
    }
    .onAppear { Task { await load() } } // re-fires after pop, unlike .task
    .alert("New collection", isPresented: $showCreate) {
      TextField("Name", text: $newName)
      Button("Create") { create() }
        .disabled(newName.trimmingCharacters(in: .whitespaces).isEmpty)
      Button("Cancel", role: .cancel) {}
    }
    .alert("Rename collection", isPresented: Binding(
      get: { renaming != nil }, set: {
        if !$0 {
          renaming = nil
        }
      }
    )) {
      TextField("Name", text: $renameText)
      Button("Rename") { rename() }
        .disabled(renameText.trimmingCharacters(in: .whitespaces).isEmpty)
      Button("Cancel", role: .cancel) {}
    }
    .confirmationDialog("Delete \"\(deleting?.name ?? "")\"? Titles stay in the library.",
                        isPresented: Binding(get: { deleting != nil }, set: {
                          if !$0 {
                            deleting = nil
                          }
                        }),
                        titleVisibility: .visible)
    {
      Button("Delete collection", role: .destructive) { delete() }
    }
  }

  private var picker: some View {
    Picker("Group", selection: $mode) {
      Text("Authors").tag("author")
      Text("Series").tag("smart")
      Text("Custom").tag("custom")
    }
    .pickerStyle(.segmented)
    .padding(.horizontal)
    .padding(.top, 8)
  }

  @ViewBuilder private var content: some View {
    if let entries {
      if entries.isEmpty {
        Text(mode == "custom" ? "No collections yet — create one with +." : "Nothing here yet.")
          .foregroundStyle(.secondary)
          .padding(.top, 60)
      }
      LazyVGrid(columns: [GridItem(.adaptive(minimum: 150), spacing: 12)], spacing: 16) {
        ForEach(entries, id: \.uid) { entry in
          NavigationLink(value: entry) {
            CollectionCard(entry: entry)
          }
          .buttonStyle(.plain)
          .contextMenu {
            if entry.id != nil {
              Button("Rename", systemImage: "pencil") {
                renameText = entry.name
                renaming = entry
              }
              Button("Delete", systemImage: "trash", role: .destructive) {
                deleting = entry
              }
            }
          }
        }
      }
      .padding(.horizontal)
      .padding(.top, 8)
    } else {
      ProgressView().padding(.top, 80)
    }
    if let error {
      Text(error).foregroundStyle(Theme.error).font(.footnote).padding()
    }
  }

  private func load() async {
    guard let api = app.api else { return }
    do {
      groups = try await api.get("/api/v1/collections")
      error = nil
    } catch {
      self.error = error.localizedDescription
    }
  }

  private func create() {
    mutate("POST", "/api/v1/collections", body: ["name": newName])
  }

  private func rename() {
    guard let id = renaming?.id else { return }
    renaming = nil
    mutate("PATCH", "/api/v1/collections/\(id)", body: ["name": renameText])
  }

  private func delete() {
    guard let id = deleting?.id else { return }
    deleting = nil
    mutate("DELETE", "/api/v1/collections/\(id)")
  }

  private func mutate(_ method: String, _ path: String, body: [String: String]? = nil) {
    guard let api = app.api else { return }
    Task {
      do {
        _ = try await api.data(method, path, body: body)
        await load()
      } catch { self.error = error.localizedDescription }
    }
  }
}

struct CollectionCard: View {
  let entry: CollectionEntry

  var body: some View {
    VStack(alignment: .leading, spacing: 6) {
      Color.clear
        .aspectRatio(5 / 7, contentMode: .fit)
        .overlay { collage }
        .clipShape(RoundedRectangle(cornerRadius: 8))
      Text(entry.name)
        .font(.caption)
        .lineLimit(2, reservesSpace: true)
        .foregroundStyle(.primary)
      HStack(alignment: .firstTextBaseline) {
        Text("\(entry.titleIds.count) titles").lineLimit(1)
        Spacer(minLength: 8)
        Text(humanBytes(entry.sizeBytes))
      }
      .font(.caption2)
      .foregroundStyle(.secondary)
      HStack {
        if entry.chapters > 0 {
          Text("\(entry.chapters) ch")
        }
        Spacer(minLength: 4)
        if entry.volumes > 0 {
          Text("\(entry.volumes) vol")
        }
        Spacer(minLength: 4)
        if entry.pages > 0 {
          Text("\(entry.pages) pages")
        }
      }
      .font(.caption2)
      .foregroundStyle(.secondary)
      BarView(read: Double(entry.readPct) / 100,
              full: entry.chapters + entry.volumes > 0 ? 1 : 0)
    }
  }

  /// Web card rules: 2 stack vertically; 2×2 grid; >4 -> three covers + "+N".
  @ViewBuilder private var collage: some View {
    let n = entry.titleIds.count
    if n == 2 {
      VStack(spacing: 8) {
        cover(entry.titleIds[0])
        cover(entry.titleIds[1])
      }
    } else {
      let shown = n > 4 ? 3 : min(n, 4)
      let extra = n > 4 ? n - 3 : 0
      let ids = Array(entry.titleIds.prefix(shown))
      VStack(spacing: 8) {
        HStack(spacing: 8) {
          cell(0, ids)
          cell(1, ids)
        }
        HStack(spacing: 8) {
          cell(2, ids)
          if extra > 0 {
            extraCell(extra)
          } else {
            cell(3, ids)
          }
        }
      }
    }
  }

  @ViewBuilder private func cell(_ i: Int, _ ids: [Int64]) -> some View {
    if i < ids.count {
      cover(ids[i])
    } else {
      Color.clear
    }
  }

  private func cover(_ id: Int64) -> some View {
    Color.clear
      .overlay {
        ServerImage(path: "/api/v1/covers/\(id)",
                    maxPixelSize: CGSize(width: 240, height: 340))
      }
      .clipShape(RoundedRectangle(cornerRadius: 8))
  }

  private func extraCell(_ n: Int) -> some View {
    RoundedRectangle(cornerRadius: 8)
      .fill(Theme.neutral)
      .overlay {
        Text("+\(n)")
          .font(.headline)
          .foregroundStyle(.secondary)
      }
  }
}

struct CollectionMembersView: View {
  @Environment(AppState.self) private var app
  let entry: CollectionEntry
  @State private var titles: [Title]?
  @State private var error: String?

  var body: some View {
    ScrollView {
      if let titles {
        if titles.isEmpty {
          Text("No titles.").foregroundStyle(.secondary).padding(.top, 60)
        }
        LazyVGrid(columns: [GridItem(.adaptive(minimum: 110), spacing: 12)], spacing: 16) {
          ForEach(titles) { title in
            NavigationLink(value: title.id) {
              TitleCard(title: title)
            }
            .buttonStyle(.plain)
            .contextMenu { removeButton(title) }
          }
        }
        .padding(.horizontal)
      } else {
        ProgressView().padding(.top, 80)
      }
      if let error {
        Text(error).foregroundStyle(Theme.error).font(.footnote).padding()
      }
    }
    .nordScreen()
    .navigationTitle(entry.name)
    .navigationBarTitleDisplayMode(.inline)
    .task { await load() }
  }

  @ViewBuilder private func removeButton(_ title: Title) -> some View {
    if let id = entry.id {
      Button("Remove from collection", systemImage: "minus.circle", role: .destructive) {
        remove(title, path: "/api/v1/collections/\(id)/titles/\(title.id)")
      }
    } else if let key = entry.key, entry.pinnedIds?.contains(title.id) == true,
              let escaped = key.addingPercentEncoding(withAllowedCharacters: .urlPathAllowed)
    {
      Button("Unpin from series", systemImage: "pin.slash", role: .destructive) {
        remove(title, path: "/api/v1/collections/smart/\(escaped)/pins/\(title.id)")
      }
    }
  }

  private func remove(_ title: Title, path: String) {
    guard let api = app.api else { return }
    Task {
      do {
        _ = try await api.data("DELETE", path)
        if entry.id != nil {
          titles?.removeAll { $0.id == title.id }
        } else {
          await load() // an unpinned title can still be a relation member
        }
      } catch { self.error = error.localizedDescription }
    }
  }

  private func load() async {
    guard let api = app.api else { return }
    guard !entry.titleIds.isEmpty else {
      titles = []
      return
    }
    let csv = entry.titleIds.map(String.init).joined(separator: ",")
    var all: [Title] = []
    var cursor = ""
    do {
      repeat {
        let page: TitlePage = try await api.get(
          "/api/v1/library?ids=\(csv)&limit=200&cursor=\(cursor)"
        )
        all += page.items
        cursor = page.nextCursor
      } while !cursor.isEmpty
      titles = all
      error = nil
    } catch {
      self.error = error.localizedDescription
    }
  }
}

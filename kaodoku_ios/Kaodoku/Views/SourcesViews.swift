import SwiftUI

/// TitleSourcesSheet mirrors the web title page's Sources section: linked
/// sources, find + match candidates, and manual page-URL linking — the missing
/// middle step between adding a title and downloading chapters.
struct TitleSourcesSheet: View {
  @Environment(AppState.self) private var app
  @Environment(\.dismiss) private var dismiss
  let titleID: Int64
  var onChanged: () -> Void = {}
  @State private var data: TitleSources?
  @State private var busy = false
  @State private var linkSource: SourcePick?
  @State private var linkURL = ""
  @State private var error: String?
  @State private var pollGen = 0

  var body: some View {
    NavigationStack {
      List {
        Group {
          Section("Linked sources") {
            if let data {
              if data.linked.isEmpty {
                Text("No source linked yet — chapters can't be found or downloaded until one is.")
                  .font(.caption).foregroundStyle(.secondary)
              }
              ForEach(data.linked) { link in
                HStack {
                  VStack(alignment: .leading, spacing: 2) {
                    HStack {
                      Text(link.name)
                      if link.active {
                        Badge(text: "active", style: .soft)
                      }
                    }
                    Text(link.url).font(.caption2).foregroundStyle(.secondary).lineLimit(1)
                  }
                  Spacer()
                  Button {
                    unlink(link)
                  } label: {
                    Image(systemName: "xmark.circle").foregroundStyle(Theme.error)
                  }
                  .buttonStyle(.borderless)
                  .disabled(busy)
                }
                .swipeActions {
                  Button("Unlink", systemImage: "minus.circle", role: .destructive) {
                    unlink(link)
                  }
                }
              }
            } else {
              ProgressView()
            }
          }
          if let data {
            Section("Found sources") {
              Button(data.finding ? "Searching sources…" : "Find sources") { find() }
                .disabled(data.finding || busy)
              if data.failed, let msg = data.error, !msg.isEmpty {
                Text(msg).font(.caption).foregroundStyle(Theme.error)
              }
              if data.matches.isEmpty, !data.finding {
                Text("No candidates yet — run a search.")
                  .font(.caption).foregroundStyle(.secondary)
              }
              ForEach(data.matches) { match in
                Button {
                  link(match)
                } label: {
                  VStack(alignment: .leading, spacing: 2) {
                    Text(match.sourceUrl.isEmpty ? match.sourceId : match.sourceUrl)
                      .lineLimit(1).truncationMode(.middle)
                    Text("\(match.sourceId) · \(match.chaptersFound) chapters · \(Int(match.confidence * 100))%")
                      .font(.caption).foregroundStyle(.secondary)
                  }
                }
                .disabled(busy || match.chaptersFound == 0)
              }
            }
            Section("Link a page URL") {
              Picker("Source", selection: $linkSource) {
                Text("Choose…").tag(SourcePick?.none)
                ForEach(data.sources) { Text($0.name).tag(Optional($0)) }
              }
              TextField("https://…", text: $linkURL)
                .keyboardType(.URL)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
              Button("Link this page") { linkByURL() }
                .disabled(linkSource == nil || linkURL.isEmpty || busy)
            }
          }
          if let error {
            Text(error).foregroundStyle(Theme.error)
          }
        }
        .nordRows()
      }
      .nordScreen()
      .navigationTitle("Sources")
      .navigationBarTitleDisplayMode(.inline)
      .toolbar { Button("Close") { dismiss() } }
      .task(id: pollGen) { await refreshLoop() }
    }
  }

  private func load() async {
    guard let api = app.api else { return }
    data = try? await api.get("/api/v1/library/\(titleID)/sources")
  }

  /// refreshLoop polls while a find job is running, like the web's HTMX poll.
  private func refreshLoop() async {
    await load()
    while !Task.isCancelled, data?.finding == true {
      do { try await Task.sleep(for: .seconds(2)) } catch { return }
      await load()
    }
  }

  /// find kicks the match job, then bumps pollGen to restart the poll loop on
  /// the view's task (a detached poller would outlive the sheet).
  private func find() {
    guard let api = app.api else { return }
    busy = true
    Task {
      defer { busy = false }
      do {
        _ = try await api.data("POST", "/api/v1/library/\(titleID)/sources/find")
        pollGen += 1
      } catch { self.error = error.localizedDescription }
    }
  }

  private func link(_ match: Match) {
    post("/api/v1/library/\(titleID)/sources/link", body: ["match_id": match.id]) {
      onChanged()
      dismiss()
    }
  }

  private func linkByURL() {
    guard let source = linkSource else { return }
    post("/api/v1/library/\(titleID)/sources/link-url",
         body: ["source_id": source.id, "url": linkURL.trimmingCharacters(in: .whitespaces)])
    {
      onChanged()
      dismiss()
    }
  }

  private func unlink(_ link: LinkedSource) {
    post("/api/v1/library/\(titleID)/sources/unlink", body: ["url": link.url]) {
      Task { await load() }
      onChanged()
    }
  }

  private func post(_ path: String, body: some Encodable, then done: @escaping () -> Void) {
    guard let api = app.api else { return }
    busy = true
    Task {
      defer { busy = false }
      do {
        _ = try await api.data("POST", path, body: body)
        done()
      } catch { self.error = error.localizedDescription }
    }
  }
}

import SwiftUI

/// JobsView is the native twin of the web dashboard's jobs panel.
struct JobsView: View {
  @Environment(AppState.self) private var app
  @State private var jobs: [Job] = []
  @State private var filter: String?
  @State private var didLoad = false
  @State private var note: String?

  private var canManage: Bool {
    app.me?.can("jobs.manage") == true
  }

  var body: some View {
    List {
      if didLoad, jobs.isEmpty {
        ContentUnavailableView("No jobs", systemImage: "checkmark.circle",
                               description: Text(filter == nil ? "The queue is empty."
                                 : "No \(filter!) jobs."))
          .nordRows()
      } else {
        ForEach(jobs) { job in
          JobRow(job: job)
            .swipeActions {
              if canManage, job.active {
                Button("Cancel", role: .destructive) { cancel(job) }
              }
            }
        }
        .nordRows()
      }
    }
    .nordScreen()
    .navigationTitle("Jobs")
    .navigationBarTitleDisplayMode(.inline)
    .refreshable { await load() }
    .task(id: filter) {
      while !Task.isCancelled {
        await load()
        try? await Task.sleep(for: .seconds(jobs.contains(where: \.active) ? 4 : 12))
      }
    }
    .toolbar {
      ToolbarItem(placement: .topBarLeading) {
        Menu {
          Picker("Status", selection: $filter) {
            Text("All").tag(String?.none)
            ForEach(["queued", "running", "failed", "dead", "done", "cancelled"], id: \.self) {
              Text($0.capitalized).tag(String?.some($0))
            }
          }
        } label: {
          Label("Filter", systemImage: "line.3.horizontal.decrease.circle")
        }
      }
      if canManage {
        ToolbarItem(placement: .topBarTrailing) {
          Menu {
            Button("Run queue now", systemImage: "play.fill") { runNow() }
            Menu {
              ForEach(startableJobs, id: \.0) { type, label in
                Button(label) { start(type) }
              }
            } label: {
              Label("Start a job", systemImage: "plus")
            }
          } label: {
            Image(systemName: "ellipsis.circle")
          }
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
          .task { try? await Task.sleep(for: .seconds(4)); self.note = nil }
      }
    }
  }

  private func load() async {
    guard let api = app.api else { return }
    var path = "/api/v1/jobs"
    if let filter {
      path += "?status=\(filter)"
    }
    if let list: JobList = try? await api.get(path) {
      jobs = list.items
    }
    didLoad = true
  }

  private func runNow() {
    guard let api = app.api else { return }
    Task {
      do {
        let s: RunSummary = try await api.post("/api/v1/jobs/run")
        note = "\(s.done) done, \(s.failed) failed"
        await load()
      } catch { note = error.localizedDescription }
    }
  }

  private func start(_ type: String) {
    guard let api = app.api else { return }
    Task {
      do {
        let _: Job = try await api.post("/api/v1/jobs/enqueue", body: ["type": type])
        note = "Queued \(jobLabel(type))"
        await load()
      } catch { note = error.localizedDescription }
    }
  }

  private func cancel(_ job: Job) {
    guard let api = app.api else { return }
    Task {
      do {
        _ = try await api.data("POST", "/api/v1/jobs/\(job.id)/cancel")
        await load()
      } catch { note = error.localizedDescription }
    }
  }
}

private struct JobRow: View {
  let job: Job

  var body: some View {
    HStack(alignment: .top, spacing: 10) {
      let (symbol, color) = jobStatusStyle(job.status)
      Group {
        if job.status == "running" {
          ProgressView().controlSize(.small)
        } else {
          Image(systemName: symbol).foregroundStyle(color)
        }
      }
      .frame(width: 22, alignment: .center)

      VStack(alignment: .leading, spacing: 3) {
        Text(jobLabel(job.type))
        Text(subtitle).font(.caption).foregroundStyle(.secondary)
        if !job.lastError.isEmpty {
          Text(job.lastError).font(.caption2).foregroundStyle(Theme.error).lineLimit(2)
        }
      }
    }
    .padding(.vertical, 2)
  }

  private var subtitle: String {
    var parts: [String] = [job.status]
    if let t = job.titleId, t > 0 {
      parts.append("title #\(t)")
    }
    if let s = job.sourceId, !s.isEmpty {
      parts.append("source \(s)")
    }
    if let c = job.catalogId, c > 0 {
      parts.append("catalog #\(c)")
    }
    if job.attempts > 0 {
      parts.append("\(job.attempts) attempt\(job.attempts == 1 ? "" : "s")")
    }
    let rt = relativeTime(job.updatedAt)
    if !rt.isEmpty {
      parts.append(rt)
    }
    return parts.joined(separator: " · ")
  }
}

struct RunSummary: Decodable {
  var done: Int
  var failed: Int
}

/// The global job types the server lets an admin start with no title context.
private let startableJobs: [(String, String)] = [
  ("refresh_title", "Refresh chapters"),
  ("scan_downloads", "Scan downloads"),
  ("download_missing", "Download missing"),
  ("catalog_refresh", "Catalog refresh"),
  ("sync_trackers", "Tracker sync"),
  ("backup_user_data", "Back up user data"),
]

func jobLabel(_ type: String) -> String {
  switch type {
  case "refresh_title": "Refresh chapters"
  case "scan_downloads": "Scan downloads"
  case "download_missing": "Download missing"
  case "sync_trackers": "Tracker sync"
  case "catalog_refresh": "Catalog refresh"
  case "attach_volumes": "Attach volumes"
  case "verify_source": "Verify source"
  case "match_sources": "Match sources"
  case "backup_user_data": "Back up user data"
  default: type
  }
}

func jobStatusStyle(_ status: String) -> (String, Color) {
  switch status {
  case "done": ("checkmark.circle.fill", Theme.success)
  case "running": ("arrow.triangle.2.circlepath", Theme.primary)
  case "queued": ("clock", Theme.mutedText)
  case "failed": ("exclamationmark.triangle.fill", Theme.warning)
  case "dead": ("xmark.octagon.fill", Theme.error)
  case "cancelled": ("minus.circle", Theme.mutedText)
  default: ("circle", Theme.mutedText)
  }
}

@MainActor private let isoPlain = ISO8601DateFormatter()
@MainActor private let isoFractional: ISO8601DateFormatter = {
  let f = ISO8601DateFormatter()
  f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
  return f
}()

@MainActor private let relative = RelativeDateTimeFormatter()

@MainActor func relativeTime(_ s: String) -> String {
  guard let d = isoFractional.date(from: s) ?? isoPlain.date(from: s) else { return "" }
  return relative.localizedString(for: d, relativeTo: Date())
}

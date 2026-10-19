import Foundation

// Wire DTOs for /api/v1 (snake_case mapped via .convertFromSnakeCase).
// Timestamps stay Strings: the server emits RFC3339 with optional fractional
// seconds, and the app only displays them.

struct Meta: Decodable {
  var serverVersion: String
  var apiVersion: Int
  var authRequired: Bool
  var features: [String]
  var instanceId: String?
}

struct Me: Decodable {
  struct User: Decodable {
    var id: Int64
    var username: String
    var role: String
    var allowAdult: Bool
  }

  var user: User
  var permissions: [String]

  func can(_ perm: String) -> Bool {
    permissions.contains(perm)
  }
}

struct LoginResponse: Decodable {
  var token: String
  var me: Me
}

/// Job mirrors the server's jobDTO. Timestamps stay Strings (see file header).
struct Job: Decodable, Identifiable, Hashable {
  var id: Int64
  var type: String
  var status: String
  var attempts: Int
  var lastError: String
  var parentId: Int64
  var runAfter: String
  var createdAt: String
  var updatedAt: String
  var titleId: Int64?
  var sourceId: String?
  var catalogId: Int64?

  var active: Bool {
    status == "queued" || status == "running"
  }
}

struct JobList: Decodable {
  var items: [Job]
}

struct Title: Decodable, Identifiable, Hashable {
  var id: Int64
  var sourceUrl: String
  var displayTitle: String
  var coverImage: String
  var monitored: Bool
  var refreshInterval: String
  var isAdult: Bool
  var averageScore: Int64
  var contentTags: [String]
  var favourite: Bool
  var releaseStatus: String
  var discoveredCount: Int64
  var missingCount: Int64
  var failedCount: Int64
  var readCount: Int64
  var completedCount: Int64
  var sizeBytes: Int64
  var volumeCount: Int64
  var volumeReadCount: Int64
  var volumeBytes: Int64
  var updatedAt: String

  /// linked mirrors the web's `linked` helper: a real source page is
  /// attached (imported-from-disk titles have a placeholder URL).
  var linked: Bool {
    sourceUrl.hasPrefix("http")
  }
}

/// MangaDetail is the catalog metadata block the web title page shows
/// (badges, description, authors, genres). Codable: it's also snapshotted
/// into the offline index.
struct MangaDetail: Codable, Hashable, Sendable {
  var description: String?
  var status: String?
  var format: String?
  var year: Int?
  var chapters: Int?
  var volumes: Int?
  var authors: [String]?
  var genres: [String]?
  var averageScore: Int?
}

struct TitlePage: Decodable {
  var items: [Title]
  var nextCursor: String
  var total: Int
}

struct ChapterProgress: Decodable, Identifiable, Hashable {
  var id: Int64
  var titleId: Int64
  var label: String
  var title: String
  var displayName: String?
  var displayVolume: String?
  var numberMain: Int
  var downloaded: Bool
  var bytes: Int64
  var pages: Int
  var totalPages: Int
  var readPages: Int
  var completed: Bool
  var manual: Bool
  var firstUnreadPage: Int
  var lastReadAt: String?
}

struct TitleReadProgress: Decodable {
  var title: Title
  var manga: MangaDetail?
  var chapters: [ChapterProgress]
  var readChapters: Int
  var totalChapters: Int
  var readPages: Int64
  var nextChapterId: Int64
  var nextPage: Int
}

struct AniListStatus: Decodable {
  var connected: Bool
}

struct TitleActivity: Decodable {
  var active: String
  var queued: [String]
  var failed: Bool
  var error: String?
  var running: [String: Bool]?

  var busy: Bool {
    !active.isEmpty || !queued.isEmpty
  }

  func isActive(_ jobType: String, verb: String) -> Bool {
    if let running {
      return running[jobType] == true
    }
    return active == verb || queued.contains(verb) // old-server fallback
  }
}

struct CollectionEntry: Decodable, Hashable {
  var id: Int64?
  var key: String?
  var name: String
  var titleIds: [Int64]
  var pinnedIds: [Int64]?
  var chapters: Int64
  var volumes: Int64
  var pages: Int64
  var sizeBytes: Int64
  var readPct: Int64

  var uid: String {
    key ?? id.map { "c\($0)" } ?? name
  }
}

struct CollectionGroups: Decodable {
  var author: [CollectionEntry]
  var smart: [CollectionEntry]
  var custom: [CollectionEntry]
}

struct Manifest: Decodable {
  struct Chapter: Decodable, Identifiable {
    struct Page: Decodable {
      var page: Int
      var url: String
      var read: Bool
      var crop: [Double]?
    }

    var id: Int64
    var label: String
    var pageCount: Int
    var readPages: Int
    var completed: Bool
    var pages: [Page]
  }

  var titleId: Int64
  var title: String
  var resumeChapterId: Int64
  var resumePage: Int
  var markBase: String
  var extendBase: String
  var chapters: [Chapter]
}

struct APIErrorBody: Decodable {
  var error: String
  var code: String?
}

struct UserSettings: Codable {
  var readerMode: String?
  var readerDir: String?
  var readerPageLayout: String? // single / double / auto
  var readerSplitWide: Bool?
  var readerImageQuality: String? // standard / enhanced
}

struct Manga: Decodable, Identifiable, Hashable {
  var catalogId: Int64
  var providerId: String
  var titleRomaji: String
  var titleEnglish: String
  var description: String?
  var coverImage: String
  var status: String?
  var format: String?
  var chapters: Int?
  var averageScore: Int?
  var isAdult: Bool
  var titleId: Int64?

  /// Browse results aren't DB-cached (catalog id 0), so identity is the
  /// stable AniList provider id.
  var id: String {
    providerId
  }

  var name: String {
    titleEnglish.isEmpty ? titleRomaji : titleEnglish
  }

  enum CodingKeys: String, CodingKey {
    case catalogId = "id"
    case providerId, titleRomaji, titleEnglish, description, coverImage
    case status, format, chapters, averageScore, isAdult, titleId
  }

  /// caption mirrors the web card: FORMAT · STATUS · N ch · ★ N%
  var caption: String {
    var parts: [String] = []
    if let format, !format.isEmpty {
      parts.append(format)
    }
    if let status, !status.isEmpty {
      parts.append(status)
    }
    if let chapters, chapters > 0 {
      parts.append("\(chapters) ch")
    }
    if let averageScore, averageScore > 0 {
      parts.append("★ \(averageScore)%")
    }
    return parts.joined(separator: " · ")
  }
}

struct SearchPage: Decodable {
  var items: [Manga]
  var hasMore: Bool?
  var page: Int?
}

struct TagOption: Decodable, Identifiable, Hashable {
  var name: String
  var kind: String
  var id: String {
    name
  }
}

struct Match: Decodable, Identifiable, Hashable {
  var id: Int64
  var sourceId: String
  var sourceUrl: String
  var title: String
  var confidence: Double
  var chaptersFound: Int
  var error: String?
  var verifiedAt: String?
}

struct MatchList: Decodable {
  var items: [Match]
}

// Source linking (GET /api/v1/library/{id}/sources) DTOs.

struct LinkedSource: Decodable, Identifiable {
  var sourceId: String
  var name: String
  var url: String
  var active: Bool
  var id: String {
    url
  }
}

struct SourcePick: Decodable, Identifiable, Hashable {
  var id: String
  var name: String
  var enabled: Bool
}

struct TitleSources: Decodable {
  var linked: [LinkedSource]
  var matches: [Match]
  var finding: Bool
  var failed: Bool
  var error: String?
  var sources: [SourcePick]
}

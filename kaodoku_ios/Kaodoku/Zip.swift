import Compression
import Foundation

/// ZipArchive is a minimal CBZ reader: central directory parsing plus stored
/// and deflated entries — everything Go's archive/zip produces. No zip64.
struct ZipArchive: Sendable {
  enum ArchiveError: LocalizedError, Equatable {
    case invalid, encrypted, unsupported, tooLarge, zip64

    var errorDescription: String? {
      switch self {
      case .invalid: "Invalid or empty CBZ archive"
      case .encrypted: "Encrypted CBZ archives are not supported"
      case .unsupported: "Unsupported CBZ compression"
      case .tooLarge: "CBZ archive exceeds safe size limits"
      case .zip64: "ZIP64 CBZ archives are not supported"
      }
    }
  }

  struct Entry: Sendable {
    let name: String
    let method: Int
    let compressedSize: Int
    let uncompressedSize: Int
    let headerOffset: Int
  }

  let url: URL
  let entries: [Entry]
  let imageEntries: [Entry]
  private let fileSize: UInt64

  private static let imageExts: Set<String> = ["jpg", "jpeg", "png", "webp", "gif", "avif"]
  private static let maxEntries = 10000
  private static let maxDirectorySize = 32 << 20
  private static let maxEntrySize = 128 << 20
  private static let maxTotalSize = 4 << 30

  init(url: URL) throws {
    guard let fh = try? FileHandle(forReadingFrom: url),
          let size = try? fh.seekToEnd(), size > 22 else { throw ArchiveError.invalid }
    defer { try? fh.close() }

    let tailLen = Int(min(size, 66000))
    try? fh.seek(toOffset: size - UInt64(tailLen))
    guard let tail = try? fh.read(upToCount: tailLen), tail.count == tailLen else {
      throw ArchiveError.invalid
    }
    var eocd = -1
    var i = tail.count - 22
    while i >= 0 {
      if tail[tail.startIndex + i] == 0x50, tail[tail.startIndex + i + 1] == 0x4B,
         tail[tail.startIndex + i + 2] == 0x05, tail[tail.startIndex + i + 3] == 0x06
      {
        eocd = i
        break
      }
      i -= 1
    }
    guard eocd >= 0,
          eocd + 22 + Self.u16(tail, eocd + 20) == tail.count
    else {
      throw ArchiveError.invalid
    }

    let count = Self.u16(tail, eocd + 10)
    let cdSize = Self.u32(tail, eocd + 12)
    let cdOffset = Self.u32(tail, eocd + 16)
    guard count != 0xFFFF, cdSize != 0xFFFF_FFFF, cdOffset != 0xFFFF_FFFF else {
      throw ArchiveError.zip64
    }
    guard Self.u16(tail, eocd + 4) == 0, Self.u16(tail, eocd + 6) == 0,
          Self.u16(tail, eocd + 8) == count,
          count > 0, count <= Self.maxEntries, cdSize <= Self.maxDirectorySize
    else {
      throw ArchiveError.invalid
    }
    let directoryEnd = UInt64(cdOffset) + UInt64(cdSize)
    let eocdOffset = size - UInt64(tail.count) + UInt64(eocd)
    guard directoryEnd <= eocdOffset else { throw ArchiveError.invalid }
    try? fh.seek(toOffset: UInt64(cdOffset))
    guard let cd = try? fh.read(upToCount: cdSize), cd.count == cdSize else {
      throw ArchiveError.invalid
    }

    var out: [Entry] = []
    out.reserveCapacity(count)
    var p = 0
    var totalSize = 0
    for _ in 0 ..< count {
      guard p <= cd.count - 46, Self.u32(cd, p) == 0x0201_4B50 else {
        throw ArchiveError.invalid
      }
      let nlen = Self.u16(cd, p + 28)
      let xlen = Self.u16(cd, p + 30)
      let clen = Self.u16(cd, p + 32)
      let recordSize = 46 + nlen + xlen + clen
      guard recordSize <= cd.count - p else { throw ArchiveError.invalid }
      let flags = Self.u16(cd, p + 8)
      guard flags & 0x41 == 0 else { throw ArchiveError.encrypted }
      let method = Self.u16(cd, p + 10)
      guard method == 0 || method == 8 else { throw ArchiveError.unsupported }
      let compressedSize = Self.u32(cd, p + 20)
      let uncompressedSize = Self.u32(cd, p + 24)
      let headerOffset = Self.u32(cd, p + 42)
      guard compressedSize != 0xFFFF_FFFF, uncompressedSize != 0xFFFF_FFFF,
            headerOffset != 0xFFFF_FFFF else { throw ArchiveError.zip64 }
      guard compressedSize <= Self.maxEntrySize, uncompressedSize <= Self.maxEntrySize,
            totalSize <= Self.maxTotalSize - uncompressedSize
      else {
        throw ArchiveError.tooLarge
      }
      guard UInt64(headerOffset) + 30 <= size else { throw ArchiveError.invalid }
      totalSize += uncompressedSize
      let name = String(data: cd.subdata(in: cd.startIndex + p + 46 ..< cd.startIndex + p + 46 + nlen), encoding: .utf8) ?? ""
      out.append(Entry(
        name: name,
        method: method,
        compressedSize: compressedSize,
        uncompressedSize: uncompressedSize,
        headerOffset: headerOffset
      ))
      p += recordSize
    }
    self.url = url
    entries = out
    imageEntries = out
      .filter { Self.imageExts.contains(($0.name as NSString).pathExtension.lowercased()) }
      .sorted { $0.name.localizedStandardCompare($1.name) == .orderedAscending }
    fileSize = size
  }

  /// data extracts one entry (method 0 = stored, 8 = deflate).
  func data(for entry: Entry) -> Data? {
    guard entry.compressedSize <= Self.maxEntrySize,
          entry.uncompressedSize <= Self.maxEntrySize,
          UInt64(entry.headerOffset) + 30 <= fileSize else { return nil }
    guard let fh = try? FileHandle(forReadingFrom: url) else { return nil }
    defer { try? fh.close() }
    try? fh.seek(toOffset: UInt64(entry.headerOffset))
    guard let head = try? fh.read(upToCount: 30), head.count == 30,
          Self.u32(head, 0) == 0x0403_4B50,
          Self.u16(head, 6) & 0x41 == 0,
          Self.u16(head, 8) == entry.method else { return nil }
    let nlen = Self.u16(head, 26)
    let xlen = Self.u16(head, 28)
    let dataOffset = UInt64(entry.headerOffset) + UInt64(30 + nlen + xlen)
    guard dataOffset <= fileSize,
          UInt64(entry.compressedSize) <= fileSize - dataOffset else { return nil }
    try? fh.seek(toOffset: dataOffset)
    guard let raw = try? fh.read(upToCount: entry.compressedSize),
          raw.count == entry.compressedSize else { return nil }
    switch entry.method {
    case 0: return raw.count == entry.uncompressedSize ? raw : nil
    case 8: return Self.inflate(raw, to: entry.uncompressedSize)
    default: return nil
    }
  }

  private static func u16(_ d: Data, _ o: Int) -> Int {
    Int(d[d.startIndex + o]) | Int(d[d.startIndex + o + 1]) << 8
  }

  private static func u32(_ d: Data, _ o: Int) -> Int {
    u16(d, o) | u16(d, o + 2) << 16
  }

  private static func inflate(_ data: Data, to size: Int) -> Data? {
    guard size > 0, size <= maxEntrySize else { return size == 0 ? Data() : nil }
    var dst = Data(count: size)
    let written = dst.withUnsafeMutableBytes { db in
      data.withUnsafeBytes { sb in
        compression_decode_buffer(
          db.bindMemory(to: UInt8.self).baseAddress!, size,
          sb.bindMemory(to: UInt8.self).baseAddress!, data.count,
          nil, COMPRESSION_ZLIB
        )
      }
    }
    return written == size ? dst : nil
  }
}

import Foundation
@testable import Kaodoku
import Testing

@MainActor
@Test("Switching servers keeps each server's unsynced marks")
func switchKeepsMarks() async throws {
  let a = "switch-a-\(UUID().uuidString)"
  let b = "switch-b-\(UUID().uuidString)"
  defer {
    for id in [a, b] {
      try? FileManager.default.removeItem(at: LocalStore.root.appendingPathComponent(id))
    }
  }

  let store = LocalStore()
  await store.load(instance: a)
  store.recordMark(id: 1, volume: false, page: 2, totalPages: 5)
  let unreachable = try APIClient(baseURL: #require(URL(string: "http://127.0.0.1:1")), token: "t")
  async let failing: Void = store.flush(unreachable)
  await store.activate(b)
  await failing

  #expect(store.instance == b)
  #expect(store.pendingMarks == 0)
  store.recordMark(id: 9, volume: false, page: 1, totalPages: 3)

  await store.activate(a)
  #expect(store.queuedMarks.map(\.chapterId) == [1])
  await store.activate(b)
  #expect(store.queuedMarks.map(\.chapterId) == [9])
}

@testable import Kaodoku
import Testing

@Test("Five half stars cover the 1–10 rating")
func ratingStars() {
  #expect((1 ... 5).map { RatingRow.symbol($0, rating: 7) } == ["star.fill", "star.fill", "star.fill", "star.leadinghalf.filled", "star"])
  #expect((1 ... 5).allSatisfy { RatingRow.symbol($0, rating: 0) == "star" })
}

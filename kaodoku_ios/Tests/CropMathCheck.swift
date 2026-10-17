@testable import Kaodoku
import Testing
import UIKit

private func blank(_ w: Int, _ h: Int) -> UIImage {
  UIGraphicsImageRenderer(size: CGSize(width: w, height: h),
                          format: {
                            let f = UIGraphicsImageRendererFormat()
                            f.scale = 1
                            return f
                          }()).image { ctx in
    UIColor.white.setFill()
    ctx.fill(CGRect(x: 0, y: 0, width: w, height: h))
  }
}

@Test("Trim strength and margin scale the tight box as expected")
func cropStrengthAndMargin() throws {
  let img = blank(1000, 1000)
  let box: [Double] = [0.2, 0.2, 0.6, 0.6] // 20% inset on every edge

  // Full strength, no margin: exactly the tight box.
  let full = try #require(img.cropped(normalized: box))
  #expect(abs(full.size.width - 600) <= 1 && abs(full.size.height - 600) <= 1)

  // Half strength: only half of each border removed.
  let half = try #require(img.cropped(normalized: box, strength: 0.5))
  #expect(abs(half.size.width - 800) <= 1 && abs(half.size.height - 800) <= 1)

  // Margin adds breathing room back onto the cut.
  let padded = try #require(img.cropped(normalized: box, strength: 1, margin: 0.05))
  #expect(abs(padded.size.width - 700) <= 1 && abs(padded.size.height - 700) <= 1)

  // Zero strength = no-op: callers keep the original.
  #expect(img.cropped(normalized: box, strength: 0) == nil)

  // Margin can never push past the page edge.
  let clamped = try #require(img.cropped(normalized: [0.01, 0.01, 0.98, 0.98], margin: 0.005))
  #expect(clamped.size.width <= 1000 && clamped.size.height <= 1000)
}

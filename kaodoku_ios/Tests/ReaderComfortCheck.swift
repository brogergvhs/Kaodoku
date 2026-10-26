import CoreGraphics
@testable import Kaodoku
import Testing
import UIKit

private func makeImage(_ pixels: [(Int, Int, Int)]) -> UIImage {
  let ctx = CGContext(data: nil, width: pixels.count, height: 1, bitsPerComponent: 8,
                      bytesPerRow: 0, space: CGColorSpaceCreateDeviceRGB(),
                      bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)!
  for (x, p) in pixels.enumerated() {
    ctx.setFillColor(red: CGFloat(p.0) / 255, green: CGFloat(p.1) / 255,
                     blue: CGFloat(p.2) / 255, alpha: 1)
    ctx.fill(CGRect(x: x, y: 0, width: 1, height: 1))
  }
  return UIImage(cgImage: ctx.makeImage()!)
}

private func sample(_ image: UIImage, at x: Int) -> (r: Int, g: Int, b: Int) {
  let cg = image.cgImage!
  let w = cg.width
  var data = [UInt8](repeating: 0, count: w * 4)
  let ctx = CGContext(data: &data, width: w, height: 1, bitsPerComponent: 8,
                      bytesPerRow: w * 4, space: CGColorSpaceCreateDeviceRGB(),
                      bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)!
  ctx.draw(cg, in: CGRect(x: 0, y: 0, width: w, height: 1))
  let i = x * 4
  return (Int(data[i]), Int(data[i + 1]), Int(data[i + 2]))
}

@Test("White-only warmth tints white but leaves saturated colors alone")
func whiteOnlyPreservesColor() {
  let img = makeImage([(255, 255, 255), (0, 0, 255)]) // white, blue
  let out = ReaderComfort.apply(img, ComfortParams(warmth: 1, dim: 0, whiteOnly: true))

  let white = sample(out, at: 0)
  let blue = sample(out, at: 1)
  // White warmed: red held, green/blue pulled down (amber), b < g.
  #expect(white.r > 240)
  #expect(white.g < 240)
  #expect(white.b < white.g)
  // Blue is not white (min channel 0) -> untouched.
  #expect(blue.r < 15)
  #expect(blue.g < 15)
  #expect(blue.b > 235)
}

@Test("Without white-only, warmth reaches colored pixels too")
func fullWarmthAffectsColor() {
  let img = makeImage([(0, 0, 255)]) // blue
  let out = ReaderComfort.apply(img, ComfortParams(warmth: 1, dim: 0, whiteOnly: false))
  // Blue channel scaled down by warmth.
  #expect(sample(out, at: 0).b < 210)
}

@Test("Dim darkens a white page")
func dimDarkens() {
  let img = makeImage([(255, 255, 255)])
  let out = ReaderComfort.apply(img, ComfortParams(warmth: 0, dim: 1, whiteOnly: false))
  #expect(sample(out, at: 0).r < 100)
}

@Test("Identity params return the original image untouched")
func identityIsNoop() {
  let img = makeImage([(123, 45, 67)])
  let out = ReaderComfort.apply(img, ComfortParams(warmth: 0, dim: 0, whiteOnly: true))
  #expect(out === img)
}

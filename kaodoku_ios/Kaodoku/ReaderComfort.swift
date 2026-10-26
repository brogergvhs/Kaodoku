import CoreImage
import UIKit

/// ComfortParams are the reader's device-local eye-comfort settings: an amber
/// warmth and a dim, optionally confined to white/near-white pixels so colored
/// pages keep their hues.
struct ComfortParams: Equatable {
  var warmth: Double // 0...1, amber tint strength
  var dim: Double // 0...1, darkening
  var whiteOnly: Bool // confine the effect to white/near-white pixels

  var isIdentity: Bool {
    warmth <= 0 && dim <= 0
  }
}

/// ReaderComfort applies ComfortParams to a page image with Core Image.
enum ReaderComfort {
  private nonisolated(unsafe) static let context = CIContext(options: [
    .name: "readerComfort",
    .workingColorSpace: CGColorSpace(name: CGColorSpace.sRGB)!,
  ])

  static func apply(_ image: UIImage, _ p: ComfortParams) -> UIImage {
    guard !p.isIdentity, let cg = image.cgImage else { return image }
    let input = CIImage(cgImage: cg)

    let dimF = 1 - p.dim * 0.85
    let warmed = input.applyingFilter("CIColorMatrix", parameters: [
      "inputRVector": CIVector(x: dimF, y: 0, z: 0, w: 0),
      "inputGVector": CIVector(x: 0, y: (1 - 0.18 * p.warmth) * dimF, z: 0, w: 0),
      "inputBVector": CIVector(x: 0, y: 0, z: (1 - 0.60 * p.warmth) * dimF, w: 0),
      "inputAVector": CIVector(x: 0, y: 0, z: 0, w: 1),
    ])

    let result: CIImage
    if p.whiteOnly {
      let mask = input
        .applyingFilter("CIMinimumComponent")
        .applyingFilter("CIMaskToAlpha")
      result = warmed.applyingFilter("CIBlendWithMask", parameters: [
        "inputBackgroundImage": input,
        "inputMaskImage": mask,
      ])
    } else {
      result = warmed
    }

    guard let out = context.createCGImage(result, from: input.extent) else { return image }
    return UIImage(cgImage: out, scale: image.scale, orientation: image.imageOrientation)
  }
}

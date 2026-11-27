import AppKit
import CoreGraphics

private let space = CGColorSpace(name: CGColorSpace.sRGB)!

private func color(_ hex: UInt32, opacity: CGFloat = 1) -> CGColor {
    CGColor(srgbRed: CGFloat((hex >> 16) & 255) / 255, green: CGFloat((hex >> 8) & 255) / 255,
            blue: CGFloat(hex & 255) / 255, alpha: opacity)
}

private func polygon(_ points: [CGPoint]) -> CGPath {
    let path = CGMutablePath()
    path.addLines(between: points)
    path.closeSubpath()
    return path
}

private func point(_ x: CGFloat, _ y: CGFloat) -> CGPoint {
    CGPoint(x: x, y: 1024 - y)
}

private func paint(_ context: CGContext, _ shape: CGPath, _ from: CGColor, _ to: CGColor, start: CGPoint, end: CGPoint) {
    let gradient = CGGradient(colorsSpace: space, colors: [from, to] as CFArray, locations: nil)!
    context.saveGState()
    context.addPath(shape)
    context.clip()
    context.drawLinearGradient(gradient, start: start, end: end, options: [.drawsBeforeStartLocation, .drawsAfterEndLocation])
    context.restoreGState()
}

private func plate(_ context: CGContext, y: CGFloat, upper: UInt32, lower: UInt32, edge: UInt32) {
    let north = point(512, y)
    let east = point(750, y + 122)
    let south = point(512, y + 253)
    let west = point(274, y + 122)
    let tip = point(512, y + 287)
    let left = polygon([west, south, tip, point(274, y + 156)])
    let right = polygon([south, east, point(750, y + 156), tip])
    paint(context, left, color(edge), color(0x263554), start: west, end: tip)
    paint(context, right, color(edge), color(0x3C4B82), start: east, end: tip)
    let face = polygon([north, east, south, west])
    paint(context, face, color(upper), color(lower), start: point(345, y + 40), end: point(664, y + 224))
    context.addPath(face)
    context.setStrokeColor(color(0xDFE7FF, opacity: 0.45))
    context.setLineWidth(3)
    context.setLineJoin(.round)
    context.strokePath()
}

private func icon(_ size: Int) throws -> Data {
    guard let context = CGContext(data: nil, width: size, height: size, bitsPerComponent: 8, bytesPerRow: 0,
                                  space: space, bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue) else {
        throw CocoaError(.fileWriteUnknown)
    }
    let scale = CGFloat(size) / 1024
    context.scaleBy(x: scale, y: scale)
    let rect = CGRect(x: 91, y: 94, width: 842, height: 842)
    let frame = CGPath(roundedRect: rect, cornerWidth: 194, cornerHeight: 194, transform: nil)
    context.saveGState()
    context.setShadow(offset: CGSize(width: 0, height: -13), blur: 27, color: color(0x030611, opacity: 0.55))
    context.addPath(frame)
    context.setFillColor(color(0x151C2E))
    context.fillPath()
    context.restoreGState()
    paint(context, frame, color(0x3D4F83), color(0x101626), start: point(200, 95), end: point(825, 946))

    let glow = CGGradient(colorsSpace: space, colors: [color(0x829CFD, opacity: 0.46), color(0x829CFD, opacity: 0)] as CFArray, locations: nil)!
    context.saveGState()
    context.addPath(frame)
    context.clip()
    let center = point(528, 451)
    context.drawRadialGradient(glow, startCenter: center, startRadius: 45, endCenter: center, endRadius: 460,
                               options: [.drawsBeforeStartLocation])
    context.restoreGState()
    context.addPath(frame)
    context.setStrokeColor(color(0xAFC1FF, opacity: 0.5))
    context.setLineWidth(4)
    context.strokePath()

    plate(context, y: 452, upper: 0x6677BA, lower: 0x43578E, edge: 0x5970B8)
    plate(context, y: 365, upper: 0x93A7E8, lower: 0x718AD0, edge: 0x657DC8)
    plate(context, y: 278, upper: 0xE2E9FF, lower: 0x9BAFFF, edge: 0x8CA2F6)

    guard let image = context.makeImage(),
          let png = NSBitmapImageRep(cgImage: image).representation(using: .png, properties: [:]) else {
        throw CocoaError(.fileWriteUnknown)
    }
    return png
}

guard CommandLine.arguments.count == 2 else { fatalError("Pass an output .iconset directory") }
let directory = URL(fileURLWithPath: CommandLine.arguments[1], isDirectory: true)
try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
for size in [16, 32, 128, 256, 512] {
    try icon(size).write(to: directory.appendingPathComponent("icon_\(size)x\(size).png"))
    try icon(size * 2).write(to: directory.appendingPathComponent("icon_\(size)x\(size)@2x.png"))
}

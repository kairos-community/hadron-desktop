#!/usr/bin/env python3
"""Compare cua-driver's own desktop PNG against QEMU's actual scanout PPM.

This is the framebuffer half of the Task 6 visible-desktop gate. cua-driver
captures what IT believes the XLibre/i3 desktop looks like (cua-desktop.png,
extracted from the guest artifact disk); QEMU's QMP `screendump` captures what
the virtio-vga device is REALLY scanning out (qmp-desktop.ppm). If the pinned
driver is genuinely controlling the desktop shown through QEMU VNC, those two
images must agree.

Pure Python 3 standard library only: the host may not have numpy or PIL, so the
PNG and PPM are decoded here by hand (zlib for the IDAT stream, manual scanline
un-filtering, a small P6 parser).

Usage:
    frame_compare.py <cua-desktop.png> <qmp-desktop.ppm> <out frame-compare.json>

Writes frame-compare.json with cua_width/cua_height/qmp_width/qmp_height, the
mean absolute RGB error over a sampled grid, and a passed boolean. Exits 0 only
when passed; any mismatch (unequal dimensions or error >= threshold) exits 1 so
the harness can gate on it.
"""

import json
import sys
import zlib

# Sampling parameters (kept in lock-step with the task brief).
GRID = 16          # 16x16 grid of sample points.
BORDER = 12        # exclude a 12-pixel border on every side.
MAX_MEAN_ABS_ERROR = 24.0  # PASS only when mean absolute RGB error is below this.

PNG_SIGNATURE = b"\x89PNG\r\n\x1a\n"

# Bytes per pixel for each PNG colour type at 8-bit depth.
_CHANNELS_BY_COLOR_TYPE = {
    0: 1,  # grayscale
    2: 3,  # truecolour (RGB)
    3: 1,  # indexed (palette)
    4: 2,  # grayscale + alpha
    6: 4,  # truecolour + alpha (RGBA) -- what cua-driver's capture emits
}


def _paeth(a, b, c):
    """Paeth predictor used by PNG filter type 4."""
    p = a + b - c
    pa = abs(p - a)
    pb = abs(p - b)
    pc = abs(p - c)
    if pa <= pb and pa <= pc:
        return a
    if pb <= pc:
        return b
    return c


def decode_png(path):
    """Decode an 8-bit, non-interlaced PNG to (width, height, get_rgb).

    Handles colour types 0/2/3/4/6 at bit depth 8, which covers the RGBA output
    of cua-driver's screenshot path plus the common fallbacks. Interlaced, 16-bit
    or otherwise exotic PNGs raise ValueError rather than silently misdecoding.
    """
    with open(path, "rb") as fh:
        data = fh.read()

    if data[:8] != PNG_SIGNATURE:
        raise ValueError(f"{path}: not a PNG (bad signature)")

    off = 8
    width = height = bit_depth = color_type = interlace = None
    palette = None
    idat = bytearray()

    while off + 8 <= len(data):
        length = int.from_bytes(data[off:off + 4], "big")
        ctype = data[off + 4:off + 8]
        cstart = off + 8
        cdata = data[cstart:cstart + length]
        off = cstart + length + 4  # skip chunk data and its 4-byte CRC

        if ctype == b"IHDR":
            width = int.from_bytes(cdata[0:4], "big")
            height = int.from_bytes(cdata[4:8], "big")
            bit_depth = cdata[8]
            color_type = cdata[9]
            interlace = cdata[12]
        elif ctype == b"PLTE":
            palette = cdata
        elif ctype == b"IDAT":
            idat += cdata
        elif ctype == b"IEND":
            break

    if width is None:
        raise ValueError(f"{path}: no IHDR chunk")
    if bit_depth != 8:
        raise ValueError(f"{path}: unsupported bit depth {bit_depth} (only 8 handled)")
    if interlace != 0:
        raise ValueError(f"{path}: interlaced PNGs are not supported")
    channels = _CHANNELS_BY_COLOR_TYPE.get(color_type)
    if channels is None:
        raise ValueError(f"{path}: unsupported colour type {color_type}")
    if color_type == 3 and palette is None:
        raise ValueError(f"{path}: indexed PNG without a PLTE chunk")

    raw = zlib.decompress(bytes(idat))
    stride = width * channels
    expected = (stride + 1) * height
    if len(raw) < expected:
        raise ValueError(
            f"{path}: truncated pixel data ({len(raw)} < {expected} bytes)"
        )

    recon = bytearray(stride * height)
    prev = bytearray(stride)
    bpp = channels
    pos = 0
    for y in range(height):
        ftype = raw[pos]
        pos += 1
        line = bytearray(raw[pos:pos + stride])
        pos += stride

        if ftype == 0:
            pass
        elif ftype == 1:  # Sub
            for x in range(bpp, stride):
                line[x] = (line[x] + line[x - bpp]) & 0xFF
        elif ftype == 2:  # Up
            for x in range(stride):
                line[x] = (line[x] + prev[x]) & 0xFF
        elif ftype == 3:  # Average
            for x in range(stride):
                a = line[x - bpp] if x >= bpp else 0
                line[x] = (line[x] + ((a + prev[x]) >> 1)) & 0xFF
        elif ftype == 4:  # Paeth
            for x in range(stride):
                a = line[x - bpp] if x >= bpp else 0
                c = prev[x - bpp] if x >= bpp else 0
                line[x] = (line[x] + _paeth(a, prev[x], c)) & 0xFF
        else:
            raise ValueError(f"{path}: unknown scanline filter {ftype}")

        recon[y * stride:(y + 1) * stride] = line
        prev = line

    def get_rgb(x, y):
        base = y * stride + x * channels
        if color_type in (2, 6):
            return recon[base], recon[base + 1], recon[base + 2]
        if color_type in (0, 4):
            v = recon[base]
            return v, v, v
        # indexed
        idx = recon[base] * 3
        return palette[idx], palette[idx + 1], palette[idx + 2]

    return width, height, get_rgb


def decode_ppm(path):
    """Decode a binary (P6) PPM to (width, height, get_rgb).

    QEMU's `screendump` with a .ppm filename writes P6 with maxval 255.
    """
    with open(path, "rb") as fh:
        data = fh.read()

    if data[:2] != b"P6":
        raise ValueError(f"{path}: not a binary (P6) PPM")

    idx = 2

    def read_token(i):
        # Skip whitespace and '#' comment lines between header fields.
        while i < len(data):
            ch = data[i:i + 1]
            if ch.isspace():
                i += 1
            elif ch == b"#":
                while i < len(data) and data[i:i + 1] != b"\n":
                    i += 1
            else:
                break
        start = i
        while i < len(data) and not data[i:i + 1].isspace():
            i += 1
        return data[start:i], i

    tok, idx = read_token(idx)
    width = int(tok)
    tok, idx = read_token(idx)
    height = int(tok)
    tok, idx = read_token(idx)
    maxval = int(tok)
    if maxval != 255:
        raise ValueError(f"{path}: unsupported PPM maxval {maxval}")

    # Exactly one whitespace byte separates the header from the pixel data.
    idx += 1
    pix = data[idx:idx + width * height * 3]
    if len(pix) < width * height * 3:
        raise ValueError(f"{path}: truncated PPM pixel data")

    def get_rgb(x, y):
        base = (y * width + x) * 3
        return pix[base], pix[base + 1], pix[base + 2]

    return width, height, get_rgb


def sample_axis(length):
    """Return GRID evenly spaced coordinates inside the BORDER on one axis."""
    inner = length - 2 * BORDER
    if inner <= 0:
        raise ValueError(f"image dimension {length} too small for a {BORDER}px border")
    return [BORDER + int((k + 0.5) * inner / GRID) for k in range(GRID)]


def mean_abs_error(cua_rgb, qmp_rgb, width, height):
    """Mean absolute RGB error over the sampled grid, excluding the border."""
    xs = sample_axis(width)
    ys = sample_axis(height)
    total = 0
    count = 0
    for y in ys:
        for x in xs:
            cr, cg, cb = cua_rgb(x, y)
            qr, qg, qb = qmp_rgb(x, y)
            total += abs(cr - qr) + abs(cg - qg) + abs(cb - qb)
            count += 3
    return total / count


def main(argv):
    if len(argv) != 4:
        print(
            "usage: frame_compare.py <cua.png> <qmp.ppm> <out.json>",
            file=sys.stderr,
        )
        return 2

    cua_path, qmp_path, out_path = argv[1], argv[2], argv[3]

    try:
        cua_w, cua_h, cua_rgb = decode_png(cua_path)
        qmp_w, qmp_h, qmp_rgb = decode_ppm(qmp_path)
    except (OSError, ValueError, zlib.error) as exc:
        print(f"frame_compare: decode failed: {exc}", file=sys.stderr)
        return 1

    result = {
        "cua_width": cua_w,
        "cua_height": cua_h,
        "qmp_width": qmp_w,
        "qmp_height": qmp_h,
        "error": None,
        "passed": False,
    }

    if (cua_w, cua_h) != (qmp_w, qmp_h):
        print(
            f"frame_compare: dimension mismatch "
            f"cua={cua_w}x{cua_h} qmp={qmp_w}x{qmp_h}",
            file=sys.stderr,
        )
    else:
        try:
            error = mean_abs_error(cua_rgb, qmp_rgb, cua_w, cua_h)
        except (ValueError, IndexError) as exc:
            print(f"frame_compare: comparison failed: {exc}", file=sys.stderr)
            error = None
        if error is not None:
            result["error"] = round(error, 4)
            result["passed"] = error < MAX_MEAN_ABS_ERROR

    try:
        with open(out_path, "w", encoding="utf-8") as fh:
            json.dump(result, fh, indent=2)
            fh.write("\n")
    except OSError as exc:
        print(f"frame_compare: cannot write {out_path}: {exc}", file=sys.stderr)
        return 1

    if result["passed"]:
        print(
            f"frame_compare: PASS "
            f"({cua_w}x{cua_h}, mean abs error {result['error']} < {MAX_MEAN_ABS_ERROR})"
        )
        return 0

    print(
        f"frame_compare: FAIL "
        f"(error={result['error']}, threshold={MAX_MEAN_ABS_ERROR})",
        file=sys.stderr,
    )
    return 1


if __name__ == "__main__":
    sys.exit(main(sys.argv))

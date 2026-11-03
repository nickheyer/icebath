"""Writes pkg/vxl/glyphs.go, every encoder glyph rasterized from the geometry its Unicode name describes"""

import os
import re
import unicodedata

COLS, ROWS, SUB = 4, 8, 8
VERTICAL = {"UPPER": 0.0, "UPPER MIDDLE": 1 / 3, "MIDDLE": 0.5, "LOWER MIDDLE": 2 / 3, "LOWER": 1.0}
HORIZONTAL = {"LEFT": 0.0, "CENTRE": 0.5, "RIGHT": 1.0}
SHARES = {
    "ONE EIGHTH": 1 / 8, "ONE QUARTER": 1 / 4, "THREE EIGHTHS": 3 / 8, "HALF": 1 / 2,
    "FIVE EIGHTHS": 5 / 8, "THREE QUARTERS": 3 / 4, "SEVEN EIGHTHS": 7 / 8,
}
SIDE = "(UPPER|LOWER|LEFT|RIGHT)"
SHARE = "(" + "|".join(SHARES) + ")"
LIGHT, HEAVY, DIAGONAL = 1 / 8, 1 / 4, 1 / 6
CENTRE = (0.5, 0.5)
CORNERS = {"UPPER LEFT": (0.0, 0.0), "UPPER RIGHT": (1.0, 0.0), "LOWER LEFT": (0.0, 1.0), "LOWER RIGHT": (1.0, 1.0)}
EDGES = {"UPPER": ((0.0, 0.0), (1.0, 0.0)), "RIGHT": ((1.0, 0.0), (1.0, 1.0)), "LOWER": ((1.0, 1.0), (0.0, 1.0)), "LEFT": ((0.0, 1.0), (0.0, 0.0))}


# Axis aligned rectangle as a polygon
def rect(x0, y0, x1, y1):
    return [(x0, y0), (x1, y0), (x1, y1), (x0, y1)]


SQUARE = rect(0.0, 0.0, 1.0, 1.0)


# Rectangle covering a share of the cell against one side
def side(word, f):
    return {"UPPER": rect(0, 0, 1, f), "LOWER": rect(0, 1 - f, 1, 1), "LEFT": rect(0, 0, f, 1), "RIGHT": rect(1 - f, 0, 1, 1)}[word]


# Overlap of two axis aligned rectangles
def overlap(a, b):
    return rect(max(a[0][0], b[0][0]), max(a[0][1], b[0][1]), min(a[2][0], b[2][0]), min(a[2][1], b[2][1]))


# Signed parallelogram area from a to b and a to p, positive when p lies left of the line
def cross(a, b, p):
    return (b[0] - a[0]) * (p[1] - a[1]) - (b[1] - a[1]) * (p[0] - a[0])


# Polygon cut by the line through a and b, keeping the side where the reference point lies
def clip(poly, a, b, ref):
    s = cross(a, b, ref)
    if abs(s) < 1e-12:
        raise ValueError("reference point lies on the cutting line")
    out = []
    for i, p in enumerate(poly):
        q = poly[(i + 1) % len(poly)]
        fp, fq = cross(a, b, p) * s, cross(a, b, q) * s
        if fp >= 0:
            out.append(p)
        if (fp >= 0) != (fq >= 0):
            t = fp / (fp - fq)
            out.append((p[0] + (q[0] - p[0]) * t, p[1] + (q[1] - p[1]) * t))
    return out


# Border point named by a vertical word then a horizontal word
def point(words):
    for v in sorted(VERTICAL, key=len, reverse=True):
        if words.startswith(v + " ") and words[len(v) + 1:] in HORIZONTAL:
            return (HORIZONTAL[words[len(v) + 1:]], VERTICAL[v])
    raise ValueError("unknown border point " + words)


# Band of a width around the line through two points, kept inside the cell
def stroke(a, b, width):
    dx, dy = b[0] - a[0], b[1] - a[1]
    n = (dx * dx + dy * dy) ** 0.5
    ox, oy = -dy / n * width / 2, dx / n * width / 2
    poly = clip(SQUARE, (a[0] + ox, a[1] + oy), (b[0] + ox, b[1] + oy), (a[0] - ox, a[1] - oy))
    return clip(poly, (a[0] - ox, a[1] - oy), (b[0] - ox, b[1] - oy), (a[0] + ox, a[1] + oy))


# Arm of a box drawing line from the cell centre to one edge
def arm(word, t):
    lo, hi = 0.5 - t / 2, 0.5 + t / 2
    return {"UP": rect(lo, 0, hi, hi), "DOWN": rect(lo, lo, hi, 1), "LEFT": rect(0, lo, hi, hi), "RIGHT": rect(lo, lo, 1, hi)}[word]


# Filled polygons of a character in unit cell coordinates, parsed from its Unicode name
def shape(cp):
    name = unicodedata.name(chr(cp))
    if name == "SPACE":
        return []
    if name == "FULL BLOCK":
        return [SQUARE]
    if m := re.fullmatch(SIDE + " " + SHARE + " BLOCK", name):
        return [side(m[1], SHARES[m[2]])]
    if m := re.fullmatch(SIDE + " " + SHARE + " " + SIDE + " " + SHARE + " BLOCK", name):
        return [overlap(side(m[1], SHARES[m[2]]), side(m[3], SHARES[m[4]]))]
    if m := re.fullmatch("MIDDLE (LEFT|RIGHT) ONE QUARTER BLOCK", name):
        return [overlap(side(m[1], 0.5), rect(0, 0.25, 1, 0.75))]
    if m := re.fullmatch("QUADRANT (.+)", name):
        return [overlap(side(q.split()[0], 0.5), side(q.split()[1], 0.5)) for q in m[1].split(" AND ")]
    if m := re.fullmatch("BLOCK (SEXTANT|OCTANT)-([1-8]+)", name):
        down = 3 if m[1] == "SEXTANT" else 4
        return [rect((d - 1) % 2 / 2, (d - 1) // 2 / down, ((d - 1) % 2 + 1) / 2, ((d - 1) // 2 + 1) / down) for d in map(int, m[2])]
    if m := re.fullmatch("(VERTICAL|HORIZONTAL) ONE EIGHTH BLOCK-([1-8])", name):
        k = int(m[2])
        return [rect((k - 1) / 8, 0, k / 8, 1) if m[1] == "VERTICAL" else rect(0, (k - 1) / 8, 1, k / 8)]
    if m := re.fullmatch("((?:UPPER|LOWER) (?:LEFT|RIGHT)) BLOCK DIAGONAL (.+) TO (.+)", name):
        return [clip(SQUARE, point(m[2]), point(m[3]), CORNERS[m[1]])]
    if m := re.fullmatch("(.+) TRIANGULAR (ONE QUARTER|HALF|THREE QUARTERS) BLOCK", name):
        edges = m[1].split(" AND ")
        if len(edges) / 4 != SHARES[m[2]]:
            raise ValueError("triangle count disagrees with the share in " + name)
        return [[EDGES[e][0], EDGES[e][1], CENTRE] for e in edges]
    if m := re.fullmatch("BLACK ((?:UPPER|LOWER) (?:LEFT|RIGHT)) TRIANGLE", name):
        c = CORNERS[m[1]]
        return [[c, (1 - c[0], c[1]), (c[0], 1 - c[1])]]
    if m := re.fullmatch("BOX DRAWINGS LIGHT DIAGONAL (.+) TO (.+)", name):
        return [stroke(CORNERS[m[1]], CORNERS[m[2]], DIAGONAL)]
    if name == "BOX DRAWINGS LIGHT DIAGONAL CROSS":
        return [stroke(CORNERS["UPPER LEFT"], CORNERS["LOWER RIGHT"], DIAGONAL), stroke(CORNERS["UPPER RIGHT"], CORNERS["LOWER LEFT"], DIAGONAL)]
    if m := re.fullmatch("BOX DRAWINGS (LIGHT|HEAVY) (.+)", name):
        t = LIGHT if m[1] == "LIGHT" else HEAVY
        words = []
        for w in m[2].split(" AND "):
            words += {"VERTICAL": ["UP", "DOWN"], "HORIZONTAL": ["LEFT", "RIGHT"]}.get(w, [w])
        return [arm(w, t) for w in words]
    raise ValueError("no geometry for U+%04X %s" % (cp, name))


# Edge equations of a convex polygon, each positive on the inside
def edges(poly):
    area = sum(p[0] * q[1] - q[0] * p[1] for p, q in zip(poly, poly[1:] + poly[:1]))
    if abs(area) < 1e-12:
        return None
    s = 1 if area > 0 else -1
    return [(p, q, s) for p, q in zip(poly, poly[1:] + poly[:1]) if p != q]


# Subsamples of each pixel inside any polygon, out of SUB by SUB
def raster(polys):
    shapes = [e for e in map(edges, polys) if e]
    counts = []
    for py in range(ROWS):
        for px in range(COLS):
            n = 0
            for sy in range(SUB):
                y = (py + (sy + 0.5) / SUB) / ROWS
                for sx in range(SUB):
                    x = (px + (sx + 0.5) / SUB) / COLS
                    for es in shapes:
                        inside = True
                        for p, q, s in es:
                            c = cross(p, q, (x, y)) * s
                            if abs(c) < 1e-12:
                                raise ValueError("subsample lies on a polygon edge")
                            if c < 0:
                                inside = False
                                break
                        if inside:
                            n += 1
                            break
            counts.append(n)
    return tuple(counts)


# Characters whose geometry is a union of block cells, any of which may fill a grid library
BLOCKS = (
    [0x20] + [c for c in range(0x2580, 0x25A0) if "SHADE" not in unicodedata.name(chr(c))]
    + list(range(0x1FB00, 0x1FB3C)) + list(range(0x1CD00, 0x1CDE6)) + list(range(0x1CEA0, 0x1CEB0))
    + list(range(0x1FB82, 0x1FB8C)) + [0x1FBE6, 0x1FBE7]
)


# Glyph for every mask of a grid of block cells, matched by raster against every block character
def grid(across, down, rasters):
    by = {}
    for cp in BLOCKS:
        r = rasters(cp)
        if r in by:
            raise ValueError("U+%04X and U+%04X rasterize alike" % (by[r], cp))
        by[r] = cp
    out = []
    for mask in range(1 << (across * down)):
        cells = [rect(i % across / across, i // across / down, (i % across + 1) / across, (i // across + 1) / down) for i in range(across * down) if mask >> i & 1]
        r = raster(cells)
        if r not in by:
            raise ValueError("no character fills mask %d of the %d by %d grid" % (mask, across, down))
        out.append(by[r])
    return out


# Rasterizes every library, then writes the Go table
def main():
    cache = {}

    def rasters(cp):
        if cp not in cache:
            cache[cp] = raster(shape(cp))
        return cache[cp]

    half, quadrant, sextant, octant = grid(1, 2, rasters), grid(2, 2, rasters), grid(2, 3, rasters), grid(2, 4, rasters)
    shapes = (
        [0x20, 0x2588] + quadrant + list(range(0x258F, 0x2588, -1)) + list(range(0x2581, 0x2588)) + [0x2594, 0x2595]
        + list(range(0x25E2, 0x25E6)) + list(range(0x2571, 0x2574))
        + [0x2500, 0x2502, 0x250C, 0x2510, 0x2514, 0x2518, 0x251C, 0x2524, 0x252C, 0x2534, 0x253C, 0x2501, 0x2503]
    )
    legacy = shapes + sextant + list(range(0x1FB3C, 0x1FB7C)) + list(range(0x1FB82, 0x1FB8C)) + [0x1FB9A, 0x1FB9B, 0x1FBE6, 0x1FBE7]
    every = legacy + octant
    libs = [half, quadrant, sextant, octant, shapes, legacy, every]
    order = list(dict.fromkeys(every))
    lines = [
        "// Code generated by scripts/glyphs.py. DO NOT EDIT.",
        "",
        "package vxl",
        "",
        "// Every glyph with the libraries holding it, its lit pixel mask and the coverage of each pixel",
        "var glyphs = [...]glyph{",
    ]
    for cp in order:
        r = rasters(cp)
        bits = sum(1 << m for m, lib in enumerate(libs) if cp in lib)
        mask = sum(1 << i for i, n in enumerate(r) if n * 2 >= SUB * SUB)
        cover = ", ".join(str((n * 255 + SUB * SUB // 2) // (SUB * SUB)) for n in r)
        lines.append('\t{"%s", 0x%02x, 0x%08x, cover{%s}},' % (chr(cp), bits, mask, cover))
    lines.append("}")
    path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "pkg", "vxl", "glyphs.go")
    with open(path, "w", encoding="utf-8") as f:
        f.write("\n".join(lines) + "\n")


if __name__ == "__main__":
    main()

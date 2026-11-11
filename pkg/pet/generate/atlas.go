package generate

import (
	"fmt"
	"image"
	"math"
	"sort"

	"github.com/nickheyer/icebath/pkg/pet"
)

const (
	cellWidth     = pet.FrameW
	cellHeight    = pet.FrameH
	alphaFloor    = 16
	cellPad       = 10
	normalizePad  = 14
	sideLobeRatio = 0.18
)

// RowSpec is one atlas row, its state, row index and frame count
type RowSpec struct {
	State       string
	Row, Frames int
}

// Atlas rows in the petdex order, short rows leaving their tail transparent
var RowSpecs = []RowSpec{
	{"idle", 0, 6}, {"running-right", 1, 8}, {"running-left", 2, 8}, {"waving", 3, 4}, {"jumping", 4, 5},
	{"failed", 5, 8}, {"waiting", 6, 6}, {"running", 7, 6}, {"review", 8, 6},
}

// Atlas pixel size, eight cells wide and one row per state
var (
	AtlasWidth  = 8 * cellWidth
	AtlasHeight = len(RowSpecs) * cellHeight
)

var neighbours = [4][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}}

// Middle element of the sorted values
func median(values []int) int {
	ordered := append([]int{}, values...)
	sort.Ints(ordered)
	return ordered[len(ordered)/2]
}

// Median width and height of boxes, each at least one
func medianBoxSize(boxes []Box) (int, int) {
	ws, hs := make([]int, len(boxes)), make([]int, len(boxes))
	for i, b := range boxes {
		ws[i], hs[i] = b[2]-b[0], b[3]-b[1]
	}
	return max(1, median(ws)), max(1, median(hs))
}

// Four connected breadth first flood from the seeds over accepted pixels, returning the visited pixels
func flood(w, h int, visited []byte, seeds [][2]int, accept func(x, y int) bool) [][2]int {
	queue := append([][2]int{}, seeds...)
	var pixels [][2]int
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		pixels = append(pixels, p)
		for _, d := range neighbours {
			nx, ny := p[0]+d[0], p[1]+d[1]
			if nx >= 0 && nx < w && ny >= 0 && ny < h && visited[ny*w+nx] == 0 {
				visited[ny*w+nx] = 1
				if accept(nx, ny) {
					queue = append(queue, [2]int{nx, ny})
				}
			}
		}
	}
	return pixels
}

// Flood from every accepted border pixel, the edge connected region only
func borderFlood(w, h int, visited []byte, accept func(x, y int) bool) [][2]int {
	var border [][2]int
	for x := 0; x < w; x++ {
		border = append(border, [2]int{x, 0}, [2]int{x, h - 1})
	}
	for y := 0; y < h; y++ {
		border = append(border, [2]int{0, y}, [2]int{w - 1, y})
	}
	var seeds [][2]int
	for _, p := range border {
		if visited[p[1]*w+p[0]] == 0 && accept(p[0], p[1]) {
			visited[p[1]*w+p[0]] = 1
			seeds = append(seeds, p)
		}
	}
	return flood(w, h, visited, seeds, accept)
}

// Each four connected component of unvisited accepted pixels in scan order
func unvisitedComponents(w, h int, visited []byte, accept func(x, y int) bool) [][][2]int {
	var out [][][2]int
	for start := 0; start < w*h; start++ {
		if visited[start] != 0 {
			continue
		}
		visited[start] = 1
		x, y := start%w, start/w
		if accept(x, y) {
			out = append(out, flood(w, h, visited, [][2]int{{x, y}}, accept))
		}
	}
	return out
}

// Clears the masked pixels then erodes alpha by one pixel to drop the key blend ring
func removeMasked(rgba *image.NRGBA, mask []bool) *image.NRGBA {
	out := copyImage(rgba)
	w, h := size(out)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if mask[y*w+x] {
				o := pixOff(out, x, y)
				out.Pix[o], out.Pix[o+1], out.Pix[o+2], out.Pix[o+3] = 0, 0, 0, 0
			}
		}
	}
	erodeAlpha3(out)
	return out
}

// Image with its flat background keyed out from the border inward, a real alpha background only repaired
func RemoveBackground(img *image.NRGBA, chromaKey *[3]int, threshold float64) *image.NRGBA {
	rgba := copyImage(img)
	w, h := size(rgba)
	minAlpha, low := 255, 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			a := int(rgba.Pix[pixOff(rgba, x, y)+3])
			minAlpha = min(minAlpha, a)
			if a <= alphaFloor {
				low++
			}
		}
	}
	if minAlpha <= alphaFloor && float64(low) > float64(w*h)*0.05 {
		return repairInternalAlphaHoles(rgba)
	}
	px := func(x, y int) []uint8 { o := pixOff(rgba, x, y); return rgba.Pix[o : o+4] }
	var key [3]int
	if chromaKey != nil {
		key = *chromaKey
	} else {
		type cnt struct {
			k [3]int
			n int
		}
		var counts []cnt
		for _, c := range [][2]int{{0, 0}, {w - 1, 0}, {0, h - 1}, {w - 1, h - 1}} {
			p := px(c[0], c[1])
			if int(p[3]) <= alphaFloor {
				continue
			}
			k := [3]int{int(p[0]), int(p[1]), int(p[2])}
			found := false
			for i := range counts {
				if counts[i].k == k {
					counts[i].n++
					found = true
				}
			}
			if !found {
				counts = append(counts, cnt{k, 1})
			}
		}
		key = [3]int{0, 255, 0}
		best := 0
		for _, c := range counts {
			if c.n > best {
				key, best = c.k, c.n
			}
		}
	}
	if max(key[0], key[1], key[2])-min(key[0], key[1], key[2]) >= 120 {
		mask := make([]bool, w*h)
		absi := func(v int) int {
			if v < 0 {
				return -v
			}
			return v
		}
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				p := px(x, y)
				near := absi(int(p[0])-key[0]) <= 48 && absi(int(p[1])-key[1]) <= 48 && absi(int(p[2])-key[2]) <= 48
				mask[y*w+x] = near && int(p[3]) > alphaFloor
			}
		}
		return removeMasked(rgba, mask)
	}
	isBG := func(x, y int) bool {
		p := px(x, y)
		dr, dg, db := float64(int(p[0])-key[0]), float64(int(p[1])-key[1]), float64(int(p[2])-key[2])
		return int(p[3]) > alphaFloor && math.Sqrt(dr*dr+dg*dg+db*db) <= threshold
	}
	remove := make([]bool, w*h)
	for _, p := range borderFlood(w, h, make([]byte, w*h), isBG) {
		remove[p[1]*w+p[0]] = true
	}
	return removeMasked(rgba, remove)
}

// Fills transparent islands enclosed by opaque pixels with their average opaque neighbour colour
func repairInternalAlphaHoles(rgba *image.NRGBA) *image.NRGBA {
	w, h := size(rgba)
	visited := make([]byte, w*h)
	transparent := func(x, y int) bool { return int(rgba.Pix[pixOff(rgba, x, y)+3]) <= alphaFloor }
	borderFlood(w, h, visited, transparent)
	for _, hole := range unvisitedComponents(w, h, visited, transparent) {
		seen := make(map[[2]int]bool, len(hole))
		for _, p := range hole {
			seen[p] = true
		}
		var sums [3]int
		n := 0
		for _, p := range hole {
			for _, d := range neighbours {
				nx, ny := p[0]+d[0], p[1]+d[1]
				if nx < 0 || nx >= w || ny < 0 || ny >= h || seen[[2]int{nx, ny}] {
					continue
				}
				o := pixOff(rgba, nx, ny)
				if int(rgba.Pix[o+3]) <= alphaFloor {
					continue
				}
				sums[0], sums[1], sums[2] = sums[0]+int(rgba.Pix[o]), sums[1]+int(rgba.Pix[o+1]), sums[2]+int(rgba.Pix[o+2])
				n++
			}
		}
		color := [4]uint8{0, 0, 0, 255}
		if n > 0 {
			for i := 0; i < 3; i++ {
				color[i] = uint8(pyRound(float64(sums[i]) / float64(n)))
			}
		}
		for _, p := range hole {
			o := pixOff(rgba, p[0], p[1])
			copy(rgba.Pix[o:o+4], color[:])
		}
	}
	return rgba
}

// Crops to content, scales to fit a padded cell and centres it on transparent
func fitToCell(img *image.NRGBA) *image.NRGBA {
	img = dropSideBleed(img)
	bbox, ok := getbbox(img)
	if !ok {
		return blank(cellWidth, cellHeight)
	}
	sprite := crop(img, bbox)
	sw, sh := size(sprite)
	scale := math.Min(math.Min(float64(cellWidth-cellPad)/float64(sw), float64(cellHeight-cellPad)/float64(sh)), 1.0)
	if scale != 1.0 {
		sprite = resizeNearest(sprite, max(1, pyRound(float64(sw)*scale)), max(1, pyRound(float64(sh)*scale)))
		sw, sh = size(sprite)
	}
	return place(sprite, cellWidth, cellHeight, floorDiv(cellWidth-sw, 2), floorDiv(cellHeight-sh, 2))
}

// Python floor division of ints
func floorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// Column run with its alpha mass
type contentRun struct {
	left, right, mass int
}

// Removes small separated left and right lobes before fitting, wide poses surviving
func dropSideBleed(img *image.NRGBA) *image.NRGBA {
	w, h := size(img)
	runs := contentRuns(columnProfile(img), 2)
	if len(runs) < 2 {
		return img
	}
	maxMass := 0
	for _, r := range runs {
		maxMass = max(maxMass, r.mass)
	}
	var keep []contentRun
	for _, r := range runs {
		if float64(r.mass) >= float64(maxMass)*sideLobeRatio {
			keep = append(keep, r)
		}
	}
	if len(keep) == len(runs) {
		return img
	}
	out := copyImage(img)
	prev := 0
	for _, r := range append(keep, contentRun{w, w, 0}) {
		if r.left > prev {
			clearRegion(out, Box{prev, 0, r.left, h})
		}
		prev = r.right
	}
	return out
}

// Start and end of each run of consecutive indices at most four long
func thinGroups(indices []int) [][2]int {
	var out [][2]int
	for i := 0; i < len(indices); {
		j := i
		for j+1 < len(indices) && indices[j+1]-indices[j] == 1 {
			j++
		}
		if j-i+1 <= 4 {
			out = append(out, [2]int{indices[i], indices[j] + 1})
		}
		i = j + 1
	}
	return out
}

// Removes thin slot spanning guide, floor and divider lines
func eraseLongAxisLines(img *image.NRGBA) *image.NRGBA {
	out := copyImage(img)
	w, h := size(out)
	opaque := make([]bool, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			opaque[y*w+x] = int(out.Pix[pixOff(out, x, y)+3]) > alphaFloor
		}
	}
	var rows, cols []int
	for y := 0; y < h; y++ {
		n := 0
		for x := 0; x < w; x++ {
			if opaque[y*w+x] {
				n++
			}
		}
		if float64(n) >= float64(w)*0.85 {
			rows = append(rows, y)
		}
	}
	for x := 0; x < w; x++ {
		n := 0
		for y := 0; y < h; y++ {
			if opaque[y*w+x] {
				n++
			}
		}
		if float64(n) >= float64(h)*0.85 {
			cols = append(cols, x)
		}
	}
	for _, g := range thinGroups(rows) {
		clearRegion(out, Box{0, g[0], w, g[1]})
	}
	for _, g := range thinGroups(cols) {
		clearRegion(out, Box{g[0], 0, g[1], h})
	}
	return out
}

// Connected opaque component with its box and pixel mass
type component struct {
	box  Box
	mass int
}

// Connected opaque components as boxes with their mass
func componentBoxes(img *image.NRGBA) []component {
	bbox, ok := getbbox(img)
	if !ok {
		return nil
	}
	l0, t0 := bbox[0], bbox[1]
	w, h := bbox[2]-bbox[0], bbox[3]-bbox[1]
	accept := func(x, y int) bool { return int(img.Pix[pixOff(img, l0+x, t0+y)+3]) > alphaFloor }
	var out []component
	for _, comp := range unvisitedComponents(w, h, make([]byte, w*h), accept) {
		minX, minY, maxX, maxY := comp[0][0], comp[0][1], comp[0][0], comp[0][1]
		for _, p := range comp {
			minX, minY, maxX, maxY = min(minX, p[0]), min(minY, p[1]), max(maxX, p[0]), max(maxY, p[1])
		}
		out = append(out, component{Box{l0 + minX, t0 + minY, l0 + maxX + 1, t0 + maxY + 1}, len(comp)})
	}
	return out
}

// Keeps the slot's real subject and drops detached effects and noise
func isolateSlotSubject(img *image.NRGBA) *image.NRGBA {
	rgba := eraseLongAxisLines(img)
	comps := componentBoxes(rgba)
	if len(comps) == 0 {
		return rgba
	}
	main := comps[0]
	for _, c := range comps[1:] {
		if c.mass > main.mass {
			main = c
		}
	}
	ml, mr := main.box[0], main.box[2]
	mw := max(1, mr-ml)
	w, h := size(rgba)
	out := blank(w, h)
	for _, c := range comps {
		left, right := c.box[0], c.box[2]
		overlap := max(0, min(right, mr)-max(left, ml))
		mid := float64(left+right) / 2
		nearMain := float64(ml)-float64(mw)*0.25 <= mid && mid <= float64(mr)+float64(mw)*0.25
		if c.box == main.box || (float64(c.mass) >= math.Max(24, float64(main.mass)*0.035) && (float64(overlap) >= float64(mw)*0.3 || nearMain)) {
			alphaComposite(out, crop(rgba, c.box), c.box[0], c.box[1])
		}
	}
	return out
}

// Whether the box leaves empty room on all four edges, at least 4 and at most 12 or 16 pixels
func hasMargin(w, h int, b Box, fx, fy float64) bool {
	minX, minY := max(4, min(12, pyRound(float64(w)*fx))), max(4, min(16, pyRound(float64(h)*fy)))
	return b[0] >= minX && b[1] >= minY && w-b[2] >= minX && h-b[3] >= minY
}

// Groups component boxes into visual rows sorted left to right
func groupComponentRows(boxes []Box) [][]Box {
	if len(boxes) == 0 {
		return nil
	}
	cy := func(b Box) float64 { return float64(b[1]+b[3]) / 2 }
	_, medH := medianBoxSize(boxes)
	rowTol := math.Max(12, float64(medH)*0.55)
	sorted := append([]Box{}, boxes...)
	sort.SliceStable(sorted, func(i, j int) bool { return cy(sorted[i]) < cy(sorted[j]) })
	var rows [][]Box
	var centers []float64
	for _, box := range sorted {
		idx := -1
		for i, c := range centers {
			if math.Abs(cy(box)-c) <= rowTol {
				idx = i
				break
			}
		}
		if idx < 0 {
			rows = append(rows, []Box{box})
			centers = append(centers, cy(box))
			continue
		}
		rows[idx] = append(rows[idx], box)
		sum := 0.0
		for _, b := range rows[idx] {
			sum += cy(b)
		}
		centers[idx] = sum / float64(len(rows[idx]))
	}
	order := make([]int, len(rows))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return centers[order[i]] < centers[order[j]] })
	out := make([][]Box, len(rows))
	for i, o := range order {
		row := rows[o]
		sort.SliceStable(row, func(a, b int) bool { return float64(row[a][0]+row[a][2])/2 < float64(row[b][0]+row[b][2])/2 })
		out[i] = row
	}
	return out
}

// Merges disconnected parts of one subject on the same row, never bridging the gaps between poses
func mergeRelatedBoxes(boxes []Box) []Box {
	related := func(a, b Box) bool {
		vOverlap, minH := max(0, min(a[3], b[3])-max(a[1], b[1])), max(1, min(a[3]-a[1], b[3]-b[1]))
		gap, minW := max(0, max(a[0], b[0])-min(a[2], b[2])), max(1, min(a[2]-a[0], b[2]-b[0]))
		return float64(vOverlap) >= float64(minH)*0.45 && float64(gap) <= math.Max(14, float64(minW)*0.22)
	}
	boxes = append([]Box{}, boxes...)
	for changed := true; changed; {
		changed = false
		var merged []Box
		used := make([]bool, len(boxes))
		for i, a := range boxes {
			if used[i] {
				continue
			}
			used[i] = true
			for j := i + 1; j < len(boxes); j++ {
				if !used[j] && related(a, boxes[j]) {
					b := boxes[j]
					a = Box{min(a[0], b[0]), min(a[1], b[1]), max(a[2], b[2]), max(a[3], b[3])}
					used[j], changed = true, true
				}
			}
			merged = append(merged, a)
		}
		boxes = merged
	}
	return boxes
}

// Frames as connected subjects in reading order, nil when the frame count cannot be met
func componentCrops(strip *image.NRGBA, frameCount int, requirePadding bool) []*image.NRGBA {
	attempt := func(source *image.NRGBA) []*image.NRGBA {
		subjects := significantSubjectBoxes(source, 64)
		if len(subjects) < frameCount {
			return nil
		}
		rows := groupComponentRows(subjects)
		var ordered []Box
		for _, row := range rows {
			ordered = append(ordered, row...)
		}
		ordered = ordered[:frameCount]
		sw, sh := size(source)
		if requirePadding {
			for _, b := range ordered {
				if !hasMargin(sw, sh, b, 0.01, 0.015) {
					return nil
				}
			}
		}
		multirow := len(rows) > 1
		var frames []*image.NRGBA
		for _, b := range ordered {
			padX, padY := max(8, pyRound(float64(b[2]-b[0])*0.08)), max(8, pyRound(float64(b[3]-b[1])*0.08))
			cl, cr := 0, sw
			if frameCount != 1 {
				cl, cr = max(0, b[0]-padX), min(sw, b[2]+padX)
			}
			ct, cb := 0, sh
			if multirow {
				ct, cb = max(0, b[1]-padY), min(sh, b[3]+padY)
			}
			frames = append(frames, place(crop(source, b), cr-cl, cb-ct, b[0]-cl, b[1]-ct))
		}
		return frames
	}
	if frames := attempt(strip); frames != nil {
		return frames
	}
	return attempt(eraseLongAxisLines(strip))
}

// Strip with narrow transparent bands cut at the expected frame boundaries
func severExpectedGutters(strip *image.NRGBA, frameCount int) *image.NRGBA {
	if frameCount <= 1 {
		return strip
	}
	out := copyImage(strip)
	w, h := size(out)
	slot := float64(w) / float64(frameCount)
	half := max(3, min(18, pyRound(slot*0.06)))
	for i := 1; i < frameCount; i++ {
		x := pyRound(float64(i) * slot)
		for y := 0; y < h; y++ {
			for xx := max(0, x-half); xx < min(w, x+half+1); xx++ {
				out.Pix[pixOff(out, xx, y)+3] = 0
			}
		}
	}
	return out
}

// Slot subject isolated then trimmed of side bleed
func cleanSlot(img *image.NRGBA) *image.NRGBA { return dropSideBleed(isolateSlotSubject(img)) }

// Equal width independently cleaned columns, nil when padding is required and missing
func slotCrops(strip *image.NRGBA, frameCount int, requirePadding bool) []*image.NRGBA {
	w, h := size(strip)
	frames := make([]*image.NRGBA, frameCount)
	for i := 0; i < frameCount; i++ {
		left := pyRound(float64(i) * float64(w) / float64(frameCount))
		right := pyRound(float64(i+1) * float64(w) / float64(frameCount))
		frames[i] = cleanSlot(crop(strip, Box{left, 0, right, h}))
	}
	if requirePadding {
		for _, f := range frames {
			b, ok := getbbox(f)
			fw, fh := size(f)
			if !ok || !hasMargin(fw, fh, b, 0.025, 0.02) {
				return nil
			}
		}
	}
	return frames
}

// Column spans whose alpha exceeds the threshold with their mass
func contentRuns(profile []int, threshold int) []contentRun {
	var runs []contentRun
	start := -1
	for x := 0; x <= len(profile); x++ {
		v := 0
		if x < len(profile) {
			v = profile[x]
		}
		if v > threshold {
			if start < 0 {
				start = x
			}
		} else if start >= 0 {
			mass := 0
			for _, m := range profile[start:x] {
				mass += m
			}
			runs = append(runs, contentRun{start, x, mass})
			start = -1
		}
	}
	return runs
}

// Per frame column ranges from the row's gutters, merging across the smallest gaps, nil when too few
func frameXRanges(strip *image.NRGBA, frameCount int) [][2]int {
	runs := contentRuns(columnProfile(strip), 2)
	maxMass := 0
	for _, r := range runs {
		maxMass = max(maxMass, r.mass)
	}
	var groups [][2]int
	for _, r := range runs {
		if float64(r.mass) >= float64(maxMass)*0.02 {
			groups = append(groups, [2]int{r.left, r.right})
		}
	}
	if len(groups) < frameCount {
		return nil
	}
	for len(groups) > frameCount {
		gi := 0
		for i := 1; i < len(groups)-1; i++ {
			if groups[i+1][0]-groups[i][1] < groups[gi+1][0]-groups[gi][1] {
				gi = i
			}
		}
		groups[gi][1] = groups[gi+1][1]
		groups = append(groups[:gi+1], groups[gi+2:]...)
	}
	return groups
}

// Merged boxes of components carrying at least twelve percent of the largest mass
func significantSubjectBoxes(img *image.NRGBA, minMass int) []Box {
	comps := componentBoxes(img)
	if len(comps) == 0 {
		return nil
	}
	maxMass := 0
	for _, c := range comps {
		maxMass = max(maxMass, c.mass)
	}
	var boxes []Box
	for _, c := range comps {
		if float64(c.mass) >= math.Max(float64(minMass), float64(maxMass)*0.12) {
			boxes = append(boxes, c.box)
		}
	}
	return mergeRelatedBoxes(boxes)
}

// A frame several times wider than the median but not proportionally taller
func isMultiPoseOutlier(width, height, medW, medH int) bool {
	return float64(width) > math.Max(float64(medW)*3.0, float64(medW+96)) && float64(height) <= float64(medH)*1.6
}

// Rejects rows where one frame is really several poses
func validateExtractedFrames(frames []*image.NRGBA, frameCount int) error {
	if len(frames) != frameCount {
		return fmt.Errorf("ValueError: expected %d frames, got %d", frameCount, len(frames))
	}
	var boxes []Box
	for i, f := range frames {
		b, ok := getbbox(f)
		if !ok {
			return fmt.Errorf("ValueError: frame %d is empty", i)
		}
		if len(significantSubjectBoxes(f, 32)) >= 3 {
			return fmt.Errorf("ValueError: frame %d contains multiple separated subjects", i)
		}
		boxes = append(boxes, b)
	}
	if frameCount <= 1 {
		return nil
	}
	medW, medH := medianBoxSize(boxes)
	for i, b := range boxes {
		if isMultiPoseOutlier(b[2]-b[0], b[3]-b[1], medW, medH) {
			return fmt.Errorf("ValueError: frame %d is a multi-pose width outlier", i)
		}
	}
	return nil
}

// Turns one generated row strip into the frames, components first then slots, auto salvaging leniently
func ExtractStripFrames(path string, frameCount int, method string, fit bool) ([]*image.NRGBA, error) {
	loaded, err := loadRGBA(path)
	if err != nil {
		return nil, err
	}
	strip := RemoveBackground(loaded, nil, 90.0)
	frames := componentCrops(strip, frameCount, true)
	if frames == nil {
		frames = slotCrops(strip, frameCount, true)
	}
	if frames == nil {
		if method == "components" {
			return nil, fmt.Errorf("ValueError: could not segment %d padded sprites from strip", frameCount)
		}
		frames = componentCrops(strip, frameCount, false)
	}
	if frames == nil {
		frames = salvageFrames(strip, frameCount)
	}
	if err := validateExtractedFrames(frames, frameCount); err != nil {
		return nil, err
	}
	if !fit {
		return frames, nil
	}
	out := make([]*image.NRGBA, len(frames))
	for i, f := range frames {
		out[i] = fitToCell(f)
	}
	return out, nil
}

// Lenient last resort, gutter ranges with severed gutters if needed, else raw slots
func salvageFrames(strip *image.NRGBA, frameCount int) []*image.NRGBA {
	source, ranges := strip, frameXRanges(strip, frameCount)
	if ranges == nil {
		source = severExpectedGutters(strip, frameCount)
		ranges = frameXRanges(source, frameCount)
	}
	if ranges == nil {
		return slotCrops(source, frameCount, false)
	}
	w, h := size(source)
	pad := max(2, min(16, pyRound(float64(w)/float64(max(1, frameCount))*0.04)))
	out := make([]*image.NRGBA, len(ranges))
	for i, r := range ranges {
		out[i] = cleanSlot(crop(source, Box{max(0, r[0]-pad), 0, min(w, r[1]+pad), h}))
	}
	return out
}

// Integer shift that best aligns the profile onto the reference, ties to the smallest shift
func bestShift(ref, prof []int, window int) int {
	n := len(ref)
	best, bestScore := -window, int64(math.MinInt64)
	for d := -window; d <= window; d++ {
		var score int64
		for x := max(0, d); x < min(n, n+d); x++ {
			score += int64(ref[x]) * int64(prof[x-d])
		}
		if score > bestScore {
			best, bestScore = d, score
		}
	}
	return best
}

// StateFrames is one state's frames in insertion order
type StateFrames struct {
	State  string
	Frames []*image.NRGBA
}

// Registers every frame into a cell with one shared scale and baseline so the pet never slides or pulses
func NormalizeCells(framesByState []StateFrames, pad int) map[string][]*image.NRGBA {
	out := map[string][]*image.NRGBA{}
	type prep struct {
		state   string
		aligned []*image.NRGBA
		union   Box
		poseH   int
	}
	var prepared []prep
	targetW, targetH := cellWidth-pad, cellHeight-pad
	for _, sf := range framesByState {
		any := false
		for _, f := range sf.Frames {
			if _, ok := getbbox(f); ok {
				any = true
			}
		}
		if !any {
			blanks := make([]*image.NRGBA, len(sf.Frames))
			for i := range blanks {
				blanks[i] = blank(cellWidth, cellHeight)
			}
			out[sf.State] = blanks
			continue
		}
		w0, h0 := 0, 0
		for _, f := range sf.Frames {
			fw, fh := size(f)
			w0, h0 = max(w0, fw), max(h0, fh)
		}
		canvas := make([]*image.NRGBA, len(sf.Frames))
		profiles := make([][]int, len(sf.Frames))
		for i, f := range sf.Frames {
			if fw, fh := size(f); fw == w0 && fh == h0 {
				canvas[i] = f
			} else {
				canvas[i] = place(f, w0, h0, 0, 0)
			}
			profiles[i] = columnProfile(canvas[i])
		}
		ref := make([]int, w0)
		for x := 0; x < w0; x++ {
			col := make([]int, len(profiles))
			for i, p := range profiles {
				col[i] = p[x]
			}
			ref[x] = median(col)
		}
		window := max(8, w0/5)
		aligned := make([]*image.NRGBA, len(canvas))
		var boxes []Box
		var heights []int
		for i, f := range canvas {
			aligned[i] = place(f, w0+2*window, h0, window+bestShift(ref, profiles[i], window), 0)
			if b, ok := getbbox(aligned[i]); ok {
				boxes = append(boxes, b)
				heights = append(heights, b[3]-b[1])
			}
		}
		union := boxes[0]
		for _, b := range boxes[1:] {
			union = Box{min(union[0], b[0]), min(union[1], b[1]), max(union[2], b[2]), max(union[3], b[3])}
		}
		prepared = append(prepared, prep{sf.State, aligned, union, median(heights)})
	}
	if len(prepared) == 0 {
		return out
	}
	k := float64(targetH)
	for _, p := range prepared {
		k = math.Min(k, math.Min(float64(targetH)*float64(p.poseH)/float64(max(1, p.union[3]-p.union[1])),
			float64(targetW)*float64(p.poseH)/float64(max(1, p.union[2]-p.union[0]))))
	}
	for _, p := range prepared {
		scale := k / float64(max(1, p.poseH))
		sw := max(1, pyRound(float64(p.union[2]-p.union[0])*scale))
		sh := max(1, pyRound(float64(p.union[3]-p.union[1])*scale))
		ox, oy := pyRound(float64(cellWidth-sw)/2), (cellHeight-pad/2)-sh
		frames := make([]*image.NRGBA, len(p.aligned))
		for i, a := range p.aligned {
			c := crop(a, p.union)
			if cw, ch := size(c); cw != sw || ch != sh {
				c = resizeNearest(c, sw, sh)
			}
			frames[i] = place(c, cellWidth, cellHeight, ox, oy)
		}
		out[p.state] = frames
	}
	return out
}

// One frame from a standalone image, fit false giving the raw keyed sprite for NormalizeCells
func SingleFrame(path string, fit bool) (*image.NRGBA, error) {
	loaded, err := loadRGBA(path)
	if err != nil {
		return nil, err
	}
	keyed := RemoveBackground(loaded, nil, 90.0)
	if fit {
		return fitToCell(keyed), nil
	}
	return dropSideBleed(keyed), nil
}

// Zeroes the colour of fully transparent pixels so no halo residue remains
func ClearTransparentRGB(img *image.NRGBA) *image.NRGBA {
	out := copyImage(img)
	for i := 3; i < len(out.Pix); i += 4 {
		if out.Pix[i] == 0 {
			out.Pix[i-3], out.Pix[i-2], out.Pix[i-1] = 0, 0, 0
		}
	}
	return out
}

// Each frame flipped horizontally
func MirrorFrames(frames []*image.NRGBA) []*image.NRGBA {
	out := make([]*image.NRGBA, len(frames))
	for i, f := range frames {
		out[i] = flipLeftRight(f)
	}
	return out
}

// Packs per state frames into the atlas, short states leaving trailing cells transparent
func ComposeAtlas(framesByState map[string][]*image.NRGBA) *image.NRGBA {
	atlas := blank(AtlasWidth, AtlasHeight)
	for _, spec := range RowSpecs {
		frames := framesByState[spec.State]
		if len(frames) > spec.Frames {
			frames = frames[:spec.Frames]
		}
		for col, frame := range frames {
			cell := frame
			if w, h := size(frame); w != cellWidth || h != cellHeight {
				cell = fitToCell(frame)
			}
			alphaComposite(atlas, cell, col*cellWidth, spec.Row*cellHeight)
		}
	}
	return ClearTransparentRGB(atlas)
}

// AtlasValidation is the geometry, occupancy and transparency check of an atlas
type AtlasValidation struct {
	OK               bool
	Width, Height    int
	Errors, Warnings []string
	FilledStates     []string
}

// Checks atlas geometry, occupancy and transparency
func ValidateAtlas(atlas *image.NRGBA) AtlasValidation {
	w, h := size(atlas)
	v := AtlasValidation{Width: w, Height: h, Errors: []string{}, Warnings: []string{}, FilledStates: []string{}}
	if w == AtlasWidth && h == AtlasHeight {
		v.Errors, v.Warnings, v.FilledStates = checkAtlasCells(atlas)
	} else {
		v.Errors = []string{fmt.Sprintf("expected %dx%d, got %dx%d", AtlasWidth, AtlasHeight, w, h)}
	}
	v.OK = len(v.Errors) == 0
	return v
}

// Occupancy, collapse and residue checks of a correctly sized atlas
func checkAtlasCells(atlas *image.NRGBA) ([]string, []string, []string) {
	errs, warnings, filled := []string{}, []string{}, []string{}
	type stateBoxes struct {
		state string
		boxes []Box
	}
	var byState []stateBoxes
	for _, spec := range RowSpecs {
		any := false
		var boxes []Box
		for c := 0; c < spec.Frames; c++ {
			cell := crop(atlas, Box{c * cellWidth, spec.Row * cellHeight, (c + 1) * cellWidth, (spec.Row + 1) * cellHeight})
			if b, ok := getbbox(cell); ok {
				any = true
				boxes = append(boxes, b)
			}
		}
		if any {
			filled = append(filled, spec.State)
			byState = append(byState, stateBoxes{spec.State, boxes})
		} else {
			warnings = append(warnings, fmt.Sprintf("state '%s' has no frames", spec.State))
		}
	}
	if len(filled) == 0 {
		errs = append(errs, "atlas is empty — no state produced any frames")
	}
	var all []Box
	for _, sb := range byState {
		all = append(all, sb.boxes...)
	}
	globalW, globalH := 0, 0
	if len(all) > 0 {
		globalW, globalH = medianBoxSize(all)
		if globalH < max(56, pyRound(float64(cellHeight)*0.28)) {
			errs = append(errs, fmt.Sprintf("atlas sprites are too small after normalization (median frame height %dpx)", globalH))
		}
	}
	for _, sb := range byState {
		if len(sb.boxes) <= 1 {
			continue
		}
		medW, medH := medianBoxSize(sb.boxes)
		maxW, maxH := 0, 0
		for _, b := range sb.boxes {
			maxW, maxH = max(maxW, b[2]-b[0]), max(maxH, b[3]-b[1])
		}
		if isMultiPoseOutlier(maxW, maxH, medW, medH) {
			errs = append(errs, fmt.Sprintf("state '%s' contains a multi-pose frame outlier", sb.state))
		}
		collapsed := medW < max(32, pyRound(float64(globalW)*0.42)) || medH < max(40, pyRound(float64(globalH)*0.50))
		if globalW != 0 && globalH != 0 && collapsed {
			errs = append(errs, fmt.Sprintf("state '%s' appears collapsed (median %dx%dpx, global median %dx%dpx)", sb.state, medW, medH, globalW, globalH))
		}
	}
	residue := 0
	for i := 3; i < len(atlas.Pix); i += 4 {
		if atlas.Pix[i] == 0 && (atlas.Pix[i-3] != 0 || atlas.Pix[i-2] != 0 || atlas.Pix[i-1] != 0) {
			residue++
		}
	}
	if residue > 0 {
		errs = append(errs, fmt.Sprintf("%d transparent pixels retain RGB residue", residue))
	}
	return errs, warnings, filled
}

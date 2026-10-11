package util

import (
	"archive/zip"
	"encoding/json"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"

	_ "golang.org/x/image/webp"
)

// CropsEntryName is the CBZ entry carrying per-page content crops.
const CropsEntryName = "kaodoku-crops.json"

// FullPageCrop means "nothing to trim" — readers skip it.
var FullPageCrop = [4]float64{0, 0, 1, 1}

// cropsFile is the versioned JSON payload inside the archive.
type cropsFile struct {
	V     int          `json:"v"`
	Pages [][4]float64 `json:"pages"`
}

// PageCropsEntry computes content crops for the given page image files and
// returns them as a CreateCBZ extra entry, ordered the way readers order the
// archive (NaturalLess on the file name). Returns nil when no page has a
// trimmable border, so border-free archives carry no extra entry.
func PageCropsEntry(files []string) map[string][]byte {
	ordered := append([]string(nil), files...)
	sort.SliceStable(ordered, func(i, j int) bool { return NaturalLess(ordered[i], ordered[j]) })
	crops := make([][4]float64, len(ordered))
	trimmed := false
	for i, f := range ordered {
		crops[i] = FullPageCrop
		if c, ok := fileCrop(f); ok {
			crops[i] = c
			trimmed = true
		}
	}
	if !trimmed {
		return nil
	}
	body, err := json.Marshal(cropsFile{V: 1, Pages: crops})
	if err != nil {
		return nil
	}
	return map[string][]byte{CropsEntryName: body}
}

// ReadPageCrops extracts the crops entry from an opened archive.
func ReadPageCrops(files []*zip.File) [][4]float64 {
	for _, f := range files {
		if f.Name != CropsEntryName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil
		}
		body, err := io.ReadAll(io.LimitReader(rc, 1<<20))
		rc.Close()
		if err != nil {
			return nil
		}
		var cf cropsFile
		if json.Unmarshal(body, &cf) != nil || cf.V != 1 {
			return nil
		}
		return cf.Pages
	}
	return nil
}

func fileCrop(path string) ([4]float64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return FullPageCrop, false
	}
	defer f.Close()
	return readerCrop(f)
}

func readerCrop(r io.Reader) ([4]float64, bool) {
	img, _, err := image.Decode(r) // AVIF has no decoder: full page
	if err != nil {
		return FullPageCrop, false
	}
	c := ContentCrop(img)
	return c, c != FullPageCrop
}

// AddCropsToCBZ computes content crops for an existing archive's pages and
// rewrites it with the crops entry appended.
func AddCropsToCBZ(path string) (added bool, err error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return false, err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name == CropsEntryName {
			return false, nil
		}
	}
	images := CBZImageEntries(zr.File)
	if len(images) == 0 {
		return false, nil
	}
	crops := make([][4]float64, len(images))
	trimmed := false
	for i, f := range images {
		crops[i] = FullPageCrop
		rc, err := f.Open()
		if err != nil {
			continue
		}
		if c, ok := readerCrop(rc); ok {
			crops[i] = c
			trimmed = true
		}
		rc.Close()
	}
	if !trimmed {
		return false, nil
	}
	body, err := json.Marshal(cropsFile{V: 1, Pages: crops})
	if err != nil {
		return false, err
	}

	tmp := path + ".crops.tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return false, err
	}
	zw := zip.NewWriter(out)
	defer func() {
		if err != nil {
			zw.Close()
			out.Close()
			_ = os.Remove(tmp)
		}
	}()
	for _, f := range zr.File {
		if err = zw.Copy(f); err != nil {
			return false, err
		}
	}
	w, err := zw.Create(CropsEntryName)
	if err != nil {
		return false, err
	}
	if _, err = w.Write(body); err != nil {
		return false, err
	}
	if err = zw.Close(); err != nil {
		return false, err
	}
	if err = out.Sync(); err != nil {
		return false, err
	}
	if err = out.Close(); err != nil {
		return false, err
	}
	if err = os.Rename(tmp, path); err != nil {
		return false, err
	}
	if dir, derr := os.Open(filepath.Dir(path)); derr == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}

	return true, nil
}

// ContentCrop finds the page's content bounding box as a normalized
// [x, y, w, h]: scanning inward from each edge, a line counts as border while
// ~all of its samples match the background sampled from the outer pixel ring.
// At most 25% trimmed per edge and if the content box drops under 55% of the 
// page (spacers, credits, blank pages) the page is left untouched.
func ContentCrop(img image.Image) [4]float64 {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w < 32 || h < 32 {
		return FullPageCrop
	}
	step := max(1, max(w, h)/400)
	luma := func(x, y int) int {
		r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
		return int((299*r + 587*g + 114*bl) / 1000 >> 8)
	}

	// bg: median luma of the outermost ring.
	var ring []int
	for x := 0; x < w; x += step {
		ring = append(ring, luma(x, 0), luma(x, h-1))
	}
	for y := 0; y < h; y += step {
		ring = append(ring, luma(0, y), luma(w-1, y))
	}
	sort.Ints(ring)
	bg := ring[len(ring)/2]

	const tol = 24
	blankRow := func(y int) bool {
		miss := 0
		n := 0
		for x := 0; x < w; x += step {
			n++
			if d := luma(x, y) - bg; d > tol || d < -tol {
				miss++
			}
		}
		return miss*50 <= n // ≥98% background
	}
	blankCol := func(x int) bool {
		miss := 0
		n := 0
		for y := 0; y < h; y += step {
			n++
			if d := luma(x, y) - bg; d > tol || d < -tol {
				miss++
			}
		}
		return miss*50 <= n
	}

	top, bottom, left, right := 0, h-1, 0, w-1
	for top < bottom && blankRow(top) {
		top++
	}
	for bottom > top && blankRow(bottom) {
		bottom--
	}
	for left < right && blankCol(left) {
		left++
	}
	for right > left && blankCol(right) {
		right--
	}

	pad := max(w, h) / 100
	top = max(0, top-pad)
	left = max(0, left-pad)
	bottom = min(h-1, bottom+pad)
	right = min(w-1, right+pad)
	if top > h/4 || left > w/4 || h-1-bottom > h/4 || w-1-right > w/4 {
		return FullPageCrop
	}
	cw, ch := right-left+1, bottom-top+1
	if cw*ch*100 < w*h*55 {
		return FullPageCrop
	}
	round := func(v float64) float64 { return math.Round(v*10000) / 10000 }
	crop := [4]float64{
		round(float64(left) / float64(w)),
		round(float64(top) / float64(h)),
		round(float64(cw) / float64(w)),
		round(float64(ch) / float64(h)),
	}
	if crop == FullPageCrop {
		return FullPageCrop
	}
	return crop
}

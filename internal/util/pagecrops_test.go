package util

import (
	"archive/zip"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// page draws a w×h image filled with bg and a content rect filled with fg.
func page(w, h int, bg, fg color.Gray, content image.Rectangle) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if image.Pt(x, y).In(content) {
				img.SetGray(x, y, fg)
			} else {
				img.SetGray(x, y, bg)
			}
		}
	}
	return img
}

func TestContentCrop(t *testing.T) {
	white, black := color.Gray{Y: 250}, color.Gray{Y: 20}

	// 10% white border all around: crop lands near the content box.
	c := ContentCrop(page(800, 1200, white, black, image.Rect(80, 120, 720, 1080)))
	if c == FullPageCrop {
		t.Fatalf("bordered page not cropped")
	}
	for i, want := range [4]float64{0.1, 0.1, 0.8, 0.8} {
		if diff := c[i] - want; diff > 0.03 || diff < -0.03 {
			t.Fatalf("crop[%d] = %v, want ≈%v (crop %v)", i, c[i], want, c)
		}
	}

	// Black border around light content trims too (background is sampled).
	if ContentCrop(page(800, 1200, black, white, image.Rect(80, 120, 720, 1080))) == FullPageCrop {
		t.Fatalf("black-bordered page not cropped")
	}

	// Full-bleed content: nothing to trim.
	if c := ContentCrop(page(800, 1200, white, black, image.Rect(0, 0, 800, 1200))); c != FullPageCrop {
		t.Fatalf("full-bleed page cropped to %v", c)
	}

	// Blank page: the area floor bails out.
	if c := ContentCrop(page(800, 1200, white, white, image.Rect(0, 0, 0, 0))); c != FullPageCrop {
		t.Fatalf("blank page cropped to %v", c)
	}

	// Tiny content (credits speck): too little remains, left untouched.
	if c := ContentCrop(page(800, 1200, white, black, image.Rect(350, 550, 450, 650))); c != FullPageCrop {
		t.Fatalf("speck page cropped to %v", c)
	}

	// A 1px panel frame off the sampling grid must still stop the scan — the
	// step-advance bug ate 400+px into white panel interiors.
	framed := page(1200, 1200, white, white, image.Rect(0, 0, 0, 0))
	for i := 25; i < 1175; i++ { // 1px frame at inset 25 (not a step multiple)
		framed.SetGray(i, 25, black)
		framed.SetGray(i, 1174, black)
		framed.SetGray(25, i, black)
		framed.SetGray(1174, i, black)
	}
	c = ContentCrop(framed)
	if c == FullPageCrop {
		t.Fatalf("framed page not cropped")
	}
	for i := range 4 {
		limit := 25.0 / 1200
		got := c[i]
		if i >= 2 {
			got = 1 - c[i] // w/h: frame must survive on the far side too
			limit = (25.0 + 25.0) / 1200
		}
		if got > limit+0.001 {
			t.Fatalf("crop %v cuts into the 1px panel frame", c)
		}
	}
}

func TestPageCropsRoundtrip(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, content image.Rectangle) string {
		p := filepath.Join(dir, name)
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := png.Encode(f, page(400, 600, color.Gray{Y: 250}, color.Gray{Y: 20}, content)); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Deliberately unsorted: crops must follow reader (natural) order.
	p2 := write("2.png", image.Rect(0, 0, 400, 600))     // full bleed
	p10 := write("10.png", image.Rect(40, 60, 360, 540)) // bordered

	extra := PageCropsEntry([]string{p10, p2})
	if extra == nil {
		t.Fatal("PageCropsEntry returned nil for a trimmable set")
	}
	out := filepath.Join(dir, "out.cbz")
	if err := CreateCBZ([]string{p2, p10}, out, false, extra); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	crops := ReadPageCrops(zr.File)
	if len(crops) != 2 {
		t.Fatalf("crops = %v, want 2 pages", crops)
	}
	// Natural order: 2.png is page 1 (full bleed), 10.png page 2 (cropped).
	if crops[0] != FullPageCrop {
		t.Fatalf("page 1 crop = %v, want full page", crops[0])
	}
	if crops[1] == FullPageCrop {
		t.Fatalf("page 2 not cropped")
	}

	// Border-free set: no entry at all.
	if PageCropsEntry([]string{p2}) != nil {
		t.Fatal("full-bleed-only set should produce no crops entry")
	}
}

func TestAddCropsToCBZ(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "1.png")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, page(400, 600, color.Gray{Y: 250}, color.Gray{Y: 20},
		image.Rect(40, 60, 360, 540))); err != nil {
		t.Fatal(err)
	}
	f.Close()
	cbz := filepath.Join(dir, "ch.cbz")
	if err := CreateCBZ([]string{src}, cbz, false, nil); err != nil {
		t.Fatal(err)
	}

	added, err := AddCropsToCBZ(cbz)
	if err != nil || !added {
		t.Fatalf("AddCropsToCBZ() = %v, %v; want added", added, err)
	}
	zr, err := zip.OpenReader(cbz)
	if err != nil {
		t.Fatal(err)
	}
	crops := ReadPageCrops(zr.File)
	images := len(CBZImageEntries(zr.File))
	zr.Close()
	if len(crops) != 1 || crops[0] == FullPageCrop || images != 1 {
		t.Fatalf("crops = %v, images = %d", crops, images)
	}

	added, err = AddCropsToCBZ(cbz)
	if err != nil || added {
		t.Fatalf("second AddCropsToCBZ() = %v, %v; want no-op", added, err)
	}
}

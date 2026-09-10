package imagemask

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
	"time"
)

func TestRegionsRectangle(t *testing.T) {
	img := opaqueImage(32, 32)
	clearRect(img, 8, 8, 24, 24)
	regions, err := Regions(encodePNG(t, img))
	if err != nil {
		t.Fatal(err)
	}
	if len(regions) != 1 {
		t.Fatalf("regions = %#v", regions)
	}
	bbox := bounds(regions[0].Outer)
	if bbox.minX < 0.2 || bbox.minX > 0.3 || bbox.maxX < 0.7 || bbox.maxX > 0.8 {
		t.Fatalf("x bounds = %#v points=%v", bbox, regions[0].Outer)
	}
	if bbox.minY < 0.2 || bbox.minY > 0.3 || bbox.maxY < 0.7 || bbox.maxY > 0.8 {
		t.Fatalf("y bounds = %#v points=%v", bbox, regions[0].Outer)
	}
	if len(regions[0].Outer) < 6 || len(regions[0].Outer)%2 != 0 {
		t.Fatalf("outer points = %v", regions[0].Outer)
	}
	if len(regions[0].Holes) != 0 {
		t.Fatalf("holes = %#v", regions[0].Holes)
	}
}

func TestRegionsTwoBlobs(t *testing.T) {
	img := opaqueImage(40, 20)
	clearRect(img, 2, 2, 10, 10)
	clearRect(img, 28, 8, 38, 18)
	regions, err := Regions(encodePNG(t, img))
	if err != nil {
		t.Fatal(err)
	}
	if len(regions) != 2 {
		t.Fatalf("regions = %#v", regions)
	}
}

func TestRegionsDonutHasHole(t *testing.T) {
	img := opaqueImage(40, 40)
	clearRect(img, 6, 6, 34, 34)
	fillRect(img, 16, 16, 24, 24)
	regions, err := Regions(encodePNG(t, img))
	if err != nil {
		t.Fatal(err)
	}
	if len(regions) != 1 {
		t.Fatalf("regions = %#v", regions)
	}
	if len(regions[0].Holes) != 1 {
		t.Fatalf("holes = %#v", regions[0].Holes)
	}
	outer := bounds(regions[0].Outer)
	hole := bounds(regions[0].Holes[0])
	if hole.minX <= outer.minX || hole.maxX >= outer.maxX || hole.minY <= outer.minY || hole.maxY >= outer.maxY {
		t.Fatalf("hole %#v not inside outer %#v", hole, outer)
	}
}

func TestRegionsRejectsOpaqueAndEmpty(t *testing.T) {
	if _, err := Regions(encodePNG(t, opaqueImage(8, 8))); err != ErrNoAlpha {
		t.Fatalf("opaque err = %v", err)
	}
	if _, err := Regions(nil); err != ErrDecode {
		t.Fatalf("empty bytes err = %v", err)
	}
	if _, err := Regions([]byte("not-an-image")); err != ErrDecode {
		t.Fatalf("garbage err = %v", err)
	}
}

func TestRegionsFullTransparent(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	regions, err := Regions(encodePNG(t, img))
	if err != nil {
		t.Fatal(err)
	}
	if len(regions) != 1 {
		t.Fatalf("regions = %#v", regions)
	}
	bbox := bounds(regions[0].Outer)
	if bbox.minX > 0.05 || bbox.minY > 0.05 || bbox.maxX < 0.95 || bbox.maxY < 0.95 {
		t.Fatalf("full-frame bounds = %#v", bbox)
	}
}

func TestRegionsLargeMaskIsCheap(t *testing.T) {
	img := opaqueImage(2048, 1536)
	clearRect(img, 200, 150, 900, 700)
	raw := encodePNG(t, img)
	started := time.Now()
	regions, err := Regions(raw)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	if len(regions) != 1 {
		t.Fatalf("regions = %#v", regions)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("mask conversion too slow: %s", elapsed)
	}
}

type box struct{ minX, minY, maxX, maxY float64 }

func bounds(points []float64) box {
	b := box{minX: 1, minY: 1, maxX: 0, maxY: 0}
	for i := 0; i+1 < len(points); i += 2 {
		if points[i] < b.minX {
			b.minX = points[i]
		}
		if points[i] > b.maxX {
			b.maxX = points[i]
		}
		if points[i+1] < b.minY {
			b.minY = points[i+1]
		}
		if points[i+1] > b.maxY {
			b.maxY = points[i+1]
		}
	}
	return b
}

func opaqueImage(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	fillRect(img, 0, 0, w, h)
	return img
}

func fillRect(img *image.NRGBA, x0, y0, x1, y1 int) {
	white := color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			img.SetNRGBA(x, y, white)
		}
	}
}

func clearRect(img *image.NRGBA, x0, y0, x1, y1 int) {
	clear := color.NRGBA{}
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			img.SetNRGBA(x, y, clear)
		}
	}
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

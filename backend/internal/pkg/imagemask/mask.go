// Package imagemask converts an OpenAI-style transparent PNG mask into Grok
// Imagine selectionRegions (normalized polygons).
//
// Cost is a single O(W×H) pass plus contour tracing. Masks larger than 1024px
// on a side are OR-downsampled first, so a 4K mask stays in the low-millisecond
// range in pure Go — far cheaper than uploading or generating the image.
package imagemask

import (
	"bytes"
	"errors"
	"image"
	"image/draw"
	_ "image/jpeg"
	_ "image/png"
	"math"
)

const (
	maxTraceSide = 1024
	alphaEditMax = 128
	rdpPixelEps  = 1.25
	maxPoints    = 400
	minAreaPx    = 2.0
)

var (
	ErrDecode  = errors.New("mask 不是有效图片")
	ErrNoAlpha = errors.New("mask 没有透明像素（OpenAI 约定：透明区域才会被编辑）")
	ErrEmpty   = errors.New("mask 没有可编辑的透明区域")
)

// Region is one Grok selectionRegions entry: a normalized outer ring plus holes.
// Points are flattened [x0,y0,x1,y1,...] in 0–1 image space and are not closed.
type Region struct {
	Outer []float64
	Holes [][]float64
}

// Regions decodes an OpenAI inpaint mask (transparent = edit) into polygons.
func Regions(data []byte) ([]Region, error) {
	if len(data) == 0 {
		return nil, ErrDecode
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, ErrDecode
	}
	return regionsFromImage(img)
}

func regionsFromImage(img image.Image) ([]Region, error) {
	nrgba := asNRGBA(img)
	grid, gw, gh, transparent := editGrid(nrgba, maxTraceSide)
	if gw <= 0 || gh <= 0 {
		return nil, ErrDecode
	}
	if transparent == 0 {
		return nil, ErrNoAlpha
	}
	loops := traceLoops(grid, gw, gh)
	regions := classifyLoops(loops, gw, gh)
	if len(regions) == 0 {
		return nil, ErrEmpty
	}
	return regions, nil
}

func asNRGBA(img image.Image) *image.NRGBA {
	if nrgba, ok := img.(*image.NRGBA); ok {
		return nrgba
	}
	bounds := img.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(dst, dst.Bounds(), img, bounds.Min, draw.Src)
	return dst
}

func editGrid(img *image.NRGBA, maxSide int) ([]bool, int, int, int) {
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w <= 0 || h <= 0 {
		return nil, 0, 0, 0
	}
	side := w
	if h > side {
		side = h
	}
	gw, gh := w, h
	if side > maxSide {
		gw = int(math.Max(1, math.Round(float64(w)*float64(maxSide)/float64(side))))
		gh = int(math.Max(1, math.Round(float64(h)*float64(maxSide)/float64(side))))
	}
	grid := make([]bool, gw*gh)
	transparent := 0
	for y := 0; y < h; y++ {
		row := img.Pix[(y+bounds.Min.Y)*img.Stride+bounds.Min.X*4:]
		dy := y
		if gh != h {
			dy = y * gh / h
			if dy >= gh {
				dy = gh - 1
			}
		}
		dstRow := dy * gw
		for x := 0; x < w; x++ {
			a := row[x*4+3]
			if a < 255 {
				transparent++
			}
			if a >= alphaEditMax {
				continue
			}
			dx := x
			if gw != w {
				dx = x * gw / w
				if dx >= gw {
					dx = gw - 1
				}
			}
			grid[dstRow+dx] = true
		}
	}
	return grid, gw, gh, transparent
}

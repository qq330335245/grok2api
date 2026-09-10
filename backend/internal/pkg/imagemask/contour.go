package imagemask

import (
	"math"
	"sort"
)

type vertex struct {
	X int
	Y int
}

type pixelLoop struct {
	points []vertex
	area   float64
}

func traceLoops(edit []bool, w, h int) []pixelLoop {
	outgoing := map[vertex][]vertex{}
	isEdit := func(x, y int) bool {
		return x >= 0 && y >= 0 && x < w && y < h && edit[y*w+x]
	}
	add := func(a, b vertex) {
		outgoing[a] = append(outgoing[a], b)
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if !edit[y*w+x] {
				continue
			}
			if !isEdit(x, y-1) {
				add(vertex{x, y}, vertex{x + 1, y})
			}
			if !isEdit(x+1, y) {
				add(vertex{x + 1, y}, vertex{x + 1, y + 1})
			}
			if !isEdit(x, y+1) {
				add(vertex{x + 1, y + 1}, vertex{x, y + 1})
			}
			if !isEdit(x-1, y) {
				add(vertex{x, y + 1}, vertex{x, y})
			}
		}
	}

	used := map[vertex][]bool{}
	for from, tos := range outgoing {
		used[from] = make([]bool, len(tos))
	}

	var loops []pixelLoop
	for start, tos := range outgoing {
		for i := range tos {
			if used[start][i] {
				continue
			}
			path := walkLoop(start, i, outgoing, used)
			if len(path) < 3 {
				continue
			}
			area := math.Abs(shoelace(path))
			if area < minAreaPx {
				continue
			}
			loops = append(loops, pixelLoop{points: path, area: area})
		}
	}
	sort.Slice(loops, func(i, j int) bool { return loops[i].area > loops[j].area })
	return loops
}

func walkLoop(start vertex, first int, outgoing map[vertex][]vertex, used map[vertex][]bool) []vertex {
	path := []vertex{start}
	used[start][first] = true
	cur := outgoing[start][first]
	for {
		path = append(path, cur)
		if cur == start {
			break
		}
		tos := outgoing[cur]
		flags := used[cur]
		next := -1
		for i, to := range tos {
			if flags[i] {
				continue
			}
			next = i
			_ = to
			break
		}
		if next < 0 {
			break
		}
		flags[next] = true
		cur = tos[next]
		if len(path) > 1_000_000 {
			break
		}
	}
	if len(path) >= 2 && path[0] == path[len(path)-1] {
		path = path[:len(path)-1]
	}
	return path
}

func classifyLoops(loops []pixelLoop, w, h int) []Region {
	if len(loops) == 0 {
		return nil
	}
	parent := make([]int, len(loops))
	for i := range parent {
		parent[i] = -1
	}
	for i := range loops {
		best := -1
		bestArea := math.Inf(1)
		sample := loops[i].points[0]
		for j := range loops {
			if i == j || loops[j].area <= loops[i].area {
				continue
			}
			if !pointInLoop(sample, loops[j].points) {
				continue
			}
			if loops[j].area < bestArea {
				bestArea = loops[j].area
				best = j
			}
		}
		parent[i] = best
	}

	depthOf := func(idx int) int {
		d := 0
		for p := parent[idx]; p >= 0; p = parent[p] {
			d++
			if d > len(loops) {
				break
			}
		}
		return d
	}

	type pending struct {
		outer []float64
		holes [][]float64
	}
	byIndex := map[int]*pending{}
	order := make([]int, 0, len(loops))
	for i := range loops {
		if depthOf(i)%2 == 0 {
			pts := simplifyAndNormalize(loops[i].points, w, h)
			if len(pts) < 6 {
				continue
			}
			byIndex[i] = &pending{outer: pts}
			order = append(order, i)
		}
	}
	for i := range loops {
		if depthOf(i)%2 == 0 {
			continue
		}
		outer := -1
		for p := parent[i]; p >= 0; p = parent[p] {
			if depthOf(p)%2 == 0 {
				outer = p
				break
			}
		}
		dst, ok := byIndex[outer]
		if !ok {
			continue
		}
		pts := simplifyAndNormalize(loops[i].points, w, h)
		if len(pts) < 6 {
			continue
		}
		dst.holes = append(dst.holes, pts)
	}

	regions := make([]Region, 0, len(order))
	for _, idx := range order {
		item := byIndex[idx]
		if item == nil {
			continue
		}
		regions = append(regions, Region{Outer: item.outer, Holes: item.holes})
	}
	return regions
}

func simplifyAndNormalize(path []vertex, w, h int) []float64 {
	if len(path) == 0 || w <= 0 || h <= 0 {
		return nil
	}
	pts := make([]vertex, len(path))
	copy(pts, path)
	eps := rdpPixelEps
	for i := 0; i < 12; i++ {
		pts = rdp(pts, eps)
		if len(pts) <= maxPoints {
			break
		}
		eps *= 1.6
	}
	if len(pts) < 3 {
		return nil
	}
	out := make([]float64, 0, len(pts)*2)
	invW := 1 / float64(w)
	invH := 1 / float64(h)
	for _, p := range pts {
		x := clamp01(float64(p.X) * invW)
		y := clamp01(float64(p.Y) * invH)
		if n := len(out); n >= 2 && out[n-2] == x && out[n-1] == y {
			continue
		}
		out = append(out, x, y)
	}
	if len(out) >= 4 && out[0] == out[len(out)-2] && out[1] == out[len(out)-1] {
		out = out[:len(out)-2]
	}
	if len(out) < 6 {
		return nil
	}
	return out
}

func rdp(points []vertex, eps float64) []vertex {
	if len(points) < 3 {
		return points
	}
	maxDist := -1.0
	index := -1
	start, end := points[0], points[len(points)-1]
	for i := 1; i < len(points)-1; i++ {
		d := perpDist(points[i], start, end)
		if d > maxDist {
			maxDist = d
			index = i
		}
	}
	if maxDist <= eps || index < 0 {
		return []vertex{start, end}
	}
	left := rdp(points[:index+1], eps)
	right := rdp(points[index:], eps)
	return append(left[:len(left)-1], right...)
}

func perpDist(p, a, b vertex) float64 {
	dx := float64(b.X - a.X)
	dy := float64(b.Y - a.Y)
	if dx == 0 && dy == 0 {
		return math.Hypot(float64(p.X-a.X), float64(p.Y-a.Y))
	}
	return math.Abs(dy*float64(p.X-a.X)-dx*float64(p.Y-a.Y)) / math.Hypot(dx, dy)
}

func shoelace(path []vertex) float64 {
	if len(path) < 3 {
		return 0
	}
	sum := 0.0
	for i := range path {
		j := (i + 1) % len(path)
		sum += float64(path[i].X)*float64(path[j].Y) - float64(path[j].X)*float64(path[i].Y)
	}
	return sum / 2
}

func pointInLoop(p vertex, loop []vertex) bool {
	inside := false
	n := len(loop)
	if n < 3 {
		return false
	}
	px, py := float64(p.X)+0.5, float64(p.Y)+0.5
	for i, j := 0, n-1; i < n; j, i = i, i+1 {
		yi, yj := float64(loop[i].Y), float64(loop[j].Y)
		xi, xj := float64(loop[i].X), float64(loop[j].X)
		if (yi > py) != (yj > py) && px < (xj-xi)*(py-yi)/(yj-yi)+xi {
			inside = !inside
		}
	}
	return inside
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

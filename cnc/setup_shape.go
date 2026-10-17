package cnc

// Side-view silhouettes for the setup-sheet tool pages. Built here, not on
// the display: the firmware only scales and draws polylines, and the shapes
// can be unit-tested. Coordinates are integer mils (0.001 in); x is measured
// from the tool's centerline, y up from the tip.

import "math"

// ToolShape is a drawing: Lines are polylines [x0,y0,x1,y1,...]; HalfW and
// H are the extents the display scales to fit.
type ToolShape struct {
	HalfW int     `json:"hw"`
	H     int     `json:"h"`
	Lines [][]int `json:"l"`
}

// ShapeKind buckets a Fusion tool type into the handful of drawings.
func ShapeKind(t CardTool) string {
	ty := lower(t.Type)
	switch {
	case has(ty, "thread"):
		return "thread"
	case has(ty, "ball"):
		return "ball"
	case has(ty, "bull"):
		return "bull"
	case has(ty, "face"):
		return "face"
	case has(ty, "chamfer"), has(ty, "tapered"), has(ty, "dovetail"):
		return "chamfer"
	case has(ty, "spot"), has(ty, "center"):
		return "spot"
	case has(ty, "drill"), has(ty, "reamer"):
		return "drill"
	case has(ty, "tap"):
		return "tap"
	case has(ty, "mill") && t.CornerRadius > 0:
		return "bull"
	}
	return "flat"
}

func mil(in float64) int { return int(math.Round(in * 1000)) }

type pt struct{ x, y float64 }

func flat(ps ...pt) []int {
	out := make([]int, 0, 2*len(ps))
	for _, p := range ps {
		out = append(out, mil(p.x), mil(p.y))
	}
	return out
}

// arc returns points on a quarter-ish arc about (cx,cy) from angle a0 to a1
// (degrees, y up), n segments.
func arc(cx, cy, r, a0, a1 float64, n int) []pt {
	out := make([]pt, 0, n+1)
	for i := 0; i <= n; i++ {
		a := (a0 + (a1-a0)*float64(i)/float64(n)) * math.Pi / 180
		out = append(out, pt{cx + r*math.Cos(a), cy + r*math.Sin(a)})
	}
	return out
}

// BuildShape draws t. Dimensions come from the sidecar: diameter, flute
// length (cutting part), body length (below the holder), corner radius,
// and taperAngle (drill point full angle, chamfer half angle).
func BuildShape(t CardTool) ToolShape {
	kind := ShapeKind(t)
	d := t.Diameter
	if d <= 0 {
		d = 0.25
	}
	r := d / 2
	flute := t.FluteLength
	if flute <= 0 {
		flute = 2 * d
	}
	body := t.BodyLength
	if body < flute {
		body = flute * 1.5
	}
	H := body

	var outline []pt
	var extra [][]int

	switch kind {
	case "face":
		disc := math.Min(flute, 0.6*H)
		if disc <= 0 || disc > 0.5 {
			disc = math.Min(0.5, 0.6*H)
		}
		H = math.Max(H, disc+0.75)
		rs := 0.3 * r
		outline = []pt{{-rs, H}, {-rs, disc}, {-r, disc}, {-r, 0}, {r, 0}, {r, disc}, {rs, disc}, {rs, H}}
		flute = disc
	case "chamfer":
		half := t.TaperAngle
		if half <= 0 || half >= math.Pi/2 {
			half = math.Pi / 4
		}
		tip := 0.1 * r
		hc := math.Min((r-tip)/math.Tan(half), flute)
		outline = []pt{{-r, H}, {-r, hc}, {-tip, 0}, {tip, 0}, {r, hc}, {r, H}}
		flute = hc
	case "drill", "spot":
		full := t.TaperAngle
		if full <= 0 || full >= math.Pi {
			full = 118 * math.Pi / 180
			if kind == "spot" {
				full = math.Pi / 2
			}
		}
		hp := r / math.Tan(full/2)
		outline = []pt{{-r, H}, {-r, hp}, {0, 0}, {r, hp}, {r, H}}
		// helical flute strokes
		step := d * 0.5
		for y := hp + step*0.3; y+step*0.5 < flute; y += step {
			extra = append(extra, flat(pt{-r, y}, pt{r, y + step*0.5}))
		}
	case "tap":
		lead := math.Min(0.2*flute, 1.5*r)
		outline = []pt{{-r, H}, {-r, lead}, {-0.8 * r, 0}, {0.8 * r, 0}, {r, lead}, {r, H}}
		extra = threadMarks(r, lead, flute, d)
	case "thread":
		outline = []pt{{-r, H}, {-r, 0}, {r, 0}, {r, H}}
		extra = threadMarks(r, 0, flute, d)
	case "ball", "bull":
		cr := t.CornerRadius
		if kind == "ball" || cr > r {
			cr = r
		}
		if cr <= 0 {
			cr = 0
		}
		outline = append(outline, pt{-r, H})
		if cr > 0 {
			outline = append(outline, arc(-r+cr, cr, cr, 180, 270, 8)...)
			outline = append(outline, arc(r-cr, cr, cr, 270, 360, 8)...)
		} else {
			outline = append(outline, pt{-r, 0}, pt{r, 0})
		}
		outline = append(outline, pt{r, H})
		extra = fluteStrokes(r, math.Max(cr, 0), flute, d)
	default: // flat
		outline = []pt{{-r, H}, {-r, 0}, {r, 0}, {r, H}}
		extra = fluteStrokes(r, 0, flute, d)
	}

	lines := [][]int{flat(outline...)}
	// Where the cutting part ends and the shank begins.
	if kind != "face" {
		lines = append(lines, flat(pt{-r, flute}, pt{r, flute}))
	}
	lines = append(lines, extra...)
	return ToolShape{HalfW: mil(r), H: mil(H), Lines: lines}
}

func threadMarks(r, from, to, d float64) [][]int {
	var out [][]int
	pitch := math.Max(d/7, 0.02)
	for y := from + pitch*0.6; y+pitch*0.4 < to; y += pitch {
		out = append(out, flat(pt{-r, y}, pt{r, y + pitch*0.4}))
	}
	return out
}

func fluteStrokes(r, from, to, d float64) [][]int {
	var out [][]int
	step := math.Max(d*0.9, 0.08)
	for y := from + step*0.3; y+step*0.5 < to; y += step {
		out = append(out, flat(pt{-r, y}, pt{r, y + step*0.5}))
	}
	return out
}

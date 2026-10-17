package cnc

import (
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"testing"
)

var shapeCases = []CardTool{
	{Number: 7, Type: "flat end mill", Diameter: 0.5, FluteLength: 1, BodyLength: 2.5},
	{Number: 5, Type: "bullnose end mill", Diameter: 0.5, CornerRadius: 0.06, FluteLength: 1.1, BodyLength: 3},
	{Number: 16, Type: "ball end mill", Diameter: 0.25, CornerRadius: 0.125, FluteLength: 0.6, BodyLength: 2},
	{Number: 1, Type: "face mill", Diameter: 3, FluteLength: 0.3, BodyLength: 1.5},
	{Number: 15, Type: "drill", Diameter: 0.5, TaperAngle: 2.356194490192345, FluteLength: 2.5, BodyLength: 2.6},
	{Number: 6, Type: "tap right hand", Diameter: 0.375, FluteLength: 1, BodyLength: 3},
	{Number: 14, Type: "chamfer mill", Diameter: 0.375, TaperAngle: 0.7853981633974483, FluteLength: 0.75, BodyLength: 1.5},
	{Number: 13, Type: "thread mill", Diameter: 0.3, FluteLength: 0.6, BodyLength: 2},
	{Number: 8, Type: "spot drill", Diameter: 0.25, TaperAngle: 1.5707963267948966, FluteLength: 0.5, BodyLength: 1.8},
}

func TestBuildShapeBounds(t *testing.T) {
	for _, tl := range shapeCases {
		s := BuildShape(tl)
		if s.H <= 0 || s.HalfW <= 0 || len(s.Lines) == 0 {
			t.Fatalf("%s: empty shape %+v", tl.Type, s)
		}
		for _, l := range s.Lines {
			if len(l)%2 != 0 || len(l) < 4 {
				t.Fatalf("%s: malformed polyline %v", tl.Type, l)
			}
			for i := 0; i < len(l); i += 2 {
				if l[i] < -s.HalfW-1 || l[i] > s.HalfW+1 || l[i+1] < 0 || l[i+1] > s.H {
					t.Fatalf("%s: point (%d,%d) outside %dx%d", tl.Type, l[i], l[i+1], s.HalfW, s.H)
				}
			}
		}
	}
	// Ball mill: the tip is a semicircle, so the lowest point sits at y=0
	// on the centerline and the sides are vertical only above y=r.
	b := BuildShape(shapeCases[2])
	minAtCenter := math.MaxInt
	for i := 0; i+1 < len(b.Lines[0]); i += 2 {
		if abs := b.Lines[0][i]; abs >= -2 && abs <= 2 && b.Lines[0][i+1] < minAtCenter {
			minAtCenter = b.Lines[0][i+1]
		}
	}
	if minAtCenter != 0 {
		t.Fatalf("ball tip should reach y=0 on the centerline, got %d", minAtCenter)
	}
}

// TestRenderShapes writes PNGs when SETUP_SHAPE_PNG_DIR is set, so the
// silhouettes can be looked at; otherwise it only checks they rasterize.
func TestRenderShapes(_ *testing.T) {
	dir := os.Getenv("SETUP_SHAPE_PNG_DIR")
	for i, tl := range shapeCases {
		s := BuildShape(tl)
		const W, H = 400, 380
		img := image.NewGray(image.Rect(0, 0, W, H))
		for k := range img.Pix {
			img.Pix[k] = 255
		}
		scale := math.Min(float64(H-20)/float64(s.H), float64(W-20)/float64(2*s.HalfW))
		px := func(x, y int) (int, int) {
			return W/2 + int(math.Round(float64(x)*scale)), H - 10 - int(math.Round(float64(y)*scale))
		}
		for _, l := range s.Lines {
			for j := 0; j+3 < len(l); j += 2 {
				x0, y0 := px(l[j], l[j+1])
				x1, y1 := px(l[j+2], l[j+3])
				line(img, x0, y0, x1, y1)
			}
		}
		if dir != "" {
			f, _ := os.Create(filepath.Join(dir, tl.Type+"-"+string(rune('a'+i))+".png"))
			_ = png.Encode(f, img)
			f.Close()
		}
	}
}

func line(img *image.Gray, x0, y0, x1, y1 int) {
	dx, dy := x1-x0, y1-y0
	n := int(math.Max(math.Abs(float64(dx)), math.Abs(float64(dy))))
	for i := 0; i <= n; i++ {
		x, y := x0+dx*i/maxInt(n, 1), y0+dy*i/maxInt(n, 1)
		if image.Pt(x, y).In(img.Bounds()) {
			img.SetGray(x, y, color.Gray{0})
		}
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

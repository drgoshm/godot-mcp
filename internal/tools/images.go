package tools

import (
	"bytes"
	"image"
	"image/png"
	"os"
)

// loadPNG читает PNG и, если он шире maxWidth, уменьшает его усреднением
// по площади. Большие картинки дорого обходятся агенту в контексте, а мелкие
// детали при уменьшении до ~1280 px всё ещё различимы.
func loadPNG(path string, maxWidth int) (data []byte, width, height int, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, 0, err
	}
	return scalePNG(raw, maxWidth)
}

// scalePNG — то же для PNG в памяти (кадр, присланный игрой через мост).
func scalePNG(raw []byte, maxWidth int) (data []byte, width, height int, err error) {
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, 0, 0, err
	}
	b := img.Bounds()
	if maxWidth <= 0 || b.Dx() <= maxWidth {
		return raw, b.Dx(), b.Dy(), nil
	}
	small := downscale(img, maxWidth, max(1, b.Dy()*maxWidth/b.Dx()))
	var buf bytes.Buffer
	if err := png.Encode(&buf, small); err != nil {
		return nil, 0, 0, err
	}
	return buf.Bytes(), small.Bounds().Dx(), small.Bounds().Dy(), nil
}

// downscale уменьшает картинку до w×h, усредняя все исходные пиксели,
// попадающие в каждый новый (box filter).
func downscale(src image.Image, w, h int) *image.NRGBA {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		y0, y1 := sb.Min.Y+y*sh/h, sb.Min.Y+max((y+1)*sh/h, y*sh/h+1)
		for x := 0; x < w; x++ {
			x0, x1 := sb.Min.X+x*sw/w, sb.Min.X+max((x+1)*sw/w, x*sw/w+1)
			var r, g, bl, a, n uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					cr, cg, cb, ca := src.At(sx, sy).RGBA()
					r, g, bl, a, n = r+uint64(cr), g+uint64(cg), bl+uint64(cb), a+uint64(ca), n+1
				}
			}
			// RGBA() отдаёт premultiplied 16 бит; NRGBA хранит straight 8 бит.
			i := dst.PixOffset(x, y)
			if a == 0 {
				continue
			}
			dst.Pix[i+0] = uint8(r * 0xff / a)
			dst.Pix[i+1] = uint8(g * 0xff / a)
			dst.Pix[i+2] = uint8(bl * 0xff / a)
			dst.Pix[i+3] = uint8(a / n >> 8)
		}
	}
	return dst
}

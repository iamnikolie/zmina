package chat

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif" // decoders Thumb reads
	"image/jpeg"
	_ "image/png"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// ThumbSide is the long side of a thumbnail Chat draws as a 56 px tile (D-20 d1: three device pixels a CSS pixel).
const ThumbSide = 168

// maxPixels bounds the image a thumbnail is made from: its header is read first, so a small file that claims a huge
// canvas is refused before it is decoded.
const maxPixels = 40 << 20

// Thumb is a JPEG of an image (PNG, JPEG, GIF or WebP) scaled to fit side × side, over white where it is transparent;
// an image already that small is scaled by 1.
func Thumb(data []byte, side int) ([]byte, error) {
	if len(data) > MaxFileBytes {
		return nil, errors.New("image too large")
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > maxPixels {
		return nil, errors.New("not an image Chat shows")
	}

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("not an image Chat shows")
	}

	w, h := cfg.Width, cfg.Height
	if w > side || h > side {
		if w >= h {
			w, h = side, max(1, h*side/w)
		} else {
			w, h = max(1, w*side/h), side
		}
	}

	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Over, nil)

	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 82}); err != nil {
		return nil, err
	}

	return out.Bytes(), nil
}

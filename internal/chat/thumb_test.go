package chat

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/stretchr/testify/require"
)

func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, 0, color.NRGBA{R: 200, A: 255})
	}
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, img))
	return b.Bytes()
}

// A thumbnail fits ThumbSide on its long side and keeps the shape; a small image is not scaled up; anything else is
// refused.
func TestThumbFitsTheTile(t *testing.T) {
	for _, c := range []struct{ w, h, wantW, wantH int }{{1400, 1800, 130, 168}, {1400, 700, 168, 84}, {100, 40, 100, 40}} {
		out, err := Thumb(pngOf(t, c.w, c.h), ThumbSide)
		require.NoError(t, err)
		img, err := jpeg.Decode(bytes.NewReader(out))
		require.NoError(t, err)
		require.Equal(t, image.Pt(c.wantW, c.wantH), img.Bounds().Size())
	}
	_, err := Thumb([]byte("%PDF-1.7"), ThumbSide)
	require.Error(t, err)
	_, err = Thumb([]byte("<svg/>"), ThumbSide)
	require.Error(t, err)
}

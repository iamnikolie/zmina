package chat

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// jpegOf is a w × h JPEG whose EXIF says orientation (0: no EXIF), in the byte order given.
func jpegOf(t *testing.T, w, h, orientation int, order binary.ByteOrder) []byte {
	var plain bytes.Buffer
	require.NoError(t, jpeg.Encode(&plain, image.NewRGBA(image.Rect(0, 0, w, h)), nil))
	if orientation == 0 {
		return plain.Bytes()
	}
	tiff := make([]byte, 8+2+12+4)
	if order == binary.LittleEndian {
		copy(tiff, "II")
	} else {
		copy(tiff, "MM")
	}
	order.PutUint16(tiff[2:], 42)
	order.PutUint32(tiff[4:], 8)
	order.PutUint16(tiff[8:], 1)
	order.PutUint16(tiff[10:], 0x0112)
	order.PutUint16(tiff[12:], 3)
	order.PutUint32(tiff[14:], 1)
	order.PutUint16(tiff[18:], uint16(orientation))
	payload := append([]byte("Exif\x00\x00"), tiff...)
	segment := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(segment[2:], uint16(len(payload)+2))
	segment = append(segment, payload...)
	data := plain.Bytes()
	return append(append(append([]byte{}, data[:2]...), segment...), data[2:]...)
}

func TestImageSizeIsAsShownWithItsOrientation(t *testing.T) {
	w, h := imageSize(pngOf(t, 30, 20))
	require.Equal(t, [2]int{30, 20}, [2]int{w, h})
	for _, c := range []struct {
		orientation int
		order       binary.ByteOrder
		want        [2]int
	}{
		{0, binary.BigEndian, [2]int{40, 10}},
		{1, binary.BigEndian, [2]int{40, 10}},
		{3, binary.LittleEndian, [2]int{40, 10}},
		{6, binary.BigEndian, [2]int{10, 40}}, // a phone photo taken upright
		{8, binary.LittleEndian, [2]int{10, 40}},
	} {
		w, h := imageSize(jpegOf(t, 40, 10, c.orientation, c.order))
		require.Equal(t, c.want, [2]int{w, h}, "orientation %d", c.orientation)
	}
	w, h = imageSize([]byte("%PDF-1.7\n"))
	require.Equal(t, [2]int{0, 0}, [2]int{w, h})
	// A truncated or lying EXIF never panics and reads as no orientation.
	broken := jpegOf(t, 40, 10, 6, binary.BigEndian)
	require.Equal(t, 0, jpegOrientation(broken[:30]))
	require.Equal(t, 0, tiffOrientation([]byte("MM\x00\x2a\xff\xff\xff\xff")))
}

func TestFilesKeepAnImagesSizeAndAcceptAFileStoredWithout(t *testing.T) {
	store := FileStore{Dir: t.TempDir(), Role: "dev"}
	file := Upload{ID: "abcdefab-1234-1234-1234-123456789abd", Name: "IMG_1.jpeg", Data: jpegOf(t, 40, 10, 6, binary.BigEndian)}
	meta, err := store.Put(file)
	require.NoError(t, err)
	require.Equal(t, "image/jpeg", meta.Type)
	require.Equal(t, [2]int{10, 40}, [2]int{meta.Width, meta.Height})
	got, _, err := store.Get(file.ID)
	require.NoError(t, err)
	require.Equal(t, meta, got)

	// A file stored by an earlier zmina has no size: a retry of its upload is the same file, its metadata as stored.
	meta.Width, meta.Height = 0, 0
	raw, err := json.Marshal(meta)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "width")
	require.NoError(t, os.WriteFile(filepath.Join(store.root(file.ID), "metadata.json"), raw, 0600))
	again, err := store.Put(file)
	require.NoError(t, err)
	require.Equal(t, meta, again)
}

func TestSizedFillsAnOlderImagesSizeFromItsFile(t *testing.T) {
	store := FileStore{Dir: t.TempDir(), Role: "dev"}
	photo := Upload{ID: "abcdefab-1234-1234-1234-123456789abe", Name: "IMG_2.jpeg", Data: jpegOf(t, 40, 10, 6, binary.BigEndian)}
	doc := Upload{ID: "abcdefab-1234-1234-1234-123456789abf", Name: "notes.pdf", Data: []byte("%PDF-1.7\n")}
	stored, err := store.Put(photo)
	require.NoError(t, err)
	pdf, err := store.Put(doc)
	require.NoError(t, err)
	older := stored
	older.Width, older.Height = 0, 0
	given := []Attachment{older, pdf}
	got := store.Sized(given)
	require.Equal(t, []Attachment{stored, pdf}, got)
	require.Zero(t, given[0].Width, "the attachments given stay as they are")
	// One that knows its size, or a file gone, is left as it is.
	require.Equal(t, []Attachment{stored}, store.Sized([]Attachment{stored}))
	gone := Attachment{ID: "abcdefab-1234-1234-1234-123456789ab0", Name: "x.png", Type: "image/png"}
	require.Equal(t, []Attachment{gone}, store.Sized([]Attachment{gone}))
}

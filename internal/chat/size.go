package chat

import (
	"bytes"
	"encoding/binary"
	"image"
)

// imageSize is the size an image is shown at — its EXIF orientation applied, as browsers do — so Chat draws its tile
// at the final size before the bytes arrive. Zero for anything that is not a PNG, JPEG, GIF or WebP Chat shows.
func imageSize(data []byte) (width, height int) {
	cfg, kind, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > maxPixels {
		return 0, 0
	}
	if kind == "jpeg" && jpegOrientation(data) >= 5 {
		return cfg.Height, cfg.Width
	}
	return cfg.Width, cfg.Height
}

// jpegOrientation reads the EXIF orientation tag (1–8) of a JPEG's APP1 segment; 0 when there is none. 5–8 turn the
// picture a quarter, so its width and height swap.
func jpegOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return 0
	}
	for i := 2; i+4 <= len(data); {
		if data[i] != 0xFF {
			return 0
		}
		marker := data[i+1]
		if marker == 0xFF { // fill byte
			i++
			continue
		}
		if marker == 0xDA || marker == 0xD9 { // the image data starts, or ends: no EXIF before it
			return 0
		}
		length := int(binary.BigEndian.Uint16(data[i+2:]))
		if length < 2 || i+2+length > len(data) {
			return 0
		}
		segment := data[i+4 : i+2+length]
		if marker == 0xE1 && bytes.HasPrefix(segment, []byte("Exif\x00\x00")) {
			return tiffOrientation(segment[6:])
		}
		i += 2 + length
	}
	return 0
}

func tiffOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 0
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 0
	}
	if order.Uint16(tiff[2:]) != 42 {
		return 0
	}
	ifd := int(order.Uint32(tiff[4:]))
	if ifd < 8 || ifd+2 > len(tiff) {
		return 0
	}
	count := int(order.Uint16(tiff[ifd:]))
	for n := range count {
		entry := ifd + 2 + n*12
		if entry+12 > len(tiff) {
			return 0
		}
		if order.Uint16(tiff[entry:]) == 0x0112 && order.Uint16(tiff[entry+2:]) == 3 { // Orientation, SHORT
			if v := int(order.Uint16(tiff[entry+8:])); v >= 1 && v <= 8 {
				return v
			}
			return 0
		}
	}
	return 0
}

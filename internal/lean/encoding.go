package lean

import (
	"bytes"
	"unicode/utf8"
)

// cp1251High maps Windows-1251 bytes 0x80–0xBF; 0xC0–0xFF are А–я (U+0410–U+044F). 0x98 is unassigned.
var cp1251High = [64]rune{
	0x0402, 0x0403, 0x201A, 0x0453, 0x201E, 0x2026, 0x2020, 0x2021, 0x20AC, 0x2030, 0x0409, 0x2039, 0x040A, 0x040C, 0x040B, 0x040F,
	0x0452, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014, 0xFFFD, 0x2122, 0x0459, 0x203A, 0x045A, 0x045C, 0x045B, 0x045F,
	0x00A0, 0x040E, 0x045E, 0x0408, 0x00A4, 0x0490, 0x00A6, 0x00A7, 0x0401, 0x00A9, 0x0404, 0x00AB, 0x00AC, 0x00AD, 0x00AE, 0x0407,
	0x00B0, 0x00B1, 0x0406, 0x0456, 0x0491, 0x00B5, 0x00B6, 0x00B7, 0x0451, 0x2116, 0x0454, 0x00BB, 0x0458, 0x0405, 0x0455, 0x0457,
}

// Decode returns the text as UTF-8 and the encoding it was read in: "" for UTF-8, "cp1251" for a legacy Cyrillic
// file. Shown as UTF-8, such a file is mojibake; its outline and grep need it decoded. A file that is mostly valid
// UTF-8 with a stray bad byte stays UTF-8 (the bad bytes become U+FFFD). cp1251 needs both: most bytes above 0x7F are
// letters А–я (0xC0–0xFF), and they come in runs, as words do — Latin-1 accents (é, è) sit alone between ASCII.
func Decode(src []byte) ([]byte, string) {
	if utf8.Valid(src) {
		return src, ""
	}

	high, letters, inRuns, inUTF8 := 0, 0, 0, 0

	for i := 0; i < len(src); {
		r, size := utf8.DecodeRune(src[i:])
		if size > 1 && r != utf8.RuneError {
			inUTF8 += size
		}

		i += size
	}

	for i, c := range src {
		if c < 0x80 {
			continue
		}

		high++

		if c >= 0xC0 {
			letters++
		}

		if (i > 0 && src[i-1] >= 0x80) || (i+1 < len(src) && src[i+1] >= 0x80) {
			inRuns++
		}
	}

	if inUTF8*2 >= high || letters*2 < high || inRuns*10 < high*6 {
		return bytes.ToValidUTF8(src, []byte("\uFFFD")), ""
	}

	out := make([]byte, 0, len(src)+letters)

	for _, c := range src {
		switch {
		case c < 0x80:
			out = append(out, c)
		case c >= 0xC0:
			out = utf8.AppendRune(out, rune(0x0410+int(c)-0xC0))
		default:
			out = utf8.AppendRune(out, cp1251High[c-0x80])
		}
	}

	return out, "cp1251"
}

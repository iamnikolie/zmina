package mesh

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// wireDecoder consumes the entire newline-delimited frame before replying. A JSON decoder alone can return
// at the closing brace while its peer is still blocked writing the newline, deadlocking both writers.
type wireDecoder struct {
	reader *bufio.Reader
}

func (d *wireDecoder) Decode(v any) error {
	for {
		line, err := d.reader.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if len(bytes.TrimSpace(line)) == 0 {
			if err != nil {
				return err
			}
			continue
		}
		return json.Unmarshal(line, v)
	}
}

// Buffered hands only prefetched bytes to a raw tunnel; the decoder is not used after that transition.
func (d *wireDecoder) Buffered() io.Reader {
	buf, _ := d.reader.Peek(d.reader.Buffered())
	return bytes.NewReader(bytes.Clone(buf))
}

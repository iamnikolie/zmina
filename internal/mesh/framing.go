package mesh

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// What a frame (a line of the protocol) may weigh, read before the frame is whole, so that no peer can make a node hold more.
const (
	// helloFrameMax is the most bytes of a frame read before the peer has proved its key: the hellos and the auth, which carry a
	// certificate, the address book told and the crews' health. Anyone who reaches `zmina node serve --stdio` is such a peer.
	helloFrameMax = 4 << 20
	// frameMax is the most of a frame once the peer proved its key, and so is a certified node of the owner's: the largest frames are
	// a chat file of chat.MaxFileBytes (8 MiB, a third more as base64) and a history of 2 MiB of text whose control characters
	// JSON writes as six bytes each. A move's bundle (1 MiB) and a crew file (256 KiB) are far under it, and under helloFrameMax too.
	frameMax = 16 << 20
)

// errFrameTooLarge is the end of a session whose peer sent a frame over the cap.
var errFrameTooLarge = errors.New("a frame over the size a node takes")

// wireDecoder consumes the entire newline-delimited frame before replying. A JSON decoder alone can return
// at the closing brace while its peer is still blocked writing the newline, deadlocking both writers. A frame
// over the cap is refused where it passes it, not read to its end.
type wireDecoder struct {
	reader *bufio.Reader
	// max is the cap of a frame in bytes, without its newline; zero is helloFrameMax.
	max int
}

// raise lifts the cap to what a peer that proved its key may send. The format is the same: only the read limit changes.
func (d *wireDecoder) raise() { d.max = frameMax }

func (d *wireDecoder) limit() int {
	if d.max > 0 {
		return d.max
	}

	return helloFrameMax
}

// readLine is bufio.Reader.ReadBytes('\n') that gives up on a line longer than the cap, having taken no more than the cap and
// one buffer of it.
func (d *wireDecoder) readLine() ([]byte, error) {
	var line []byte

	for {
		chunk, err := d.reader.ReadSlice('\n')

		// The newline of a whole line is not part of what the cap counts.
		size := len(line) + len(chunk)
		if err == nil {
			size--
		}

		if size > d.limit() {
			return nil, fmt.Errorf("%w: more than %d bytes", errFrameTooLarge, d.limit())
		}

		line = append(line, chunk...)

		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}

		return line, err
	}
}

func (d *wireDecoder) Decode(v any) error {
	for {
		line, err := d.readLine()
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

package mesh

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/chat"
)

// A stream may split a JSON object from its newline. Its writer cannot finish until both are read.
type fragmentedMeshStream struct {
	io.ReadWriteCloser
}

func (s fragmentedMeshStream) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return s.ReadWriteCloser.Read(p)
}

func TestMeshReadsWholeFrameBeforeReply(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close(); _ = server.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = client.Close(); _ = server.Close() })
	defer stop()
	done := make(chan error, 1)
	go func() {
		c := newConn(fragmentedMeshStream{server})
		_, err := c.expect("hello")
		if err == nil {
			err = c.send(wire{Op: "ok"})
		}
		done <- err
	}()
	c := newConn(client)
	require.NoError(t, c.send(wire{Op: "hello", Text: strings.Repeat("x", 1024)}), "frame delimiter must be read before the peer replies")
	_, err := c.expect("ok")
	require.NoError(t, err)
	require.NoError(t, <-done)
}

func TestMeshFramingPreservesPrefetchedTunnelBytes(t *testing.T) {
	for _, size := range []int{16, 8192} {
		payload := bytes.Repeat([]byte{0, 255, '\n', 1}, size)
		raw := bytes.NewBuffer(append([]byte("{\"op\":\"terminal/tunnel-ready\"}\n"), payload...))
		c := newConn(raw)
		_, err := c.expect("terminal/tunnel-ready")
		require.NoError(t, err)
		got, err := io.ReadAll(io.MultiReader(c.dec.Buffered(), raw))
		require.NoError(t, err)
		require.Equal(t, payload, got, "raw tunnel must retain each prefetched byte exactly once")
	}
}

func TestMeshFramingKeepsLegacyJSONLinesAndFinalEOF(t *testing.T) {
	raw := bytes.NewBufferString("\n  \n{\"op\":\"hello\"}\n{\"op\":\"ok\"}")
	c := newConn(raw)
	_, err := c.expect("hello")
	require.NoError(t, err)
	_, err = c.expect("ok")
	require.NoError(t, err)
	var w wire
	require.ErrorIs(t, c.dec.Decode(&w), io.EOF)
}

func TestMeshFramingConsumesEncoderBoundarySizes(t *testing.T) {
	for _, size := range []int{512, 1024, 4096, 16384} {
		w := wire{Op: "hello", Text: "x"}
		base, err := json.Marshal(w)
		require.NoError(t, err)
		w.Text = strings.Repeat("x", size-len(base)+1)
		raw, err := json.Marshal(w)
		require.NoError(t, err)
		require.Len(t, raw, size)
		buffer := bytes.NewBuffer(append(raw, '\n'))
		c := newConn(buffer)
		got, err := c.expect("hello")
		require.NoError(t, err)
		require.Equal(t, w.Text, got.Text)
		remaining, err := io.ReadAll(io.MultiReader(c.dec.Buffered(), buffer))
		require.NoError(t, err)
		require.Empty(t, remaining, "delimiter must not leak into the next protocol phase")
	}
}

// Exec/SSH use buffered OS pipes. Verify the same frames there as well as on synchronous streams.
func TestMeshFramesOverStdioPipes(t *testing.T) {
	for _, size := range []int{1024, 65536} {
		in, toServer, err := os.Pipe()
		require.NoError(t, err)
		out, toClient, err := os.Pipe()
		require.NoError(t, err)
		closeAll := func() { _ = in.Close(); _ = toServer.Close(); _ = out.Close(); _ = toClient.Close() }
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		stop := context.AfterFunc(ctx, closeAll)
		done := make(chan error, 1)
		go func() {
			c := newConn(struct {
				io.Reader
				io.Writer
			}{in, toClient})
			_, err := c.expect("hello")
			if err == nil {
				err = c.send(wire{Op: "ok"})
			}
			done <- err
		}()
		c := newConn(struct {
			io.Reader
			io.Writer
		}{out, toServer})
		err = c.send(wire{Op: "hello", Text: strings.Repeat("x", size)})
		if err == nil {
			_, err = c.expect("ok")
		}
		stop()
		cancel()
		closeAll()
		require.NoError(t, err)
		require.NoError(t, <-done)
	}
}

// foreverX is a stream of bytes with no newline in it, and counts what was taken from it.
type foreverX struct{ taken int }

func (f *foreverX) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}

	f.taken += len(p)

	return len(p), nil
}

// A line with no end is not read to its end: a peer that has not proved its key is not owed more memory than the cap, and the
// decoder stops taking bytes at it.
func TestMeshFramingStopsReadingAFrameOverTheCap(t *testing.T) {
	src := &foreverX{}
	c := newConn(struct {
		io.Reader
		io.Writer
	}{src, io.Discard})

	var w wire

	err := c.dec.Decode(&w)
	require.ErrorIs(t, err, errFrameTooLarge)
	assert.Contains(t, err.Error(), "4194304")
	assert.Less(t, src.taken, helloFrameMax+64<<10, "it read about the cap and no more: %d bytes", src.taken)
	assert.GreaterOrEqual(t, src.taken, helloFrameMax)
}

// What fits the cap is taken, a frame of the cap exactly too; the first byte over is not.
func TestMeshFramingCapBoundary(t *testing.T) {
	frame := func(size int) []byte {
		w := wire{Op: "hello", Text: "x"}
		base, err := json.Marshal(w)
		require.NoError(t, err)
		w.Text = strings.Repeat("x", size-len(base)+1)
		raw, err := json.Marshal(w)
		require.NoError(t, err)
		require.Len(t, raw, size)

		return append(raw, '\n')
	}

	got, err := newConn(bytes.NewBuffer(frame(helloFrameMax))).expect("hello")
	require.NoError(t, err)
	assert.Len(t, got.Text, helloFrameMax-len(`{"op":"hello","text":""}`))

	_, err = newConn(bytes.NewBuffer(frame(helloFrameMax + 1))).expect("hello")
	require.ErrorIs(t, err, errFrameTooLarge)
}

// The cap is raised once the peer proved its key, to what the largest frame a certified peer sends needs: a chat file of
// MaxFileBytes goes as base64 inside one frame.
func TestMeshFramingCapIsRaisedAfterTheHello(t *testing.T) {
	big := bytes.Repeat([]byte{'y'}, 6<<20)
	line := append(append([]byte(`{"op":"chat/result","text":"`), big...), []byte("\"}\n")...)

	c := newConn(bytes.NewBuffer(line))
	_, err := c.expect("chat/result")
	require.ErrorIs(t, err, errFrameTooLarge, "before the hello is over")

	c = newConn(bytes.NewBuffer(line))
	c.dec.raise()
	got, err := c.expect("chat/result")
	require.NoError(t, err)
	assert.Len(t, got.Text, len(big))

	// And not without limit: past the raised cap it stops too.
	src := &foreverX{}
	c = newConn(struct {
		io.Reader
		io.Writer
	}{src, io.Discard})
	c.dec.raise()

	var w wire

	require.ErrorIs(t, c.dec.Decode(&w), errFrameTooLarge)
	assert.Less(t, src.taken, frameMax+64<<10)
}

// The largest legitimate frame: a chat file of the largest size, base64-encoded in a signed request, fits the cap of a session.
func TestMeshFrameCapFitsTheLargestLegitimateFrames(t *testing.T) {
	file := bytes.Repeat([]byte{0xff}, 8<<20)
	raw, err := json.Marshal(wire{Op: "chat/upload", Terminal: &TerminalRequest{Upload: &chat.Upload{Data: file}}})
	require.NoError(t, err)
	assert.Less(t, len(raw), frameMax, "an 8 MiB chat file")

	bundle := bytes.Repeat([]byte{0xff}, RoleTakeMax)
	raw, err = json.Marshal(wire{Op: "role/take", Terminal: &TerminalRequest{Payload: bundle}})
	require.NoError(t, err)
	assert.Less(t, len(raw), helloFrameMax, "a 1 MiB move bundle, base64, also fits the smaller cap")

	raw, err = json.Marshal(wire{Op: "crew/put", Terminal: &TerminalRequest{Payload: bytes.Repeat([]byte{0xff}, CrewFileMax)}})
	require.NoError(t, err)
	assert.Less(t, len(raw), helloFrameMax)
}

// A peer that has not proved its key cannot make a node hold more than the cap: a hello without an end is refused where it is.
func TestServeRefusesAHelloOverTheCapWithoutReadingItAll(t *testing.T) {
	owner := newOwner(t, "alpha")
	client, server := net.Pipe()

	defer func() { _ = client.Close() }()

	require.NoError(t, client.SetDeadline(time.Now().Add(10*time.Second)))

	done := make(chan error, 1)

	go func() {
		defer func() { _ = server.Close() }()

		done <- owner.Serve(server, (&inbox{}).deliver, time.Now)
	}()

	sent := make(chan int, 1)

	go func() {
		chunk := bytes.Repeat([]byte{'x'}, 64<<10)
		total := 0

		for {
			n, err := client.Write(chunk)
			total += n

			if err != nil {
				sent <- total

				return
			}
		}
	}()

	err := <-done
	require.ErrorIs(t, err, errFrameTooLarge)
	require.NoError(t, client.Close())
	assert.Less(t, <-sent, helloFrameMax+1<<20, "the writer was stopped near the cap")
}

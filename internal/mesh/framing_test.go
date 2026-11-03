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

	"github.com/stretchr/testify/require"
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

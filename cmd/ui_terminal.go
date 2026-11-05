package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
	"github.com/iamnikolie/zmina/internal/mesh"
)

type terminalNode struct {
	Name  string `json:"name"`
	Local bool   `json:"local"`
}

func (a *app) uiTerminals(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/terminal/history", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		text, err := a.terminalHistory(ctx, r.URL.Query().Get("node"), r.URL.Query().Get("crew"), r.URL.Query().Get("role"), 10000)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, map[string]string{"text": text})
	})
	mux.HandleFunc("GET /api/terminal/nodes", func(w http.ResponseWriter, r *http.Request) {
		nodes := []terminalNode{{Name: "", Local: true}}
		if n, err := openNode(); err == nil {
			nodes[0].Name = n.Name
			if peers, err := n.Peers(); err == nil {
				for _, p := range peers {
					nodes = append(nodes, terminalNode{Name: p.Name})
				}
			}
		}
		writeJSON(w, nodes)
	})
	mux.HandleFunc("GET /api/terminal/targets", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		targets, err := a.terminalList(ctx, r.URL.Query().Get("node"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, targets)
	})
	mux.HandleFunc("GET /api/terminal/connect", func(w http.ResponseWriter, r *http.Request) {
		serveBrowserTerminal(w, r, a.openTerminal)
	})
}

// A terminal GET can write to a role. It needs a WebSocket upgrade and an exact same-origin browser,
// in addition to the UI guard's token and Host checks. Never accept a cross-origin cookie handshake.
func terminalOrigin(r *http.Request) bool {
	origin, err := url.Parse(r.Header.Get("Origin"))
	return err == nil && (origin.Scheme == "http" || origin.Scheme == "https") && origin.Host == r.Host && origin.Path == "" && origin.RawQuery == "" && origin.User == nil
}

type terminalOpen func(context.Context, string, string, string, uint16, uint16) (mesh.Terminal, error)

func serveBrowserTerminal(w http.ResponseWriter, r *http.Request, open terminalOpen) {
	if !websocket.IsWebSocketUpgrade(r) || !terminalOrigin(r) {
		http.Error(w, "terminal needs a same-origin WebSocket", http.StatusForbidden)
		return
	}
	cols, e1 := strconv.ParseUint(r.URL.Query().Get("cols"), 10, 16)
	rows, e2 := strconv.ParseUint(r.URL.Query().Get("rows"), 10, 16)
	if e1 != nil || e2 != nil || cols < 2 || cols > 500 || rows < 2 || rows > 300 {
		http.Error(w, "invalid terminal size", http.StatusBadRequest)
		return
	}
	upgrader := websocket.Upgrader{CheckOrigin: terminalOrigin, HandshakeTimeout: 10 * time.Second}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = ws.Close() }()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	setup := time.AfterFunc(30*time.Second, func() { cancel(); _ = ws.Close() })
	tty, err := open(ctx, r.URL.Query().Get("node"), r.URL.Query().Get("crew"), r.URL.Query().Get("role"), uint16(cols), uint16(rows))
	setup.Stop()
	if err != nil {
		_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_ = ws.WriteJSON(map[string]string{"error": err.Error()})
		return
	}
	defer func() { _ = tty.Close() }()
	ws.SetReadLimit(64 << 10)
	_ = ws.SetReadDeadline(time.Now().Add(45 * time.Second))
	ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(45 * time.Second)) })
	if err = ws.WriteJSON(map[string]string{"status": "connected"}); err != nil {
		return
	}
	// Output and input are independent. Closing either path releases the other and detaches tmux.
	go func() {
		defer cancel()
		defer func() { _ = ws.Close() }()
		buf := make([]byte, 16<<10)
		for {
			n, err := tty.Read(buf)
			if n > 0 {
				_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if werr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		ping := time.NewTicker(15 * time.Second)
		defer ping.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ping.C:
				if err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
					cancel()
					_ = ws.Close()
					return
				}
			}
		}
	}()
	for {
		kind, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		switch kind {
		case websocket.BinaryMessage:
			if _, err = tty.Write(data); err != nil {
				return
			}
		case websocket.TextMessage:
			var size struct {
				Type string `json:"type"`
				Cols uint16 `json:"cols"`
				Rows uint16 `json:"rows"`
			}
			if err = json.Unmarshal(data, &size); err != nil || size.Type != "resize" || size.Cols < 2 || size.Cols > 500 || size.Rows < 2 || size.Rows > 300 {
				return
			}
			if err = tty.Resize(size.Cols, size.Rows); err != nil {
				return
			}
		default:
			return
		}
	}
}

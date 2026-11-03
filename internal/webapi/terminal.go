package webapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"

	"github.com/coder/websocket"
)

type terminalMessage struct {
	Type string `json:"type"`
	Data string `json:"data"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

func (a *API) terminal(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Query().Get("host")
	if _, err := a.Store.GetHostByName(r.Context(), host); err != nil {
		apiError(w, 404, err)
		return
	}
	client, err := a.Pool.Get(r.Context(), host)
	if err != nil {
		apiError(w, 502, err)
		return
	}
	session, err := client.NewSession()
	if err != nil {
		apiError(w, 502, err)
		return
	}
	defer session.Close()
	cols, _ := strconv.Atoi(r.URL.Query().Get("cols"))
	rows, _ := strconv.Atoi(r.URL.Query().Get("rows"))
	if cols <= 0 {
		cols = 120
	}
	if rows <= 0 {
		rows = 30
	}
	if err = session.RequestPty("xterm-256color", rows, cols, sshTerminalModes); err != nil {
		apiError(w, 502, err)
		return
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		apiError(w, 502, err)
		return
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		apiError(w, 502, err)
		return
	}
	// StdoutPipe 并不会写入 session.Stdout，所以不能拿 session.Stdout 给 Stderr 赋值：
	// 那样只是赋了一个 nil，随后被换成 io.Discard，远端 stderr 会被静默丢弃。
	stderr, err := session.StderrPipe()
	if err != nil {
		apiError(w, 502, err)
		return
	}
	if err = session.Shell(); err != nil {
		apiError(w, 502, err)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// coder/websocket 不允许并发 Write，而 stdout 与 stderr 由两个 goroutine 分别读取，
	// 这里用一个互斥写操作把它们串行化。
	var writeMu sync.Mutex
	write := func(data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.Write(ctx, websocket.MessageBinary, data)
	}
	forward := func(read io.Reader) {
		buf := make([]byte, 4096)
		for {
			n, err := read.Read(buf)
			if n > 0 {
				if write(buf[:n]) != nil {
					cancel()
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					writeMu.Lock()
					_ = conn.Close(websocket.StatusInternalError, err.Error())
					writeMu.Unlock()
				}
				cancel()
				return
			}
		}
	}
	go forward(stdout)
	go forward(stderr)
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg terminalMessage
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.Type {
		case "input":
			_, _ = io.WriteString(stdin, msg.Data)
		case "resize":
			if msg.Cols > 0 && msg.Rows > 0 {
				_ = session.WindowChange(msg.Rows, msg.Cols)
			}
		}
	}
}

package guestagent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
)

const (
	Version = "0.1.0"
	Port    = 1024
)

type helloRequest struct {
	Type string `json:"type"`
}

type helloResponse struct {
	OK       bool   `json:"ok"`
	Version  string `json:"version,omitempty"`
	OS       string `json:"os,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	Error    string `json:"error,omitempty"`
}

func Serve() error {
	return serveVsock(Port, handleConn)
}

func handleConn(rw io.ReadWriter) {
	reader := bufio.NewReader(rw)
	line, err := reader.ReadString('\n')
	if err != nil {
		writeResponse(rw, helloResponse{OK: false, Error: err.Error()})
		return
	}
	var req helloRequest
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		writeResponse(rw, helloResponse{OK: false, Error: "invalid request"})
		return
	}
	if strings.ToLower(req.Type) != "hello" {
		writeResponse(rw, helloResponse{OK: false, Error: "unsupported request"})
		return
	}
	hostname, _ := os.Hostname()
	writeResponse(rw, helloResponse{
		OK:       true,
		Version:  Version,
		OS:       runtime.GOOS,
		Hostname: hostname,
	})
}

func writeResponse(w io.Writer, resp helloResponse) {
	raw, err := json.Marshal(resp)
	if err != nil {
		_, _ = fmt.Fprintln(w, `{"ok":false,"error":"encode response"}`)
		return
	}
	_, _ = w.Write(append(raw, '\n'))
}

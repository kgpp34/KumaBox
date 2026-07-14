package guestagent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
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

type execRequest struct {
	Type    string   `json:"type"`
	Args    []string `json:"args"`
	Env     []string `json:"env,omitempty"`
	WorkDir string   `json:"workdir,omitempty"`
	Stdin   []byte   `json:"stdin,omitempty"`
}

type execResponse struct {
	OK       bool   `json:"ok"`
	ExitCode int    `json:"exitCode"`
	Stdout   []byte `json:"stdout,omitempty"`
	Stderr   []byte `json:"stderr,omitempty"`
	Error    string `json:"error,omitempty"`
}

type identityRequest struct {
	Type       string              `json:"type"`
	Hostname   string              `json:"hostname"`
	Interfaces []interfaceIdentity `json:"interfaces,omitempty"`
}

type interfaceIdentity struct {
	Name    string   `json:"name"`
	MAC     string   `json:"mac"`
	IP      string   `json:"ip,omitempty"`
	Prefix  int      `json:"prefix,omitempty"`
	Gateway string   `json:"gateway,omitempty"`
	DNS     []string `json:"dns,omitempty"`
}

type identityResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

var configureIdentity = applyIdentity

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
	switch strings.ToLower(req.Type) {
	case "hello":
		handleHello(rw)
	case "exec":
		handleExec(rw, []byte(line))
	case "identity":
		handleIdentity(rw, []byte(line))
	default:
		writeResponse(rw, helloResponse{OK: false, Error: "unsupported request"})
	}
}

func handleIdentity(w io.Writer, raw []byte) {
	var req identityRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeResponse(w, identityResponse{OK: false, Error: "invalid identity request"})
		return
	}
	if err := configureIdentity(req); err != nil {
		writeResponse(w, identityResponse{OK: false, Error: err.Error()})
		return
	}
	writeResponse(w, identityResponse{OK: true})
}

func handleHello(w io.Writer) {
	hostname, _ := os.Hostname()
	writeResponse(w, helloResponse{
		OK:       true,
		Version:  Version,
		OS:       runtime.GOOS,
		Hostname: hostname,
	})
}

func handleExec(w io.Writer, raw []byte) {
	var req execRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeResponse(w, execResponse{OK: false, ExitCode: 127, Error: "invalid exec request"})
		return
	}
	if len(req.Args) == 0 || req.Args[0] == "" {
		writeResponse(w, execResponse{OK: false, ExitCode: 127, Error: "exec args must not be empty"})
		return
	}

	cmd := exec.Command(req.Args[0], req.Args[1:]...) //nolint:gosec
	cmd.Dir = req.WorkDir
	cmd.Env = append(os.Environ(), req.Env...)
	cmd.Stdin = bytes.NewReader(req.Stdin)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	resp := execResponse{OK: true}
	if err := cmd.Run(); err != nil {
		resp.OK = false
		resp.Error = err.Error()
		if exitErr, ok := err.(*exec.ExitError); ok {
			resp.ExitCode = exitErr.ExitCode()
		} else {
			resp.ExitCode = 127
		}
	}
	resp.Stdout = stdout.Bytes()
	resp.Stderr = stderr.Bytes()
	writeResponse(w, resp)
}

func writeResponse(w io.Writer, resp any) {
	raw, err := json.Marshal(resp)
	if err != nil {
		_, _ = fmt.Fprintln(w, `{"ok":false,"error":"encode response"}`)
		return
	}
	_, _ = w.Write(append(raw, '\n'))
}

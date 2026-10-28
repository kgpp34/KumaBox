// node_probe is a static guest fixture for HTTP and TCP half-close verification.
package main

import (
	"io"
	"net"
	"net/http"
	"os"
)

func main() {
	listener, err := net.Listen("tcp4", "127.0.0.1:8081")
	if err != nil {
		os.Exit(1)
	}
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				data, err := io.ReadAll(io.LimitReader(connection, 1<<20))
				if err == nil {
					_, _ = connection.Write(append([]byte("reply:"), data...))
				}
			}()
		}
	}()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(w, io.LimitReader(r.Body, 1<<20)) })
	if err := http.ListenAndServe("127.0.0.1:8080", nil); err != nil {
		os.Exit(1)
	}
}

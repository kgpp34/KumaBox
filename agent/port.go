package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

const (
	portDialTimeout = 5 * time.Second
	// Bound a write stalled by a non-reading service/client. Idle connections
	// have no read deadline and remain usable for long-lived WebSockets.
	portWriteTimeout = 30 * time.Second
)

// ForwardPort connects a local TCP stream to a guest loopback port through an
// agent transport. After validation it owns both connections, including on
// cancellation. CloseWrite must be supported by local to preserve TCP half-close.
//
//	local TCP <-> bounded data/end frames over vsock <-> guest TCP loopback
//	           independent EOF in each direction -> done -> close both
func ForwardPort(ctx context.Context, connection io.ReadWriteCloser, port uint16, local net.Conn) error {
	if port == 0 || local == nil || connection == nil {
		return errors.New("port forwarding requires a guest port and two connections")
	}
	if _, ok := local.(interface{ CloseWrite() error }); !ok {
		return errors.New("port forwarding requires a half-close capable TCP connection")
	}
	closeIO := func() { _ = connection.Close(); _ = local.Close() }
	stop := context.AfterFunc(ctx, closeIO)
	defer stop()
	defer closeIO()
	if err := ctx.Err(); err != nil {
		return err
	}
	encoder, decoder := NewEncoder(connection), NewDecoder(connection)
	if err := encoder.Encode(Message{Type: MessagePortForward, Port: uint32(port)}); err != nil {
		return contextIOError(ctx, err)
	}
	ready, err := decoder.Decode()
	if err != nil {
		return contextIOError(ctx, err)
	}
	if ready.Type == MessageError {
		return &RemoteError{Code: ready.Code, Message: ready.Message}
	}
	if ready.Type != MessagePortReady {
		return fmt.Errorf("guest agent omitted port readiness: %q", ready.Type)
	}
	return relayPort(ctx, connection, decoder, encoder, local, portHost)
}

func servePort(ctx context.Context, connection net.Conn, decoder *Decoder, encoder *Encoder, port uint32) error {
	if port == 0 || port > 65535 {
		return encoder.Encode(Message{Type: MessageError, Code: "INVALID_ARGUMENT", Message: "guest TCP port must be between 1 and 65535"})
	}
	// The peer chooses only a port. Never accept a hostname, interface address
	// or URL that could turn the guest agent into an arbitrary network proxy.
	dialer := net.Dialer{Timeout: portDialTimeout}
	local, err := dialer.DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.FormatUint(uint64(port), 10)))
	if err != nil {
		return encoder.Encode(Message{Type: MessageError, Code: "UNAVAILABLE", Message: fmt.Sprintf("connect guest loopback port %d: %v", port, err)})
	}
	defer local.Close() //nolint:errcheck // also owns failed readiness handshakes
	if err := encoder.Encode(Message{Type: MessagePortReady}); err != nil {
		return err
	}
	return relayPort(ctx, connection, decoder, encoder, local, portGuest)
}

type portSide uint8

const (
	portHost portSide = iota
	portGuest
)

type portEvent struct {
	kind string
	err  error
}

// relayPort joins both pumps before returning. Receiving end closes only the
// TCP write half; its decoder stays alive to notice a disconnected peer while
// a service is still preparing a response. Guest sends done only after both
// halves finish, so a response produced after request EOF is never truncated.
func relayPort(ctx context.Context, connection io.ReadWriteCloser, decoder *Decoder, encoder *Encoder, local net.Conn, side portSide) error {
	closeIO := func() { _ = connection.Close(); _ = local.Close() }
	stop := context.AfterFunc(ctx, closeIO)
	defer stop()
	// At most four events are emitted by the two pumps, even when both fail
	// during shutdown. A returning coordinator never strands a channel sender.
	events := make(chan portEvent, 4)
	var pumps sync.WaitGroup
	pumps.Add(2)
	go func() {
		defer pumps.Done()
		events <- portEvent{kind: "sent_end", err: sendPortData(local, encoder)}
	}()
	go func() {
		defer pumps.Done()
		err := receivePortData(decoder, local, side, events)
		events <- portEvent{kind: "received_done", err: err}
	}()
	defer func() { closeIO(); pumps.Wait() }()

	var sentEnd, receivedEnd, done bool
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event := <-events:
			if err := ctx.Err(); err != nil {
				return err
			}
			if event.err != nil {
				return event.err
			}
			switch event.kind {
			case "sent_end":
				sentEnd = true
			case "received_end":
				receivedEnd = true
			case "received_done":
				done = true
			}
			if sentEnd && receivedEnd {
				if side == portGuest {
					return encoder.Encode(Message{Type: MessageDone})
				}
				if done {
					return nil
				}
			}
		}
	}
}

func sendPortData(local io.Reader, encoder *Encoder) error {
	buffer := make([]byte, streamChunkSize)
	for {
		n, err := local.Read(buffer)
		if n > 0 {
			if sendErr := encoder.Encode(Message{Type: MessagePortData, Data: buffer[:n]}); sendErr != nil {
				return sendErr
			}
		}
		if errors.Is(err, io.EOF) {
			return encoder.Encode(Message{Type: MessagePortEnd})
		}
		if err != nil {
			return err
		}
	}
}

func receivePortData(decoder *Decoder, local net.Conn, side portSide, events chan<- portEvent) error {
	ended := false
	for {
		message, err := decoder.Decode()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return io.ErrUnexpectedEOF
			}
			return err
		}
		switch message.Type {
		case MessagePortData:
			if ended || len(message.Data) > streamChunkSize {
				return errors.New("invalid port data frame")
			}
			// A transport disconnect cannot be decoded while this TCP write is
			// blocked. Bound the stall so it cannot strand both pumps forever.
			if err := local.SetWriteDeadline(time.Now().Add(portWriteTimeout)); err != nil {
				return err
			}
			n, err := local.Write(message.Data)
			if err != nil {
				return err
			}
			if n != len(message.Data) {
				return io.ErrShortWrite
			}
		case MessagePortEnd:
			if ended {
				return errors.New("duplicate port end frame")
			}
			writer, ok := local.(interface{ CloseWrite() error })
			if !ok {
				return errors.New("TCP connection does not support half-close")
			}
			if err := writer.CloseWrite(); err != nil {
				return err
			}
			ended = true
			events <- portEvent{kind: "received_end"}
		case MessageDone:
			if side != portHost || !ended {
				return errors.New("premature port completion")
			}
			return nil
		case MessageError:
			return &RemoteError{Code: message.Code, Message: message.Message}
		default:
			return fmt.Errorf("unexpected port frame %q", message.Type)
		}
	}
}

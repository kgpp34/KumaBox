package agent

import (
	"context"
	"errors"
	"net"
	"time"
)

func processFailure(encoder *Encoder, err error) {
	code := "INVALID_ARGUMENT"
	switch {
	case errors.Is(err, errProcessNotFound):
		code = "NOT_FOUND"
	case errors.Is(err, errProcessLimit):
		code = "RESOURCE_EXHAUSTED"
	case errors.Is(err, errProcessExited):
		code = "CONFLICT"
	}
	_ = encoder.Encode(Message{Type: MessageError, Code: code, Message: err.Error()})
}

func (s *Server) handleProcess(ctx context.Context, connection net.Conn, decoder *Decoder, encoder *Encoder, request Message) {
	if s.processes == nil {
		_ = encoder.sendError("background process management is unavailable")
		return
	}
	switch request.Type {
	case MessageSpawn:
		info, err := s.processes.spawn(request)
		if err != nil {
			processFailure(encoder, err)
			return
		}
		// If this response is lost the process remains owned by the agent and
		// discoverable through list; disconnect never rolls back a started job.
		_ = encoder.Encode(Message{Type: MessageDone, Process: &info})
	case MessageProcesses:
		for _, info := range s.processes.list() {
			if err := encoder.Encode(Message{Type: MessageProcess, Process: &info}); err != nil {
				return
			}
		}
		_ = encoder.Encode(Message{Type: MessageDone})
	default:
		job, err := s.processes.lookup(request.ProcessID)
		if err != nil {
			processFailure(encoder, err)
			return
		}
		if request.Type == MessageSignal {
			if err := job.signal(request.Signal); err != nil {
				processFailure(encoder, err)
				return
			}
			_ = encoder.Encode(Message{Type: MessageDone})
			return
		}
		if err := streamProcess(ctx, connection, decoder, encoder, job, request); err != nil && ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, net.ErrClosed) {
			s.logger.Printf("stream background process: %v", err)
		}
	}
}

// streamProcess closes its observer on disconnect without signaling the job.
// Logs stop at their initial high-water mark; attach replays then follows live
// output until exit. Both expose cursor gaps rather than silently losing bytes.
func streamProcess(parent context.Context, connection net.Conn, decoder *Decoder, encoder *Encoder, job *managedProcess, request Message) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		_, _ = decoder.Decode() // output-only: EOF or any input detaches the observer
		cancel()
	}()
	defer func() {
		cancel()
		_ = connection.SetReadDeadline(time.Now())
		<-watchDone
	}()
	stop := context.AfterFunc(ctx, func() { _ = connection.SetWriteDeadline(time.Now()) })
	defer stop()
	after := request.Cursor
	frames, info, highWater, changed := job.output(after)
	if after > highWater {
		processFailure(encoder, errors.New("output cursor exceeds the process's latest frame"))
		return nil
	}
	for {
		if len(frames) > 0 && frames[0].Cursor > after+1 {
			after = frames[0].Cursor - 1
			if err := encoder.Encode(Message{Type: MessageGap, Cursor: after}); err != nil {
				return err
			}
		}
		for _, frame := range frames {
			if err := encoder.Encode(frame); err != nil {
				return err
			}
			after = frame.Cursor
		}
		if request.Type == MessageLogs || !info.Running {
			return encoder.Encode(Message{Type: MessageDone, Cursor: after, Process: &info})
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
			frames, info, _, changed = job.output(after)
		}
	}
}

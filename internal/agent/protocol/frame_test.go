package protocol

import (
	"bytes"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	t.Parallel()

	want := Frame{
		Version: VersionV1,
		Type:    FrameExec,
		ID:      "exec-1",
		Args:    []string{"sh", "-c", "echo ok"},
		Env:     map[string]string{"FOO": "bar"},
		WorkDir: "/tmp",
		User:    "agent",
		TTY:     true,
	}
	var buf bytes.Buffer
	if err := WriteFrame(&buf, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != want.Version || got.Type != want.Type || got.ID != want.ID || got.User != want.User || !got.TTY {
		t.Fatalf("frame = %+v, want %+v", got, want)
	}
	if got.Env["FOO"] != "bar" || len(got.Args) != 3 {
		t.Fatalf("frame payload = %+v", got)
	}
}

func TestFrameRoundTripPreservesStdinEnd(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	want := Frame{Version: VersionV1, Type: FrameStdin, ID: "exec-1", Stream: StreamStdin, End: true}
	if err := WriteFrame(&buf, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !got.End || got.Stream != StreamStdin {
		t.Fatalf("frame = %+v", got)
	}
}

func TestFrameValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		frame Frame
		want  ErrorCode
	}{
		{name: "version", frame: Frame{Version: "v0", Type: FrameHello}, want: ErrorUnsupportedVersion},
		{name: "type", frame: Frame{Version: VersionV1, Type: "wat"}, want: ErrorUnsupportedFrame},
		{name: "id", frame: Frame{Version: VersionV1, Type: FrameExec}, want: ErrorInvalidFrame},
		{name: "args", frame: Frame{Version: VersionV1, Type: FrameExec, ID: "1"}, want: ErrorInvalidRequest},
		{name: "resize", frame: Frame{Version: VersionV1, Type: FrameResize, ID: "1"}, want: ErrorInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.frame.Validate()
			if err == nil || !strings.Contains(err.Error(), string(tt.want)) {
				t.Fatalf("Validate() error = %v, want %s", err, tt.want)
			}
		})
	}
}

func TestReadFrameRejectsOversizedFrame(t *testing.T) {
	t.Parallel()

	input := strings.Repeat("x", MaxFrameBytes) + "\n"
	_, err := ReadFrame(strings.NewReader(input))
	if err == nil || !strings.Contains(err.Error(), string(ErrorInvalidFrame)) {
		t.Fatalf("ReadFrame() error = %v, want invalid frame", err)
	}
}

func TestWriteFrameHandlesShortWriter(t *testing.T) {
	t.Parallel()

	var buf shortWriter
	err := WriteFrame(&buf, Frame{Version: VersionV1, Type: FrameHello})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Fatalf("encoded frame = %q", buf.String())
	}
}

func TestDecoderPreservesFollowingFrame(t *testing.T) {
	t.Parallel()

	input := `{"version":"kumabox.agent.v1","type":"hello"}
{"version":"kumabox.agent.v1","type":"ready","id":"1"}
`
	decoder := NewDecoder(strings.NewReader(input))
	first, err := decoder.ReadFrame()
	if err != nil || first.Type != FrameHello {
		t.Fatalf("first frame = %+v, error = %v", first, err)
	}
	second, err := decoder.ReadFrame()
	if err != nil || second.Type != FrameReady || second.ID != "1" {
		t.Fatalf("second frame = %+v, error = %v", second, err)
	}
}

type shortWriter struct {
	data []byte
}

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.data = append(w.data, p[0])
	return 1, nil
}

func (w *shortWriter) String() string { return string(w.data) }

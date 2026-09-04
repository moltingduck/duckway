package bridge

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

type shortWriter struct{ bytes.Buffer }

func (w *shortWriter) Write(data []byte) (int, error) {
	if len(data) > 2 {
		data = data[:2]
	}
	return w.Buffer.Write(data)
}

func TestCodecRoundTripWithShortWrites(t *testing.T) {
	w := &shortWriter{}
	encoder := NewCodec(nil, w, 1024)
	want := map[string]any{"type": "hello", "value": float64(7)}
	if err := encoder.Write(want); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := NewCodec(bytes.NewReader(w.Bytes()), nil, 1024).Read(&got); err != nil {
		t.Fatal(err)
	}
	if got["type"] != want["type"] || got["value"] != want["value"] {
		t.Fatalf("got=%v", got)
	}
}

func TestCodecRejectsOversizedBeforeAllocation(t *testing.T) {
	var data [4]byte
	binary.BigEndian.PutUint32(data[:], 1025)
	err := NewCodec(bytes.NewReader(data[:]), nil, 1024).Read(&struct{}{})
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("error=%v", err)
	}
	if err := NewCodec(nil, &bytes.Buffer{}, 4).Write(map[string]string{"long": "payload"}); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("write error=%v", err)
	}
}

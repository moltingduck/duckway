package bridge

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const DefaultMaxFrame = 1 << 20

var ErrFrameTooLarge = errors.New("frame too large")

type Codec struct {
	reader   io.Reader
	writer   io.Writer
	maxFrame uint32
}

func NewCodec(reader io.Reader, writer io.Writer, maxFrame uint32) *Codec {
	if maxFrame == 0 {
		maxFrame = DefaultMaxFrame
	}
	return &Codec{reader: reader, writer: writer, maxFrame: maxFrame}
}

func (c *Codec) Read(value any) error {
	var header [4]byte
	if _, err := io.ReadFull(c.reader, header[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 {
		return fmt.Errorf("empty frame")
	}
	if size > c.maxFrame {
		return ErrFrameTooLarge
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		return err
	}
	if err := json.Unmarshal(payload, value); err != nil {
		return fmt.Errorf("decode frame: %w", err)
	}
	return nil
}

func (c *Codec) Write(value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode frame: %w", err)
	}
	if len(payload) == 0 || uint64(len(payload)) > uint64(c.maxFrame) {
		return ErrFrameTooLarge
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if err := writeFull(c.writer, header[:]); err != nil {
		return err
	}
	return writeFull(c.writer, payload)
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

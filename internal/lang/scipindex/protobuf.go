package scipindex

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

const (
	maximumDocumentBytes = int64(64 << 20)
	maximumStringBytes   = int64(4 << 20)
	maximumMessageBytes  = int64(16 << 20)
	maximumRangeValues   = 4
)

type wireReader struct {
	reader    io.Reader
	remaining int64
}

func newWireReader(reader io.Reader, size int64) *wireReader {
	return &wireReader{reader: reader, remaining: size}
}

func newMessageReader(data []byte) *wireReader {
	return newWireReader(bytes.NewReader(data), int64(len(data)))
}

func (reader *wireReader) next() (field int, wire int, done bool, err error) {
	if reader.remaining == 0 {
		return 0, 0, true, nil
	}
	key, err := reader.varint()
	if err != nil {
		return 0, 0, false, err
	}
	if key == 0 {
		return 0, 0, false, errors.New("protobuf field key is zero")
	}
	field = int(key >> 3)
	wire = int(key & 7)
	if field <= 0 {
		return 0, 0, false, errors.New("protobuf field number is invalid")
	}
	return field, wire, false, nil
}

func (reader *wireReader) varint() (uint64, error) {
	var value uint64
	for shift := uint(0); shift < 70; shift += 7 {
		if reader.remaining <= 0 {
			return 0, io.ErrUnexpectedEOF
		}
		var one [1]byte
		if _, err := io.ReadFull(reader.reader, one[:]); err != nil {
			return 0, err
		}
		reader.remaining--
		if shift == 63 && one[0] > 1 {
			return 0, errors.New("protobuf varint overflows uint64")
		}
		value |= uint64(one[0]&0x7f) << shift
		if one[0]&0x80 == 0 {
			return value, nil
		}
	}
	return 0, errors.New("protobuf varint exceeds ten bytes")
}

func (reader *wireReader) bytes(limit int64) ([]byte, error) {
	length, err := reader.varint()
	if err != nil {
		return nil, err
	}
	if length > uint64(limit) {
		return nil, fmt.Errorf("protobuf field is %d bytes; maximum is %d", length, limit)
	}
	if length > uint64(reader.remaining) {
		return nil, io.ErrUnexpectedEOF
	}
	data := make([]byte, int(length))
	if _, err := io.ReadFull(reader.reader, data); err != nil {
		return nil, err
	}
	reader.remaining -= int64(length)
	return data, nil
}

func (reader *wireReader) string() (string, error) {
	data, err := reader.bytes(maximumStringBytes)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) {
		return "", errors.New("protobuf string is not valid UTF-8")
	}
	return string(data), nil
}

func (reader *wireReader) skip(wire int) error {
	switch wire {
	case 0:
		_, err := reader.varint()
		return err
	case 1:
		return reader.discard(8)
	case 2:
		length, err := reader.varint()
		if err != nil {
			return err
		}
		if length > uint64(reader.remaining) {
			return io.ErrUnexpectedEOF
		}
		return reader.discard(int64(length))
	case 5:
		return reader.discard(4)
	case 3, 4:
		return errors.New("protobuf groups are not supported")
	default:
		return fmt.Errorf("unsupported protobuf wire type %d", wire)
	}
}

func (reader *wireReader) discard(count int64) error {
	if count < 0 || count > reader.remaining {
		return io.ErrUnexpectedEOF
	}
	if _, err := io.CopyN(io.Discard, reader.reader, count); err != nil {
		return err
	}
	reader.remaining -= count
	return nil
}

func requireWire(field, got, want int) error {
	if got != want {
		return fmt.Errorf("protobuf field %d uses wire type %d; want %d", field, got, want)
	}
	return nil
}

func parsePackedInt32(data []byte) ([]int32, error) {
	reader := newMessageReader(data)
	values := make([]int32, 0, maximumRangeValues)
	for reader.remaining > 0 {
		value, err := reader.varint()
		if err != nil {
			return nil, err
		}
		if len(values) >= maximumRangeValues {
			return nil, errors.New("SCIP range has more than four values")
		}
		values = append(values, int32(value))
	}
	return values, nil
}

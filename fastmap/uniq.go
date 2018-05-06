package fastmap

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

type Uniq struct {
	fm               *Writer
	indices          io.Writer
	keyLen, valueLen int
	offsetLen        int
	fmRecord         []byte
	prevKey          []byte
	offsetBytes      []byte
	fullOffsetBytes  []byte
	values           []byte
	lenBuf           []byte
	offset           uint64

	// TODO should write varints to indices
}

func NewUniq(fm *Writer, indices io.Writer, keyLen, valueLen, offsetLen int) (*Uniq, error) {
	fmRecord := make([]byte, keyLen+valueLen)
	prevKey := fmRecord[:keyLen]
	offsetBytes := fmRecord[keyLen:]
	lenBuf := make([]byte, binary.MaxVarintLen64)
	fullOffsetBytes := make([]byte, 8)
	return &Uniq{
		fm:              fm,
		indices:         indices,
		keyLen:          keyLen,
		valueLen:        valueLen,
		offsetLen:       offsetLen,
		fmRecord:        fmRecord,
		prevKey:         prevKey,
		offsetBytes:     offsetBytes,
		fullOffsetBytes: fullOffsetBytes,
		lenBuf:          lenBuf,
	}, nil
}

func (u *Uniq) dump() error {
	if _, err := u.fm.Write(u.fmRecord); err != nil {
		return err
	}
	l := binary.PutUvarint(u.lenBuf, uint64(len(u.values)/4))
	if n, err := u.indices.Write(u.lenBuf[:l]); err != nil {
		return err
	} else if n != l {
		return io.ErrShortWrite
	}
	if n, err := u.indices.Write(u.values); err != nil {
		return err
	} else if n != len(u.values) {
		return io.ErrShortWrite
	}
	u.offset += uint64(l + len(u.values))
	binary.LittleEndian.PutUint64(u.fullOffsetBytes, u.offset)
	copy(u.offsetBytes, u.fullOffsetBytes)
	u.values = u.values[:0]
	return nil
}

func (u *Uniq) Write(b []byte) (int, error) {
	if len(b) != u.keyLen+u.valueLen {
		return 0, fmt.Errorf("Wrong record len")
	}
	key := b[:u.keyLen]
	value := b[u.keyLen:]
	if len(u.values) == 0 {
		// First record.
		copy(u.prevKey, key)
	} else {
		if bytes.Equal(key, u.prevKey) {
			if bytes.Equal(value, u.values[len(u.values)-u.valueLen:]) {
				// Repeated value - skip.
				return len(b), nil
			}
		} else {
			if err := u.dump(); err != nil {
				return 0, err
			}
			copy(u.prevKey, key)
		}
	}
	u.values = append(u.values, value...)
	return len(b), nil
}

func (u *Uniq) Close() error {
	if err := u.dump(); err != nil {
		return err
	}
	if err := u.fm.Close(); err != nil {
		return err
	}
	if c, ok := u.indices.(io.Closer); ok {
		if err := c.Close(); err != nil {
			return err
		}
	}
	return nil
}

type UniqMap struct {
	fm *Map

	values   []byte
	valueLen int
}

func OpenUniq(pageLen, keyLen, valueLen, offsetLen int, data, prefixes, values []byte) (*UniqMap, error) {
	fm, err := Open(pageLen, keyLen, offsetLen, data, prefixes)
	if err != nil {
		return nil, err
	}
	return &UniqMap{
		fm:       fm,
		values:   values,
		valueLen: valueLen,
	}, nil
}

func (u *UniqMap) Lookup(key []byte) ([]byte, error) {
	offsetBytes, err := u.fm.Lookup(key)
	if err != nil || offsetBytes == nil {
		return nil, err
	}
	var fullOffset [8]byte
	fullOffsetBytes := fullOffset[:]
	copy(fullOffsetBytes, offsetBytes)
	lenPos := int(binary.LittleEndian.Uint64(fullOffsetBytes))
	size0, l := binary.Uvarint(u.values[lenPos:])
	if l <= 0 {
		return nil, fmt.Errorf("Error in database: bad varint at lenPos")
	}
	dataStart := lenPos + l
	dataEnd := dataStart + int(size0)*u.valueLen
	if dataEnd > len(u.values) {
		return nil, fmt.Errorf("Error in database: too large size")
	}
	return u.values[dataStart:dataEnd], nil
}

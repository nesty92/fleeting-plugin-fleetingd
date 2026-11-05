package fleetingd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// qcow2Backing returns the backing file a qcow2 image names in its header,
// or "" when it has none.
func qcow2Backing(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var h [20]byte
	if _, err := io.ReadFull(f, h[:]); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	if string(h[:4]) != "QFI\xfb" {
		return "", fmt.Errorf("%s: not a qcow2 image", path)
	}
	off, size := binary.BigEndian.Uint64(h[8:16]), binary.BigEndian.Uint32(h[16:20])
	if off == 0 || size == 0 {
		return "", nil
	}
	if size > 1023 { // qemu's own limit
		return "", errors.New(path + ": backing file name too long")
	}
	name := make([]byte, size)
	if _, err := f.ReadAt(name, int64(off)); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return string(name), nil
}

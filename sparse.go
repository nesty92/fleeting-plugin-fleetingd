package fleetingd

import (
	"bytes"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// sparseBlock is the unit punchZeros looks at: a guest page.
const sparseBlock = 4096

// punchZeros turns runs of zero blocks in a file into holes, keeping its
// size. A snapshot's memory file is the guest's whole RAM, most of it never
// touched: 6 GB become under 1 GB on disk. A restore reads holes as zeros.
// It returns the bytes the file still occupies.
func punchZeros(path string) (int64, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := fi.Size()
	zero := make([]byte, sparseBlock)
	buf := make([]byte, 1<<20)
	var runStart, runLen int64
	flush := func() error {
		if runLen == 0 {
			return nil
		}
		err := unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, runStart, runLen)
		runLen = 0
		return err
	}

	for off := int64(0); off < size; {
		// Skip what already is a hole.
		data, err := unix.Seek(int(f.Fd()), off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break
		}
		if err != nil {
			return 0, err
		}
		if data != off {
			if err := flush(); err != nil {
				return 0, err
			}
			off = data
		}
		n, err := f.ReadAt(buf, off)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if n == 0 {
			break
		}
		for b := 0; b < n; b += sparseBlock {
			end := min(b+sparseBlock, n)
			if bytes.Equal(buf[b:end], zero[:end-b]) {
				if runLen == 0 {
					runStart = off + int64(b)
				}
				runLen += int64(end - b)
				continue
			}
			if err := flush(); err != nil {
				return 0, err
			}
		}
		off += int64(n)
	}
	if err := flush(); err != nil {
		return 0, err
	}
	if err := f.Sync(); err != nil {
		return 0, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return 0, err
	}
	return st.Blocks * 512, nil
}

package fleetingd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPunchZeros(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory")
	data := make([]byte, 64<<20)
	copy(data[5<<20:], []byte("guest page"))
	data[len(data)-1] = 1
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	used, err := punchZeros(path)
	if err != nil {
		t.Skipf("no hole punching here: %v", err)
	}
	if used > 1<<20 {
		t.Errorf("still uses %d bytes, want a few blocks", used)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(data) || string(got[5<<20:5<<20+10]) != "guest page" || got[len(got)-1] != 1 {
		t.Fatal("contents changed")
	}
	for i, b := range got {
		if b != data[i] {
			t.Fatalf("byte %d changed", i)
		}
	}
}

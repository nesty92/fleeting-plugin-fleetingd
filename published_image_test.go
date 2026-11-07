package fleetingd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/go-hclog"
)

func sumOf(b string) string {
	h := sha256.Sum256([]byte(b))
	return hex.EncodeToString(h[:])
}

func TestParseImageSums(t *testing.T) {
	base, _ := url.Parse("https://example.com/releases/download/small/SHA256SUMS")
	a, k := strings.Repeat("a", 64), strings.Repeat("b", 64)

	p, err := parseImageSums(base, a+" *small-amd64.qcow2\n"+k+"  https://cdn.example.com/k/small-amd64.vmlinuz\n"+strings.Repeat("c", 64)+" notes.txt\n")
	if err != nil {
		t.Fatal(err)
	}
	if p.image != "https://example.com/releases/download/small/small-amd64.qcow2" || p.imageSum != a ||
		p.kernel != "https://cdn.example.com/k/small-amd64.vmlinuz" || p.kernelSum != k {
		t.Errorf("parsed %+v", p)
	}

	for name, body := range map[string]string{
		"no kernel":  a + " x.qcow2\n",
		"two images": a + " x.qcow2\n" + a + " y.qcow2\n" + k + " x.vmlinuz\n",
		"short sum":  "abc x.qcow2\n" + k + " x.vmlinuz\n",
		"not hex":    strings.Repeat("z", 64) + " x.qcow2\n" + k + " x.vmlinuz\n",
		"no name":    a + "\n",
	} {
		if _, err := parseImageSums(base, body); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestValidateImageURL(t *testing.T) {
	for url, ok := range map[string]bool{
		"https://github.com/o/r/releases/download/t/SHA256SUMS": true,
		"ftp://example.com/SHA256SUMS":                          false,
		"/local/SHA256SUMS":                                     false,
	} {
		g := &InstanceGroup{VMImageURL: url}
		if err := g.validate(); (err == nil) != ok {
			t.Errorf("%s: %v", url, err)
		}
	}
	g := &InstanceGroup{VMImageURL: "https://example.com/SHA256SUMS", VMPrebuildCloudinitExtraCmds: []string{"true"}}
	if err := g.validate(); err == nil {
		t.Error("vm_image_url with commands: no error")
	}
}

// fakeQemuImg puts a qemu-img on the PATH whose convert copies its input.
func fakeQemuImg(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\nfor a; do src=$dst; dst=$a; done\ncp \"$src\" \"$dst\"\n"
	if err := os.WriteFile(filepath.Join(bin, "qemu-img"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestPublishedImage(t *testing.T) {
	fakeQemuImg(t)
	image, kernel := "image bytes", "kernel bytes"
	sums := fmt.Sprintf("%s *small-%s.qcow2\n%s *small-%[2]s.vmlinuz\n", sumOf(image), runtime.GOARCH, sumOf(kernel))
	var downloads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "SHA256SUMS":
			fmt.Fprint(w, sums)
		case "small-" + runtime.GOARCH + ".qcow2":
			downloads.Add(1)
			fmt.Fprint(w, image)
		case "small-" + runtime.GOARCH + ".vmlinuz":
			fmt.Fprint(w, kernel)
		default:
			http.NotFound(w, r)
		}
	}))
	dir := t.TempDir()
	g := &InstanceGroup{VMImageDir: dir, VMImageURL: srv.URL + "/{arch}/SHA256SUMS", logger: hclog.NewNullLogger()}

	got, gotKernel, err := g.ensurePublishedImage()
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(got); string(b) != image {
		t.Errorf("image holds %q", b)
	}
	if b, _ := os.ReadFile(gotKernel); string(b) != kernel {
		t.Errorf("kernel holds %q", b)
	}
	if !strings.HasPrefix(filepath.Base(got), "prebuilt-") {
		t.Errorf("image named %s", got)
	}

	// Again: reused, not downloaded.
	if again, _, err := g.ensurePublishedImage(); err != nil || again != got || downloads.Load() != 1 {
		t.Errorf("second start: %s, %v, %d downloads", again, err, downloads.Load())
	}

	// The list can't be read: the image it named last time.
	srv.Close()
	if offline, _, err := g.ensurePublishedImage(); err != nil || offline != got {
		t.Errorf("offline: %s, %v", offline, err)
	}

	// A new image whose download doesn't match its sum: an error, and
	// nothing half-written left behind.
	sums = fmt.Sprintf("%s *small-%s.qcow2\n%s *small-%[2]s.vmlinuz\n", sumOf("newer image"), runtime.GOARCH, sumOf(kernel))
	srv = httptest.NewServer(srv.Config.Handler)
	defer srv.Close()
	g.VMImageURL = srv.URL + "/{arch}/SHA256SUMS"
	if _, _, err := g.ensurePublishedImage(); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Errorf("bad download: %v", err)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*.download"))
	tmps, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(left)+len(tmps) > 0 {
		t.Errorf("left behind: %v %v", left, tmps)
	}
}

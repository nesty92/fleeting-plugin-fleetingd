package fleetingd

// A published prebuilt image, used instead of building one on the host.
//
// vm_image_url names a SHA256SUMS file, as sha256sum writes it, listing one
// .qcow2 and one .vmlinuz: the image (compressed qcow2 is fine) and the
// kernel whose modules are in it. Names are relative to the SHA256SUMS URL,
// or absolute URLs, so a list that stays at one URL can point at images
// published elsewhere. build-image writes such a directory.
//
// On each start the list is read again. An image it names that is not on
// the host yet is downloaded, checked against its sum and decompressed
// into vm_image_directory as prebuilt-<key>.qcow2, like a local build, so
// running VMs keep the image they have and the old one goes once unused.
// When the list can't be read, the image it named last time is used.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// publishedImage is what a SHA256SUMS lists: where the image and kernel
// are, and their sums.
type publishedImage struct {
	image, imageSum   string
	kernel, kernelSum string
}

// key names the image on the host. Its own prefix keeps it apart from
// the keys of local builds.
func (p publishedImage) key() string {
	h := sha256.Sum256([]byte("published\n" + p.imageSum + "\n" + p.kernelSum + "\n"))
	return hex.EncodeToString(h[:])[:16]
}

// imageURL is vm_image_url for this host's architecture.
func (i *InstanceGroup) imageURL() string {
	return strings.ReplaceAll(i.VMImageURL, "{arch}", runtime.GOARCH)
}

// parseImageSums reads a SHA256SUMS fetched from base.
func parseImageSums(base *url.URL, body string) (publishedImage, error) {
	var p publishedImage
	for line := range strings.Lines(body) {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		if len(f) != 2 || len(f[0]) != 64 {
			return p, fmt.Errorf("SHA256SUMS: want a sum and a name, got %q", strings.TrimSpace(line))
		}
		if _, err := hex.DecodeString(f[0]); err != nil {
			return p, fmt.Errorf("SHA256SUMS: %q is not a sha256 sum", f[0])
		}
		ref, err := url.Parse(strings.TrimPrefix(f[1], "*"))
		if err != nil {
			return p, fmt.Errorf("SHA256SUMS: %w", err)
		}
		where := base.ResolveReference(ref).String()
		var dst, sum *string
		switch {
		case strings.HasSuffix(ref.Path, ".qcow2"):
			dst, sum = &p.image, &p.imageSum
		case strings.HasSuffix(ref.Path, ".vmlinuz"):
			dst, sum = &p.kernel, &p.kernelSum
		default:
			continue
		}
		if *dst != "" {
			return p, fmt.Errorf("SHA256SUMS lists more than one %s", filepath.Ext(ref.Path))
		}
		*dst, *sum = where, strings.ToLower(f[0])
	}
	if p.image == "" || p.kernel == "" {
		return p, errors.New("SHA256SUMS must list a .qcow2 and a .vmlinuz")
	}
	return p, nil
}

// lastPublished is the file holding the key of the image vm_image_url
// named last time.
func (i *InstanceGroup) lastPublished() string {
	h := sha256.Sum256([]byte(i.imageURL()))
	return filepath.Join(i.imageDir(), "published-"+hex.EncodeToString(h[:])[:16])
}

// ensurePublishedImage makes sure the image vm_image_url names is on the
// host. Called with the directory's build lock held.
func (i *InstanceGroup) ensurePublishedImage() (image, kernel string, err error) {
	paths := func(key string) (string, string) {
		return filepath.Join(i.imageDir(), "prebuilt-"+key+".qcow2"), filepath.Join(i.imageDir(), "prebuilt-"+key+".vmlinuz")
	}
	p, err := fetchImageSums(i.imageURL())
	if err != nil {
		// Offline, or the list moved: the image used last time, if any.
		if raw, rerr := os.ReadFile(i.lastPublished()); rerr == nil {
			image, kernel = paths(strings.TrimSpace(string(raw)))
			if ok, _ := checkFileExists(image); ok {
				i.logger.Warn("reading vm_image_url failed; using the image it named last time", "err", err, "image", filepath.Base(image))
				return image, kernel, nil
			}
		}
		return "", "", fmt.Errorf("vm_image_url: %w", err)
	}
	key := p.key()
	image, kernel = paths(key)
	if ok, _ := checkFileExists(image); ok {
		i.logger.Info("reusing the published image", "image", filepath.Base(image))
	} else if err := i.downloadPublished(p, image, kernel); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(i.lastPublished(), []byte(key+"\n"), 0o600); err != nil {
		return "", "", err
	}
	return image, kernel, nil
}

// downloadPublished fetches the image and its kernel, checks both and
// moves them into place only once they are whole.
func (i *InstanceGroup) downloadPublished(p publishedImage, image, kernel string) error {
	download, tmp, tmpKernel := image+".download", image+".tmp", kernel+".tmp"
	defer os.Remove(download)
	defer os.Remove(tmp)
	defer os.Remove(tmpKernel)

	i.logger.Info("downloading the published image", "url", p.image)
	if err := fetchChecked(p.image, p.imageSum, download); err != nil {
		return fmt.Errorf("vm_image_url: the image: %w", err)
	}
	if err := fetchChecked(p.kernel, p.kernelSum, tmpKernel); err != nil {
		return fmt.Errorf("vm_image_url: the kernel: %w", err)
	}
	// cloud-hypervisor can't read compressed qcow2.
	if out, err := exec.Command("qemu-img", "convert", "-f", "qcow2", "-O", "qcow2", download, tmp).CombinedOutput(); err != nil {
		return fmt.Errorf("decompressing the published image: %w: %s", err, out)
	}
	if err := os.Rename(tmpKernel, kernel); err != nil {
		return err
	}
	if err := os.Rename(tmp, image); err != nil {
		return err
	}
	i.logger.Info("published image ready", "image", filepath.Base(image))
	return nil
}

var publishedClient = &http.Client{Timeout: time.Hour}

// get is a GET that fails on any answer but 200.
func get(u string) (*http.Response, error) {
	resp, err := publishedClient.Get(u)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return resp, nil
}

func fetchImageSums(u string) (publishedImage, error) {
	base, err := url.Parse(u)
	if err != nil {
		return publishedImage{}, err
	}
	resp, err := get(u)
	if err != nil {
		return publishedImage{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return publishedImage{}, err
	}
	// Relative names resolve against vm_image_url, not where a redirect
	// led: GitHub's release downloads redirect to signed, one-file URLs.
	return parseImageSums(base, string(body))
}

// fetchChecked downloads u to path and checks its sha256.
func fetchChecked(u, sum, path string) error {
	resp, err := get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return fmt.Errorf("%s: sha256 %s, SHA256SUMS says %s", u, got, sum)
	}
	return nil
}

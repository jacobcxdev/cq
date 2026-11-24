package dots

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func writeLauncherIconPNG(t *testing.T, path string, size int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	img.SetRGBA(0, 0, color.RGBA{R: 32, G: 64, B: 96, A: 255})
	var data bytes.Buffer
	if err := png.Encode(&data, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestWindowsLauncherIconPreservesNativeSizeVariants(t *testing.T) {
	dir := t.TempDir()
	logo := filepath.Join(dir, "Square44x44Logo.png")
	writeLauncherIconPNG(t, logo, 44)
	small := writeLauncherIconPNG(t, filepath.Join(dir, "Square44x44Logo.targetsize-16_altform-unplated.png"), 16)
	large := writeLauncherIconPNG(t, filepath.Join(dir, "Square44x44Logo.targetsize-256_altform-unplated.png"), 256)
	icon, err := windowsLauncherIcon(logo, os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	checkLauncherIcon(t, icon, []byte{16, 0}, [][]byte{small, large})
}

func TestWindowsLauncherIconAcceptsManifestImageWithoutVariants(t *testing.T) {
	logo := filepath.Join(t.TempDir(), "AppLogo.png")
	data := writeLauncherIconPNG(t, logo, 44)
	icon, err := windowsLauncherIcon(logo, os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	checkLauncherIcon(t, icon, []byte{44}, [][]byte{data})
	if err := os.WriteFile(logo, []byte("invalid PNG"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := windowsLauncherIcon(logo, os.ReadFile); err == nil {
		t.Fatal("invalid package image accepted")
	}
}

func checkLauncherIcon(t *testing.T, icon, sizes []byte, pngs [][]byte) {
	t.Helper()
	if len(icon) < 6+16*len(sizes) || !bytes.Equal(icon[:4], []byte{0, 0, 1, 0}) || int(binary.LittleEndian.Uint16(icon[4:6])) != len(sizes) {
		t.Fatal("invalid shortcut icon directory")
	}
	for i, size := range sizes {
		entry := icon[6+16*i:][:16]
		length := int(binary.LittleEndian.Uint32(entry[8:12]))
		offset := int(binary.LittleEndian.Uint32(entry[12:16]))
		if entry[0] != size || entry[1] != size || offset < 0 || offset > len(icon) || length != len(pngs[i]) || length > len(icon)-offset || !bytes.Equal(icon[offset:offset+length], pngs[i]) {
			t.Fatalf("icon image %d differs from the package image", i)
		}
	}
}

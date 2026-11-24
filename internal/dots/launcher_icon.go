package dots

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image/png"
	"io/fs"
	"path/filepath"
	"strings"
)

// Windows shortcuts need an ICO file. Embed the package's original PNGs so
// the shortcut uses the same artwork and size variants as its Start entry.
func windowsLauncherIcon(logo string, read func(string) ([]byte, error)) ([]byte, error) {
	type image struct {
		png  []byte
		size int
	}
	var images []image
	ext := filepath.Ext(logo)
	stem := strings.TrimSuffix(logo, ext)
	for _, size := range []int{16, 24, 32, 48, 64, 256} {
		data, err := read(fmt.Sprintf("%s.targetsize-%d_altform-unplated%s", stem, size, ext))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		config, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil || config.Width != size || config.Height != size {
			return nil, errors.New("invalid Windows package logo size variant")
		}
		images = append(images, image{png: data, size: size})
	}
	if len(images) == 0 {
		data, err := read(logo)
		if err != nil {
			return nil, err
		}
		config, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil || config.Width < 1 || config.Width > 256 || config.Height != config.Width {
			return nil, errors.New("invalid Windows package logo")
		}
		images = append(images, image{png: data, size: config.Width})
	}
	const headerSize, entrySize = 6, 16
	icon := make([]byte, headerSize+entrySize*len(images))
	binary.LittleEndian.PutUint16(icon[2:4], 1) // ICO, not a cursor.
	binary.LittleEndian.PutUint16(icon[4:6], uint16(len(images)))
	for index, image := range images {
		entry := icon[headerSize+entrySize*index:][:entrySize]
		entry[0], entry[1] = byte(image.size), byte(image.size) // 0 encodes 256.
		binary.LittleEndian.PutUint16(entry[4:6], 1)
		binary.LittleEndian.PutUint16(entry[6:8], 32)
		binary.LittleEndian.PutUint32(entry[8:12], uint32(len(image.png)))
		binary.LittleEndian.PutUint32(entry[12:16], uint32(len(icon)))
		icon = append(icon, image.png...)
	}
	return icon, nil
}

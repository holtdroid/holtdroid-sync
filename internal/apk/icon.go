package apk

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"io"
	"path"
	"slices"
	"strings"

	"github.com/shogo82148/androidbinary"
	androidapk "github.com/shogo82148/androidbinary/apk"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	IconSize     = 256
	MaxIconBytes = 4 << 20
	maxIconSide  = 4096
	maxXMLBytes  = 1 << 20
	maxTable     = 64 << 20
)

// NormalizeIcon decodes an untrusted PNG, JPEG or WebP and re-renders it as
// an IconSize PNG, so nothing but pixels survives.
func NormalizeIcon(raw []byte) ([]byte, error) {
	img, err := decodeImage(raw)
	if err != nil {
		return nil, err
	}
	return render(img)
}

func decodeImage(raw []byte) (image.Image, error) {
	if len(raw) > MaxIconBytes {
		return nil, fmt.Errorf("icon larger than %d bytes", MaxIconBytes)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxIconSide || cfg.Height > maxIconSide {
		return nil, fmt.Errorf("icon is %dx%d, at most %d per side", cfg.Width, cfg.Height, maxIconSide)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	return img, err
}

func render(img image.Image) ([]byte, error) {
	b := img.Bounds()
	w, h := IconSize, IconSize
	if b.Dx() > b.Dy() {
		h = IconSize * b.Dy() / b.Dx()
	} else if b.Dy() > b.Dx() {
		w = IconSize * b.Dx() / b.Dy()
	}
	dst := image.NewNRGBA(image.Rect(0, 0, IconSize, IconSize))
	off := image.Pt((IconSize-w)/2, (IconSize-h)/2)
	draw.CatmullRom.Scale(dst, image.Rectangle{Min: off, Max: off.Add(image.Pt(w, h))}, img, b, draw.Over, nil)
	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type iconReader struct {
	files map[string]*zip.File
	table *androidbinary.TableFile
}

func extractIcon(file string, app androidapk.Application) (icon []byte) {
	defer func() {
		if recover() != nil {
			icon = nil
		}
	}()
	z, err := zip.OpenReader(file)
	if err != nil {
		return nil
	}
	defer z.Close()
	r := &iconReader{files: map[string]*zip.File{}}
	for _, f := range z.File {
		r.files[f.Name] = f
	}
	for _, sdk := range []uint16{34, 25} {
		cfg := &androidbinary.ResTableConfig{SDKVersion: sdk, Density: 640}
		p, err := app.Icon.WithResTableConfig(cfg).String()
		if err != nil {
			continue
		}
		img, err := r.image(p, cfg, true)
		if err != nil {
			continue
		}
		if out, err := render(img); err == nil {
			return out
		}
	}
	return nil
}

// image reads a bitmap or a drawable XML; shrunk APKs such as Chromium's
// drop the extensions from resource paths, so those are tried as both.
func (r *iconReader) image(p string, cfg *androidbinary.ResTableConfig, top bool) (image.Image, error) {
	switch {
	case isBitmap(p):
		return r.bitmap(p)
	case strings.HasSuffix(p, ".xml"):
		return r.drawable(p, cfg, top)
	case path.Ext(p) == "":
		if img, err := r.bitmap(p); err == nil {
			return img, nil
		}
		return r.drawable(p, cfg, top)
	}
	return nil, fmt.Errorf("unsupported icon %s", p)
}

func isBitmap(p string) bool {
	return slices.Contains([]string{".png", ".webp", ".jpg", ".jpeg"}, strings.ToLower(path.Ext(p)))
}

func (r *iconReader) read(name string, limit int64) ([]byte, error) {
	f, ok := r.files[name]
	if !ok {
		return nil, fmt.Errorf("%s not in apk", name)
	}
	if f.UncompressedSize64 > uint64(limit) {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, limit)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, limit)
	}
	return raw, nil
}

func (r *iconReader) bitmap(name string) (image.Image, error) {
	raw, err := r.read(name, MaxIconBytes)
	if err != nil {
		return nil, err
	}
	return decodeImage(raw)
}

func (r *iconReader) drawable(name string, cfg *androidbinary.ResTableConfig, top bool) (image.Image, error) {
	raw, err := r.read(name, maxXMLBytes)
	if err != nil {
		return nil, err
	}
	doc, err := androidbinary.NewXMLFile(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	root, err := parseXML(doc.Reader())
	if err != nil {
		return nil, err
	}
	if r.table == nil {
		rawTable, err := r.read("resources.arsc", maxTable)
		if err != nil {
			return nil, err
		}
		if r.table, err = androidbinary.NewTableFile(bytes.NewReader(rawTable)); err != nil {
			return nil, err
		}
	}
	switch {
	case root.Name == "adaptive-icon" && top:
		return r.adaptive(root, cfg)
	case root.Name == "vector":
		return r.vector(root, cfg)
	}
	return nil, fmt.Errorf("unsupported drawable <%s>", root.Name)
}

func (r *iconReader) adaptive(icon xmlNode, cfg *androidbinary.ResTableConfig) (image.Image, error) {
	layer := func(name string) any {
		n, ok := icon.child(name)
		if !ok {
			return nil
		}
		return r.layer(n.attr("drawable"), cfg)
	}
	fg, ok := layer("foreground").(image.Image)
	if !ok {
		return nil, errors.New("adaptive icon foreground is neither a bitmap nor a vector")
	}
	const side = 4 * 108
	canvas := image.NewNRGBA(image.Rect(0, 0, side, side))
	switch bg := layer("background").(type) {
	case color.Color:
		draw.Draw(canvas, canvas.Bounds(), image.NewUniform(bg), image.Point{}, draw.Src)
	case image.Image:
		draw.CatmullRom.Scale(canvas, canvas.Bounds(), bg, bg.Bounds(), draw.Src, nil)
	default:
		draw.Draw(canvas, canvas.Bounds(), image.White, image.Point{}, draw.Src)
	}
	draw.CatmullRom.Scale(canvas, canvas.Bounds(), fg, fg.Bounds(), draw.Over, nil)
	inset := side * 18 / 108
	return canvas.SubImage(image.Rect(inset, inset, side-inset, side-inset)), nil
}

func (r *iconReader) layer(ref string, cfg *androidbinary.ResTableConfig) any {
	for range 4 {
		id, err := androidbinary.ParseResID(ref)
		if err != nil {
			return nil
		}
		v, err := r.table.GetResource(id, cfg)
		if err != nil {
			return nil
		}
		switch v := v.(type) {
		case string:
			img, err := r.image(v, cfg, false)
			if err != nil {
				return nil
			}
			return img
		case uint32:
			next := fmt.Sprintf("@0x%08X", v)
			if nid, _ := androidbinary.ParseResID(next); nid.Package() != 0 {
				if _, err := r.table.GetResource(nid, cfg); err == nil {
					ref = next
					continue
				}
			}
			return color.NRGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: uint8(v >> 24)}
		default:
			return nil
		}
	}
	return nil
}

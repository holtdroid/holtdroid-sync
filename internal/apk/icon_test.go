package apk

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func encodePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		for y := range h {
			img.Set(x, y, color.NRGBA{R: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestNormalizeIcon(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
		ok   bool
	}{
		{"square png", encodePNG(t, 48, 48), true},
		{"wide png", encodePNG(t, 64, 32), true},
		{"too wide", encodePNG(t, maxIconSide+1, 1), false},
		{"not an image", []byte("<svg onload=alert(1)>"), false},
		{"too large", append(encodePNG(t, 8, 8), make([]byte, MaxIconBytes)...), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := NormalizeIcon(tt.raw)
			if (err == nil) != tt.ok {
				t.Fatalf("err = %v", err)
			}
			if !tt.ok {
				return
			}
			img, err := png.Decode(bytes.NewReader(out))
			if err != nil {
				t.Fatal(err)
			}
			if b := img.Bounds(); b.Dx() != IconSize || b.Dy() != IconSize {
				t.Fatalf("size %v", b)
			}
			if _, _, _, a := img.At(IconSize/2, IconSize/2).RGBA(); a == 0 {
				t.Fatal("centre is transparent")
			}
		})
	}
}

func TestParseIcon(t *testing.T) {
	_, icon, err := Parse("testdata/helloworld.apk")
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(icon))
	if err != nil {
		t.Fatalf("icon is not a png: %v", err)
	}
	if b := img.Bounds(); b.Dx() != IconSize || b.Dy() != IconSize {
		t.Fatalf("icon size %v", b)
	}
}

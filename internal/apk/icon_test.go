package apk

import (
	"archive/zip"
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
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
	tests := []struct {
		apk    string
		pixels map[image.Point]color.NRGBA
	}{
		{apk: "helloworld.apk"},
		{
			apk: "vectoricon.apk",
			pixels: map[image.Point]color.NRGBA{
				{4, 4}:     {0xff, 0xf3, 0xe6, 0xff},
				{128, 120}: {0x8a, 0x5a, 0x3c, 0xff},
				{87, 69}:   {0x3d, 0xdc, 0x84, 0xff},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.apk, func(t *testing.T) {
			_, icon, err := Parse("testdata/" + tt.apk)
			if err != nil {
				t.Fatal(err)
			}
			if icon == nil {
				t.Fatal("no icon")
			}
			img, err := png.Decode(bytes.NewReader(icon))
			if err != nil {
				t.Fatalf("icon is not a png: %v", err)
			}
			if b := img.Bounds(); b.Dx() != IconSize || b.Dy() != IconSize {
				t.Fatalf("icon size %v", b)
			}
			for p, want := range tt.pixels {
				if got := color.NRGBAModel.Convert(img.At(p.X, p.Y)).(color.NRGBA); !near(got, want) {
					t.Errorf("pixel %v = %v, want %v", p, got, want)
				}
			}
		})
	}
}

func near(a, b color.NRGBA) bool {
	d := func(x, y uint8) bool { return x-y < 12 || y-x < 12 }
	return d(a.R, b.R) && d(a.G, b.G) && d(a.B, b.B) && d(a.A, b.A)
}

func TestVector(t *testing.T) {
	const ns = `xmlns:android="http://schemas.android.com/apk/res/android"`
	red := color.NRGBA{0xff, 0, 0, 0xff}
	clear := color.NRGBA{}
	tests := []struct {
		name   string
		xml    string
		pixels map[image.Point]color.NRGBA
		ok     bool
	}{
		{
			name:   "fill",
			xml:    `<vector ` + ns + ` android:viewportWidth="10" android:viewportHeight="10"><path android:fillColor="#f00" android:pathData="M0,0h5v10h-5z"/></vector>`,
			pixels: map[image.Point]color.NRGBA{{100, 200}: red, {300, 200}: clear},
			ok:     true,
		},
		{
			name: "group transform",
			xml: `<vector ` + ns + ` android:viewportWidth="10" android:viewportHeight="10"><group android:translateX="5">` +
				`<path android:fillColor="#ffff0000" android:pathData="M0,0h5v10h-5z"/></group></vector>`,
			pixels: map[image.Point]color.NRGBA{{100, 200}: clear, {300, 200}: red},
			ok:     true,
		},
		{
			name: "round stroke",
			xml: `<vector ` + ns + ` android:viewportWidth="10" android:viewportHeight="10">` +
				`<path android:strokeColor="#f00" android:strokeWidth="2" android:strokeLineCap="round" android:pathData="M2,5H8"/></vector>`,
			pixels: map[image.Point]color.NRGBA{{216, 216}: red, {216, 50}: clear},
			ok:     true,
		},
		{
			name: "even-odd hole",
			xml: `<vector ` + ns + ` android:viewportWidth="10" android:viewportHeight="10">` +
				`<path android:fillColor="#f00" android:fillType="evenOdd" android:pathData="M0,0h10v10h-10zM3,3h4v4h-4z"/></vector>`,
			pixels: map[image.Point]color.NRGBA{{20, 20}: red, {216, 216}: clear},
			ok:     true,
		},
		{name: "no viewport", xml: `<vector ` + ns + `><path android:fillColor="#f00" android:pathData="M0,0h5v5z"/></vector>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, err := parseXML(strings.NewReader(tt.xml))
			if err != nil {
				t.Fatal(err)
			}
			img, err := (&iconReader{}).vector(root, nil)
			if (err == nil) != tt.ok {
				t.Fatalf("err = %v", err)
			}
			for p, want := range tt.pixels {
				if got := color.NRGBAModel.Convert(img.At(p.X, p.Y)).(color.NRGBA); !near(got, want) {
					t.Errorf("pixel %v = %v, want %v", p, got, want)
				}
			}
		})
	}
}

func TestImageWithoutExtension(t *testing.T) {
	var img bytes.Buffer
	if err := png.Encode(&img, image.NewNRGBA(image.Rect(0, 0, 48, 48))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "shrunk.apk")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range map[string][]byte{"res/im": img.Bytes(), "res/zz": []byte("neither")} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	z, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	r := &iconReader{files: map[string]*zip.File{}}
	for _, f := range z.File {
		r.files[f.Name] = f
	}
	if got, err := r.image("res/im", nil, false); err != nil || got.Bounds().Dx() != 48 {
		t.Fatalf("extensionless bitmap: %v", err)
	}
	if _, err := r.image("res/zz", nil, false); err == nil {
		t.Fatal("garbage decoded")
	}
}

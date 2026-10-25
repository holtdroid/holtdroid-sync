package apk

import (
	"archive/zip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unicode/utf16"
)

const (
	chunkStringPool  = 0x0001
	chunkXML         = 0x0003
	chunkResourceMap = 0x0180
	chunkStartElem   = 0x0102

	attrVersionCode = 0x0101021b
	attrVersionName = 0x0101021c
	attrMinSDK      = 0x0101020c
	attrTargetSDK   = 0x01010270

	typeString = 0x03
	typeIntDec = 0x10
	typeIntHex = 0x11
)

// parseManifest reads the package, version and SDK levels from a binary
// AndroidManifest.xml by attribute resource IDs. It is the fallback for
// manifests androidbinary refuses, such as Chromium's, whose optimised
// namespaces it can't resolve.
func parseManifest(path string) (Metadata, error) {
	z, err := zip.OpenReader(path)
	if err != nil {
		return Metadata{}, err
	}
	defer z.Close()
	f, err := z.Open("AndroidManifest.xml")
	if err != nil {
		return Metadata{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, 16<<20))
	f.Close()
	if err != nil {
		return Metadata{}, err
	}
	return decodeManifest(raw)
}

var errManifest = errors.New("malformed binary manifest")

func decodeManifest(b []byte) (Metadata, error) {
	le := binary.LittleEndian
	if len(b) < 8 || le.Uint16(b) != chunkXML {
		return Metadata{}, errManifest
	}
	var strs []string
	var ids []uint32
	var m Metadata
	elements := 0
	for off := int(le.Uint16(b[2:])); off+8 <= len(b); {
		typ, hdr, size := le.Uint16(b[off:]), int(le.Uint16(b[off+2:])), int(le.Uint32(b[off+4:]))
		if size < 8 || off+size > len(b) || hdr > size {
			return Metadata{}, errManifest
		}
		c := b[off : off+size]
		switch typ {
		case chunkStringPool:
			var err error
			if strs, err = stringPool(c); err != nil {
				return Metadata{}, err
			}
		case chunkResourceMap:
			for i := hdr; i+4 <= size; i += 4 {
				ids = append(ids, le.Uint32(c[i:]))
			}
		case chunkStartElem:
			if err := attrs(c, hdr, strs, ids, &m, elements == 0); err != nil {
				return Metadata{}, err
			}
			elements++
		}
		off += size
	}
	if m.Package == "" {
		return Metadata{}, errors.New("apk has no package name")
	}
	return m, nil
}

func attrs(c []byte, hdr int, strs []string, ids []uint32, m *Metadata, root bool) error {
	le := binary.LittleEndian
	if len(c) < hdr+20 {
		return errManifest
	}
	ext := c[hdr:]
	start, step, count := int(le.Uint16(ext[8:])), int(le.Uint16(ext[10:])), int(le.Uint16(ext[12:]))
	str := func(i uint32) string {
		if int(i) < len(strs) {
			return strs[i]
		}
		return ""
	}
	for i := range count {
		a := start + i*step
		if step < 20 || a+20 > len(ext) {
			return errManifest
		}
		name, raw, typ, data := le.Uint32(ext[a+4:]), le.Uint32(ext[a+8:]), ext[a+15], le.Uint32(ext[a+16:])
		var id uint32
		if int(name) < len(ids) {
			id = ids[name]
		}
		text := str(raw)
		if typ == typeString {
			text = str(data)
		}
		num := int32(0)
		if typ == typeIntDec || typ == typeIntHex {
			num = int32(data)
		}
		switch {
		case !root && id != attrMinSDK && id != attrTargetSDK:
		case id == attrVersionCode:
			m.VersionCode = num
		case id == attrVersionName:
			m.VersionName = text
		case id == attrMinSDK:
			m.MinSDK = num
		case id == attrTargetSDK:
			m.TargetSDK = num
		case id == 0 && str(name) == "package":
			m.Package = text
		}
	}
	return nil
}

func stringPool(c []byte) ([]string, error) {
	le := binary.LittleEndian
	if len(c) < 28 {
		return nil, errManifest
	}
	hdr := int(le.Uint16(c[2:]))
	count, flags, start := int(le.Uint32(c[8:])), le.Uint32(c[16:]), int(le.Uint32(c[20:]))
	if hdr+count*4 > len(c) || start > len(c) {
		return nil, errManifest
	}
	out := make([]string, count)
	for i := range count {
		p := start + int(le.Uint32(c[hdr+i*4:]))
		var s string
		var err error
		if flags&0x100 != 0 {
			s, err = utf8String(c, p)
		} else {
			s, err = utf16String(c, p)
		}
		if err != nil {
			return nil, fmt.Errorf("string %d: %w", i, err)
		}
		out[i] = s
	}
	return out, nil
}

func utf8String(c []byte, p int) (string, error) {
	length := func() (int, error) {
		if p >= len(c) {
			return 0, errManifest
		}
		n := int(c[p])
		p++
		if n&0x80 != 0 {
			if p >= len(c) {
				return 0, errManifest
			}
			n = (n&0x7f)<<8 | int(c[p])
			p++
		}
		return n, nil
	}
	if _, err := length(); err != nil {
		return "", err
	}
	n, err := length()
	if err != nil || p+n > len(c) {
		return "", errManifest
	}
	return string(c[p : p+n]), nil
}

func utf16String(c []byte, p int) (string, error) {
	le := binary.LittleEndian
	if p+2 > len(c) {
		return "", errManifest
	}
	n := int(le.Uint16(c[p:]))
	p += 2
	if n&0x8000 != 0 {
		if p+2 > len(c) {
			return "", errManifest
		}
		n = (n&0x7fff)<<16 | int(le.Uint16(c[p:]))
		p += 2
	}
	if p+n*2 > len(c) {
		return "", errManifest
	}
	u := make([]uint16, n)
	for i := range u {
		u[i] = le.Uint16(c[p+i*2:])
	}
	return string(utf16.Decode(u)), nil
}

package apk

import (
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"image"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/shogo82148/androidbinary"
	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
)

const (
	vectorSide     = 4 * 108
	maxVectorPaths = 2000
	maxVectorDepth = 32
)

type xmlNode struct {
	Name     string
	Attrs    map[string]string
	Children []xmlNode
}

func (n xmlNode) attr(name string) string { return n.Attrs[name] }

const maxXMLNodes = 20000

func parseXML(r io.Reader) (xmlNode, error) {
	dec := xml.NewDecoder(r)
	var stack []xmlNode
	count := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return xmlNode{}, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if count++; count > maxXMLNodes {
				return xmlNode{}, fmt.Errorf("more than %d xml elements", maxXMLNodes)
			}
			n := xmlNode{Name: t.Name.Local, Attrs: map[string]string{}}
			for _, a := range t.Attr {
				n.Attrs[a.Name.Local] = a.Value
			}
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) == 0 {
				return xmlNode{}, errors.New("unbalanced xml")
			}
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return n, nil
			}
			parent := &stack[len(stack)-1]
			parent.Children = append(parent.Children, n)
		}
	}
}

func (n xmlNode) child(name string) (xmlNode, bool) {
	for _, c := range n.Children {
		if c.Name == name {
			return c, true
		}
	}
	return xmlNode{}, false
}

// vector renders an Android VectorDrawable by translating it to SVG.
// Gradients, clip paths and theme attributes are not supported: such paths
// are drawn without that fill.
func (r *iconReader) vector(root xmlNode, cfg *androidbinary.ResTableConfig) (image.Image, error) {
	vw := r.number(root.attr("viewportWidth"), cfg, 0)
	vh := r.number(root.attr("viewportHeight"), cfg, 0)
	if !(vw > 0 && vh > 0 && vw < 1e5 && vh < 1e5) {
		return nil, fmt.Errorf("vector viewport %gx%g", vw, vh)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %g %g" width="%g" height="%g">`, vw, vh, vw, vh)
	paths := 0
	if err := r.writeVector(&b, root.Children, cfg, &paths, 0); err != nil {
		return nil, err
	}
	b.WriteString("</svg>")
	icon, err := oksvg.ReadIconStream(strings.NewReader(b.String()), oksvg.IgnoreErrorMode)
	if err != nil {
		return nil, err
	}
	img := image.NewNRGBA(image.Rect(0, 0, vectorSide, vectorSide))
	icon.SetTarget(0, 0, vectorSide, vectorSide)
	scanner := rasterx.NewScannerGV(vectorSide, vectorSide, img, img.Bounds())
	icon.Draw(rasterx.NewDasher(vectorSide, vectorSide, scanner), 1)
	return img, nil
}

func (r *iconReader) writeVector(b *strings.Builder, nodes []xmlNode, cfg *androidbinary.ResTableConfig, paths *int, depth int) error {
	if depth > maxVectorDepth {
		return errors.New("vector groups nested too deep")
	}
	for _, n := range nodes {
		switch n.Name {
		case "group":
			px := r.number(n.attr("pivotX"), cfg, 0)
			py := r.number(n.attr("pivotY"), cfg, 0)
			fmt.Fprintf(b, `<g transform="translate(%g %g) rotate(%g) scale(%g %g) translate(%g %g)">`,
				r.number(n.attr("translateX"), cfg, 0)+px, r.number(n.attr("translateY"), cfg, 0)+py,
				r.number(n.attr("rotation"), cfg, 0),
				r.number(n.attr("scaleX"), cfg, 1), r.number(n.attr("scaleY"), cfg, 1),
				-px, -py)
			if err := r.writeVector(b, n.Children, cfg, paths, depth+1); err != nil {
				return err
			}
			b.WriteString("</g>")
		case "path":
			if *paths++; *paths > maxVectorPaths {
				return fmt.Errorf("vector has more than %d paths", maxVectorPaths)
			}
			d := r.text(n.attr("pathData"), cfg)
			if d == "" {
				continue
			}
			fill, fillAlpha := r.paint(n.attr("fillColor"), cfg)
			stroke, strokeAlpha := r.paint(n.attr("strokeColor"), cfg)
			fmt.Fprintf(b, `<path d="%s" fill="%s" fill-opacity="%g" fill-rule="%s" stroke="%s" stroke-opacity="%g" stroke-width="%g" stroke-linecap="%s" stroke-linejoin="%s" stroke-miterlimit="%g"/>`,
				html.EscapeString(d),
				fill, fillAlpha*r.number(n.attr("fillAlpha"), cfg, 1),
				enum(n.attr("fillType"), "nonzero", "evenodd"),
				stroke, strokeAlpha*r.number(n.attr("strokeAlpha"), cfg, 1),
				r.number(n.attr("strokeWidth"), cfg, 0),
				enum(n.attr("strokeLineCap"), "butt", "round", "square"),
				enum(n.attr("strokeLineJoin"), "miter", "round", "bevel"),
				r.number(n.attr("strokeMiterLimit"), cfg, 4))
		}
	}
	return nil
}

func enum(v string, names ...string) string {
	if i, err := strconv.Atoi(v); err == nil && i >= 0 && i < len(names) {
		return names[i]
	}
	for _, name := range names {
		if strings.EqualFold(v, name) {
			return name
		}
	}
	return names[0]
}

// Compiled XML loses value types: androidbinary prints both references and
// raw typed values (floats, colors) as "@0x…". A value that resolves in the
// resource table is a reference; anything else is the raw value.
func (r *iconReader) resolve(v string, cfg *androidbinary.ResTableConfig) (any, bool) {
	if r.table == nil || !strings.HasPrefix(v, "@0x") {
		return nil, false
	}
	id, err := androidbinary.ParseResID(v)
	if err != nil || id.Package() == 0 {
		return nil, false
	}
	res, err := r.table.GetResource(id, cfg)
	if err != nil {
		return nil, false
	}
	return res, true
}

func rawBits(v string) (uint32, bool) {
	hex, ok := strings.CutPrefix(v, "@0x")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(hex, 16, 32)
	return uint32(n), err == nil
}

func (r *iconReader) number(v string, cfg *androidbinary.ResTableConfig, def float64) float64 {
	if v == "" {
		return def
	}
	if _, ok := r.resolve(v, cfg); ok {
		return def
	}
	if bits, ok := rawBits(v); ok {
		f := float64(math.Float32frombits(bits))
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return def
		}
		return f
	}
	f, err := strconv.ParseFloat(strings.TrimSuffix(v, "dp"), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return def
	}
	return f
}

func (r *iconReader) text(v string, cfg *androidbinary.ResTableConfig) string {
	if res, ok := r.resolve(v, cfg); ok {
		s, _ := res.(string)
		return s
	}
	if strings.HasPrefix(v, "@") || strings.HasPrefix(v, "?") {
		return ""
	}
	return v
}

func (r *iconReader) paint(v string, cfg *androidbinary.ResTableConfig) (string, float64) {
	argb, ok := uint32(0), false
	if res, resolved := r.resolve(v, cfg); resolved {
		argb, ok = res.(uint32)
	} else if bits, raw := rawBits(v); raw {
		argb, ok = bits, true
	} else {
		argb, ok = parseHexColor(v)
	}
	if !ok {
		return "none", 0
	}
	return fmt.Sprintf("#%06x", argb&0xffffff), float64(argb>>24) / 255
}

func parseHexColor(v string) (uint32, bool) {
	hex, ok := strings.CutPrefix(v, "#")
	if !ok {
		return 0, false
	}
	if len(hex) == 3 || len(hex) == 4 {
		var long strings.Builder
		for _, c := range hex {
			long.WriteRune(c)
			long.WriteRune(c)
		}
		hex = long.String()
	}
	n, err := strconv.ParseUint(hex, 16, 32)
	switch {
	case err != nil:
		return 0, false
	case len(hex) == 6:
		return 0xff000000 | uint32(n), true
	case len(hex) == 8:
		return uint32(n), true
	}
	return 0, false
}

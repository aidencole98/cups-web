package main

import (
	"math"
	"os"

	"github.com/phpdave11/gofpdf"
	rscpdf "rsc.io/pdf"
)

const photoFillLayoutKeyword = "cups-web:photo-layout:fill:v1"

// Use the conversion's page setup when checking the requested output size too.
func newPhotoPDF(orientation, paperSize string) *gofpdf.Fpdf {
	name, size := paperSizeToGofpdf(paperSize)
	if name != "" {
		return gofpdf.New(getOrientationCode(orientation), "mm", name, "")
	}
	// Driver dimensions can be reported wide edge first. Apply the selected
	// direction to a portrait base in every photo path, including reprints.
	if size.Wd > size.Ht {
		size.Wd, size.Ht = size.Ht, size.Wd
	}
	return gofpdf.NewCustom(&gofpdf.InitType{
		UnitStr: "mm", Size: size, OrientationStr: getOrientationCode(orientation),
	})
}

// photoPrintScaling keeps a completed photo layout from being filled/cropped a
// second time by CUPS. The marker travels with PDFs returned by /api/convert,
// including downloads later uploaded again or used for reprints/schedules.
// It is a layout hint, not an authentication or authorization mechanism.
func photoPrintScaling(path, scaling, mime, orientation, paperSize string) (effective string) {
	effective = scaling
	if scaling != "fill" || mime != "application/pdf" {
		return
	}
	// Malformed uploaded PDFs must retain the normal printing fallback.
	defer func() {
		if recover() != nil {
			effective = scaling
		}
	}()
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return
	}
	doc, err := rscpdf.NewReader(f, info.Size())
	if err != nil || doc.Trailer().Key("Info").Key("Keywords").Text() != photoFillLayoutKeyword {
		return
	}
	width, height := newPhotoPDF(orientation, paperSize).GetPageSize()
	width, height = width*72/25.4, height*72/25.4
	pages := doc.Trailer().Key("Root").Key("Pages").Key("Kids")
	if doc.NumPage() < 1 || pages.Len() != doc.NumPage() {
		return
	}
	// gofpdf emits a flat page tree. Only inspect those leaves directly, so a
	// malformed uploaded page tree cannot trap Reader.Page in a traversal cycle.
	for i := 0; i < pages.Len(); i++ {
		page := pages.Index(i)
		if page.Key("Type").Name() != "Page" {
			return
		}
		box := photoPageAttribute(page, "MediaBox")
		crop := photoPageAttribute(page, "CropBox")
		// Our photo PDFs use an unrotated page at the origin.
		// If another tool changes that geometry, leave scaling to CUPS.
		if !matchesPhotoPageBox(box, width, height) ||
			(!crop.IsNull() && !matchesPhotoPageBox(crop, width, height)) ||
			photoPageAttribute(page, "Rotate").Float64() != 0 ||
			(!page.Key("UserUnit").IsNull() && page.Key("UserUnit").Float64() != 1) {
			return
		}
	}
	return "none"
}

func matchesPhotoPageBox(box rscpdf.Value, width, height float64) bool {
	return box.Len() == 4 && box.Index(0).Float64() == 0 && box.Index(1).Float64() == 0 &&
		math.Abs(box.Index(2).Float64()-width) <= 0.05 && math.Abs(box.Index(3).Float64()-height) <= 0.05
}

func photoPageAttribute(page rscpdf.Value, key string) rscpdf.Value {
	// Bound traversal so malformed parent cycles cannot hang attribute lookup.
	for depth := 0; depth < 32 && !page.IsNull(); depth++ {
		if value := page.Key(key); !value.IsNull() {
			return value
		}
		page = page.Key("Parent")
	}
	return rscpdf.Value{}
}

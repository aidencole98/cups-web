package main

import (
	"bytes"
	"image"
	"image/png"
	"io"
	"math/rand"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	rscpdf "rsc.io/pdf"
)

func writeCompressionTestImage(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewSource(119))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = uint8(rng.Intn(256)), uint8(rng.Intn(256)), uint8(rng.Intn(256)), 255
	}
	// 透明角落应被合成到白底。
	for y := 0; y < min(32, h); y++ {
		for x := 0; x < min(32, w); x++ {
			img.Pix[y*img.Stride+x*4+3] = 0
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "noise.png")
	if err := os.WriteFile(path, buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCompressImageIfNeeded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		w, h   int
		resize bool
	}{
		{"quality", 1000, 700, false},
		{"resize", 2400, 1600, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := writeCompressionTestImage(t, tc.w, tc.h)
			before, _ := os.ReadFile(input)
			if len(before) <= imageCompressionMaxBytes {
				t.Fatal("fixture must exceed the byte limit while below the old 3000px threshold")
			}
			output, cfg, err := compressImageIfNeeded(input, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			if len(data) > imageCompressionMaxBytes {
				t.Fatalf("compressed size = %d, want <= %d", len(data), imageCompressionMaxBytes)
			}
			decoded, format, err := image.Decode(bytes.NewReader(data))
			if err != nil || format != "jpeg" {
				t.Fatalf("invalid compressed JPEG: %s, %v", format, err)
			}
			if cfg.Width != decoded.Bounds().Dx() || cfg.Height != decoded.Bounds().Dy() {
				t.Fatal("layout dimensions differ from encoded image")
			}
			if tc.resize {
				if cfg.Width >= tc.w || cfg.Height >= tc.h {
					t.Fatal("high entropy image must shrink when quality alone cannot meet the limit")
				}
				if diff := cfg.Width*tc.h - cfg.Height*tc.w; diff < -tc.w || diff > tc.w {
					t.Fatal("aspect ratio changed")
				}
			} else if cfg.Width != tc.w || cfg.Height != tc.h {
				t.Fatal("quality compression should preserve dimensions")
			}
			r, g, b, _ := decoded.At(0, 0).RGBA()
			if r < 240*257 || g < 240*257 || b < 240*257 {
				t.Fatal("transparent pixels were not composited onto white")
			}
			after, _ := os.ReadFile(input)
			if !bytes.Equal(before, after) {
				t.Fatal("compression modified the original")
			}
		})
	}
}

func TestCompressImageSmallAndInvalid(t *testing.T) {
	input := writeCompressionTestImage(t, 32, 16)
	output, cfg, err := compressImageIfNeeded(input, t.TempDir())
	if err != nil || output != input || cfg.Width != 32 || cfg.Height != 16 {
		t.Fatalf("small image should pass through: %s, %+v, %v", output, cfg, err)
	}
	invalid := filepath.Join(t.TempDir(), "broken.png")
	if err := os.WriteFile(invalid, []byte("not an image"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{invalid, invalid + ".missing"} {
		if _, _, err := compressImageIfNeeded(path, t.TempDir()); err == nil {
			t.Fatal("invalid or missing input must fail")
		}
	}
}

func assertPDFImageLimit(t *testing.T, path string, pages, images int, compressed bool) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, _ := f.Stat()
	doc, err := rscpdf.NewReader(f, info.Size())
	if err != nil {
		t.Fatal(err)
	}
	if doc.NumPage() != pages {
		t.Fatalf("pages = %d, want %d", doc.NumPage(), pages)
	}
	count := 0
	for page := 1; page <= pages; page++ {
		xobjects := doc.Page(page).V.Key("Resources").Key("XObject")
		for _, key := range xobjects.Keys() {
			obj := xobjects.Key(key)
			if obj.Key("Subtype").Name() != "Image" {
				continue
			}
			count++
			if compressed && obj.Key("Length").Int64() > imageCompressionMaxBytes {
				t.Fatalf("embedded image exceeds limit: %d", obj.Key("Length").Int64())
			}
			if !compressed && obj.Key("Length").Int64() <= imageCompressionMaxBytes {
				t.Fatal("disabled compression unexpectedly changed the image")
			}
		}
	}
	if count != images {
		t.Fatalf("images = %d, want %d", count, images)
	}
}

func TestConvertImageCompression(t *testing.T) {
	input := writeCompressionTestImage(t, 1600, 1000)
	for _, compress := range []bool{false, true} {
		path, cleanup, err := convertImageToPDF(input, "portrait", "A4", true, compress)
		if err != nil {
			t.Fatal(err)
		}
		assertPDFImageLimit(t, path, 1, 1, compress)
		cleanup()
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for i := 0; i < 2; i++ {
		part, err := writer.CreateFormFile("files", "image.png")
		if err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(input)
		if _, err := io.Copy(part, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/convert", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if err := req.ParseMultipartForm(512 << 20); err != nil {
		t.Fatal(err)
	}
	defer req.MultipartForm.RemoveAll()
	path, cleanup, err := convertImagesMultiToPDF(req.MultipartForm.File["files"], "landscape", "A4", []int{90, 270}, true, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	assertPDFImageLimit(t, path, 1, 2, true)
}

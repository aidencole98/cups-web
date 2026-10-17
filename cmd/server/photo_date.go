package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/phpdave11/gofpdf"
)

const maxPhotoEXIFBytes = 256 << 10

// Only DateTimeOriginal is a shooting date. TIFF DateTime may be an edit date.
func photoDateFromTIFF(b []byte) string {
	if len(b) < 8 {
		return ""
	}
	var order binary.ByteOrder
	switch string(b[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return ""
	}
	if order.Uint16(b[2:4]) != 42 {
		return ""
	}
	entries := func(offset uint32, tag uint16) []byte {
		p := uint64(offset)
		if p < 8 || p+2 > uint64(len(b)) {
			return nil
		}
		n := uint64(order.Uint16(b[p : p+2]))
		if n > 4096 || p+2+n*12 > uint64(len(b)) {
			return nil
		}
		for i := uint64(0); i < n; i++ {
			e := b[p+2+i*12 : p+2+(i+1)*12]
			if order.Uint16(e[:2]) == tag {
				return e
			}
		}
		return nil
	}
	pointer := entries(order.Uint32(b[4:8]), 0x8769)
	if pointer == nil || order.Uint16(pointer[2:4]) != 4 || order.Uint32(pointer[4:8]) != 1 {
		return ""
	}
	date := entries(order.Uint32(pointer[8:12]), 0x9003)
	if date == nil || order.Uint16(date[2:4]) != 2 || order.Uint32(date[4:8]) != 20 {
		return ""
	}
	p := uint64(order.Uint32(date[8:12]))
	if p+20 > uint64(len(b)) || b[p+19] != 0 {
		return ""
	}
	s := string(b[p : p+19])
	t, err := time.Parse("2006:01:02 15:04:05", s)
	if err != nil || t.Year() < 1 || t.Format("2006:01:02 15:04:05") != s {
		return ""
	}
	return t.Format("2006-01-02")
}

// ReaderAt lets us skip image data without decoding pixels or allocating chunks.
func readPhotoDate(r io.ReaderAt) string {
	var head [8]byte
	if _, err := r.ReadAt(head[:], 0); err != nil {
		return ""
	}
	readEXIF := func(offset int64, size uint32, jpeg bool) string {
		if size > maxPhotoEXIFBytes || size < 8 {
			return ""
		}
		b := make([]byte, int(size))
		if _, err := r.ReadAt(b, offset); err != nil {
			return ""
		}
		if jpeg {
			if len(b) < 6 || string(b[:6]) != "Exif\x00\x00" {
				return ""
			}
			b = b[6:]
		}
		return photoDateFromTIFF(b)
	}
	if head[0] == 0xff && head[1] == 0xd8 {
		p := int64(2)
		for i := 0; i < 1024; i++ {
			var marker [2]byte
			if _, err := r.ReadAt(marker[:], p); err != nil || marker[0] != 0xff {
				return ""
			}
			if marker[1] == 0xff {
				p++
				continue
			}
			if marker[1] == 0xda || marker[1] == 0xd9 {
				return ""
			}
			if marker[1] == 0x01 || (marker[1] >= 0xd0 && marker[1] <= 0xd7) {
				p += 2
				continue
			}
			var size [2]byte
			if _, err := r.ReadAt(size[:], p+2); err != nil {
				return ""
			}
			n := binary.BigEndian.Uint16(size[:])
			if n < 2 {
				return ""
			}
			if marker[1] == 0xe1 {
				if date := readEXIF(p+4, uint32(n)-2, true); date != "" {
					return date
				}
			}
			p += int64(n) + 2
		}
	} else if string(head[:]) == "\x89PNG\r\n\x1a\n" {
		p := int64(8)
		for i := 0; i < 1024; i++ {
			var chunk [8]byte
			if _, err := r.ReadAt(chunk[:], p); err != nil {
				return ""
			}
			n := binary.BigEndian.Uint32(chunk[:4])
			if n > 0x7fffffff {
				return ""
			}
			switch string(chunk[4:]) {
			case "eXIf":
				return readEXIF(p+8, n, false)
			case "IEND":
				return ""
			}
			p += int64(n) + 12
		}
	}
	return ""
}

func photoDateFromFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	return readPhotoDate(f)
}

func validPhotoDate(s string) bool {
	t, err := time.Parse("2006-01-02", s)
	return err == nil && t.Year() > 0 && t.Format("2006-01-02") == s
}

func parsePhotoDates(raw string, count int) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	if len(raw) > 4096 {
		return nil, errors.New("照片日期参数过长")
	}
	var values []*string
	if err := json.Unmarshal([]byte(raw), &values); err != nil || len(values) != count {
		return nil, errors.New("照片日期必须与图片数量一一对应")
	}
	dates := make([]string, len(values))
	for i, value := range values {
		if value == nil || (*value != "" && !validPhotoDate(*value)) {
			return nil, errors.New("照片日期格式必须为 YYYY-MM-DD")
		}
		dates[i] = *value
	}
	return dates, nil
}

func resolvedPhotoDate(path string, enabled bool, dates []string, index int) string {
	if !enabled {
		return ""
	}
	if index < len(dates) {
		return dates[index]
	}
	return photoDateFromFile(path)
}

func drawPhotoDate(pdf *gofpdf.Fpdf, date string, x, y, w, h float64) {
	if !validPhotoDate(date) || w <= 0 || h <= 0 {
		return
	}
	t, _ := time.Parse("2006-01-02", date)
	text := fmt.Sprintf("%d年%d月%d日", t.Year(), t.Month(), t.Day())
	short := w
	if h < short {
		short = h
	}
	size := short * 0.045 * 72 / 25.4
	if err := setPdfTextFont(pdf, size); err != nil {
		pdf.SetError(err)
		return
	}
	inset := short * 0.04
	if textW := pdf.GetStringWidth(text); textW > w-2*inset {
		size *= (w - 2*inset) / textW
		pdf.SetFontSize(size)
	}
	tx := x + w - inset - pdf.GetStringWidth(text)
	ty := y + h - inset
	shadow := short * 0.003
	pdf.SetTextColor(60, 35, 15)
	pdf.Text(tx+shadow, ty+shadow, text)
	pdf.SetTextColor(235, 155, 65)
	pdf.Text(tx, ty, text)
	pdf.SetTextColor(0, 0, 0)
}

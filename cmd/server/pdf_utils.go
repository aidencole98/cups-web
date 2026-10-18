package main

import (
	"bufio"
	"errors"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"math"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/phpdave11/gofpdf"
	"golang.org/x/image/draw"

	"cups-web/internal/ipp"
)

const pdfPageMarginMM = 10.0

// 显式图片压缩和证件拼版使用的尺寸/编码策略。
// 普通照片关闭压缩时不使用该尺寸上限；3000px 不代表 A4/A3 的 300ppi。
const (
	imageDownscaleMaxEdge = 3000
	imageDownscaleJPEGQ   = 85
)

// downscaleSeq 为每个下采样输出文件分配一个进程内单调递增序号，彻底规避同目录文件名碰撞。
// 使用 atomic 保证并发安全（不同请求可能并发处理）。
var downscaleSeq uint64

// downscaleImageIfNeeded 在必要时把图片下采样到长边 imageDownscaleMaxEdge 以内并以 JPEG 写出。
// 返回值：
//   - outPath：可供 gofpdf 读取的图片路径（当未缩放时为原 inputPath；已缩放时为 tmpDir 下的新 JPEG）
//   - cfg：最终用于布局计算的尺寸信息（Width/Height 已反映缩放结果）
//   - err：任何 I/O / 解码 / 编码错误
//
// 规则：
//  1. 只有长边严格大于阈值时才执行缩放；小图原样返回，避免二次有损编码。
//  2. 为了统一输出格式（减小 PDF 体积、避免 gofpdf 对 PNG 透明度的处理分支），缩放后一律编码为 JPEG。
//     JPEG 不支持透明通道，因此在绘制前先把目标画布整体填白，再把源图用 draw.Over 合成上去，
//     这样带 alpha 的 PNG 也能得到"白底 + 前景"的正确打印效果，而不是黑底。
//  3. 缩放算法使用 CatmullRom（质量 / 性能 折中），比 draw.ApproxBiLinear 锐利，比 draw.BiLinear 清晰。
func downscaleImageIfNeeded(inputPath string, tmpDir string) (string, image.Config, error) {
	// 先读尺寸，避免对小图做无谓的整图解码
	f, err := os.Open(inputPath)
	if err != nil {
		return "", image.Config{}, err
	}
	cfg, _, err := image.DecodeConfig(f)
	_ = f.Close()
	if err != nil {
		return "", image.Config{}, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return "", image.Config{}, errors.New("invalid image dimensions")
	}

	longEdge := cfg.Width
	if cfg.Height > longEdge {
		longEdge = cfg.Height
	}
	if longEdge <= imageDownscaleMaxEdge {
		// 尺寸够小，直接沿用原图，不做任何转码
		return inputPath, cfg, nil
	}

	// 需要缩放：先整图解码，再按比例缩到目标尺寸
	srcFile, err := os.Open(inputPath)
	if err != nil {
		return "", image.Config{}, err
	}
	srcImg, _, err := image.Decode(srcFile)
	_ = srcFile.Close()
	if err != nil {
		return "", image.Config{}, err
	}

	scale := float64(imageDownscaleMaxEdge) / float64(longEdge)
	dstW := int(math.Round(float64(cfg.Width) * scale))
	dstH := int(math.Round(float64(cfg.Height) * scale))
	if dstW < 1 {
		dstW = 1
	}
	if dstH < 1 {
		dstH = 1
	}

	// 目标画布先整体填白：这样 PNG 透明区域在 JPEG 输出里会表现为白色，符合打印预期。
	dstImg := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	draw.Draw(dstImg, dstImg.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	// 再把缩放后的源图以 Over 模式叠加到白底上，透明像素透出白色
	draw.CatmullRom.Scale(dstImg, dstImg.Bounds(), srcImg, srcImg.Bounds(), draw.Over, nil)

	seq := atomic.AddUint64(&downscaleSeq, 1)
	outPath := filepath.Join(tmpDir, "downscaled_"+itoa(int(seq))+".jpg")
	outFile, err := os.Create(outPath)
	if err != nil {
		return "", image.Config{}, err
	}
	if err := jpeg.Encode(outFile, dstImg, &jpeg.Options{Quality: imageDownscaleJPEGQ}); err != nil {
		_ = outFile.Close()
		_ = os.Remove(outPath)
		return "", image.Config{}, err
	}
	if err := outFile.Close(); err != nil {
		_ = os.Remove(outPath)
		return "", image.Config{}, err
	}

	log.Printf("downscaleImage: %s %dx%d -> %dx%d (jpeg q=%d)",
		filepath.Base(inputPath), cfg.Width, cfg.Height, dstW, dstH, imageDownscaleJPEGQ)

	// 返回更新后的尺寸，供上层布局计算使用
	newCfg := image.Config{Width: dstW, Height: dstH, ColorModel: cfg.ColorModel}
	return outPath, newCfg, nil
}

// applyImageTransforms 使用无损 PNG 保存显式像素变换，无变换时返回原文件。
// 照片排版的旋转由 PDF 矩阵承担，此处只为反色处理像素，不损失 JPEG 编码质量。
func applyImageTransforms(inputPath string, tmpDir string, quarters int, invert bool) (string, image.Config, error) {
	quarters = ((quarters % 4) + 4) % 4
	if quarters == 0 && !invert {
		f, err := os.Open(inputPath)
		if err != nil {
			return "", image.Config{}, err
		}
		cfg, _, err := image.DecodeConfig(f)
		_ = f.Close()
		return inputPath, cfg, err
	}

	srcFile, err := os.Open(inputPath)
	if err != nil {
		return "", image.Config{}, err
	}
	src, _, err := image.Decode(srcFile)
	_ = srcFile.Close()
	if err != nil {
		return "", image.Config{}, err
	}
	sb := src.Bounds()
	rgba := image.NewRGBA(image.Rect(0, 0, sb.Dx(), sb.Dy()))
	draw.Draw(rgba, rgba.Bounds(), src, sb.Min, draw.Src)

	if invert {
		invertRGBA(rgba)
	}
	out := rgba
	if quarters != 0 {
		out = rotateRGBA(rgba, quarters)
	}

	ob := out.Bounds()
	seq := atomic.AddUint64(&downscaleSeq, 1)
	outPath := filepath.Join(tmpDir, "xform_"+itoa(int(seq))+".png")
	outFile, err := os.Create(outPath)
	if err != nil {
		return "", image.Config{}, err
	}
	if err := png.Encode(outFile, out); err != nil {
		_ = outFile.Close()
		_ = os.Remove(outPath)
		return "", image.Config{}, err
	}
	if err := outFile.Close(); err != nil {
		_ = os.Remove(outPath)
		return "", image.Config{}, err
	}
	return outPath, image.Config{Width: ob.Dx(), Height: ob.Dy()}, nil
}

// invertRGBA 原地对 RGBA 图像做反色（负片）：RGB 三通道 255-c，alpha 通道不变。
func invertRGBA(img *image.RGBA) {
	pix := img.Pix
	for i := 0; i < len(pix); i += 4 {
		pix[i] = 255 - pix[i]
		pix[i+1] = 255 - pix[i+1]
		pix[i+2] = 255 - pix[i+2]
	}
}

// rotateRGBA 对 RGBA 图像做正交旋转（quarters 个 90°顺时针）。
// quarters: 1=90°CW, 2=180°, 3=270°CW(=90°CCW)。
// 用像素字节搬移实现，无插值、无损；90°/270° 时输出宽高互换。
func rotateRGBA(src *image.RGBA, quarters int) *image.RGBA {
	sb := src.Bounds()
	w, h := sb.Dx(), sb.Dy()
	var dst *image.RGBA
	if quarters == 1 || quarters == 3 {
		dst = image.NewRGBA(image.Rect(0, 0, h, w))
	} else {
		dst = image.NewRGBA(image.Rect(0, 0, w, h))
	}
	sstride := src.Stride
	dstride := dst.Stride
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch quarters {
			case 1: // 90° CW
				dx = h - 1 - y
				dy = x
			case 2: // 180°
				dx = w - 1 - x
				dy = h - 1 - y
			case 3: // 270° CW
				dx = y
				dy = w - 1 - x
			}
			si := y*sstride + x*4
			di := dy*dstride + dx*4
			dst.Pix[di] = src.Pix[si]
			dst.Pix[di+1] = src.Pix[si+1]
			dst.Pix[di+2] = src.Pix[si+2]
			dst.Pix[di+3] = src.Pix[si+3]
		}
	}
	return dst
}

// paperSizeToGofpdf 将纸张大小名称映射到 gofpdf 参数
// 返回：gofpdf 认识的标准名称（或空字符串表示自定义）、自定义尺寸（如果是自定义纸张）
func paperSizeToGofpdf(size string) (string, gofpdf.SizeType) {
	if w, h, ok := ipp.ParsePaperDimensions(size); ok {
		return "", gofpdf.SizeType{Wd: w, Ht: h}
	}
	switch size {
	case "A5":
		return "A5", gofpdf.SizeType{}
	case "A4":
		return "A4", gofpdf.SizeType{}
	case "A3":
		return "A3", gofpdf.SizeType{}
	case "A2":
		return "A2", gofpdf.SizeType{}
	case "A1":
		return "A1", gofpdf.SizeType{}
	case "Letter":
		return "Letter", gofpdf.SizeType{}
	case "Legal":
		return "Legal", gofpdf.SizeType{}
	case "5inch":
		// 89×127 mm
		return "", gofpdf.SizeType{Wd: 89, Ht: 127}
	case "6inch":
		// 102×152 mm
		return "", gofpdf.SizeType{Wd: 102, Ht: 152}
	case "7inch":
		// 127×178 mm
		return "", gofpdf.SizeType{Wd: 127, Ht: 178}
	case "8inch":
		// 152×203 mm
		return "", gofpdf.SizeType{Wd: 152, Ht: 203}
	case "10inch":
		// 203×254 mm
		return "", gofpdf.SizeType{Wd: 203, Ht: 254}
	default:
		// 默认使用 A4
		return "A4", gofpdf.SizeType{}
	}
}

// getOrientationCode 将方向字符串转换为 gofpdf 方向代码
func getOrientationCode(orientation string) string {
	if orientation == "landscape" {
		return "L"
	}
	// 默认纵向
	return "P"
}

type imagePDFOptions struct {
	PhotoDateWatermark bool
	PhotoDates         []string
	Borderless         bool
	PrintScaling       string
}

// drawPDFImage shares the same photo layout for a whole page and a grid cell.
// fill enlarges without stretching, then clips at the content area's edges.
// Other scaling modes retain the existing fit layout before CUPS processing.
func drawPDFImage(pdf *gofpdf.Fpdf, path string, cfg image.Config, x, y, width, height float64, scaling string, date string, rotations ...int) {
	if width <= 0 || height <= 0 || cfg.Width <= 0 || cfg.Height <= 0 {
		pdf.SetErrorf("invalid image content area")
		return
	}
	rotation := 0
	if len(rotations) > 0 {
		rotation = rotations[0]
	}
	if rotation == 90 || rotation == 270 {
		cfg.Width, cfg.Height = cfg.Height, cfg.Width
	}
	scale := math.Min(width/float64(cfg.Width), height/float64(cfg.Height))
	if scaling == "fill" {
		scale = math.Max(width/float64(cfg.Width), height/float64(cfg.Height))
		pdf.ClipRect(x, y, width, height, false)
	}
	w, h := float64(cfg.Width)*scale, float64(cfg.Height)*scale
	// fill can place a wide photo left of the page. Without this flag gofpdf
	// treats a negative x as the current cursor position, breaking centering.
	opts := gofpdf.ImageOptions{ReadDpi: true, AllowNegativePosition: true}
	ix, iy := x+(width-w)/2, y+(height-h)/2
	if rotation == 0 {
		pdf.ImageOptions(path, ix, iy, w, h, false, opts, 0, "")
	} else {
		iw, ih := w, h
		if rotation == 90 || rotation == 270 {
			iw, ih = h, w
		}
		cx, cy := ix+w/2, iy+h/2
		pdf.TransformBegin()
		pdf.TransformRotate(-float64(rotation), cx, cy)
		pdf.ImageOptions(path, cx-iw/2, cy-ih/2, iw, ih, false, opts, 0, "")
		pdf.TransformEnd()
	}
	if scaling == "fill" {
		pdf.ClipEnd()
	}
	if scaling == "fill" {
		drawPhotoDate(pdf, date, x, y, width, height)
	} else {
		drawPhotoDate(pdf, date, ix, iy, w, h)
	}
}

func convertImageToPDF(inputPath string, orientation string, paperSize string, invert bool, compress bool, options ...imagePDFOptions) (string, func(), error) {
	return convertImagePathsToPDF([]string{inputPath}, orientation, paperSize, nil, invert, 1, compress, options...)
}

func convertTextToPDF(inputPath string, orientation string, paperSize string) (string, func(), error) {
	tmpDir, err := os.MkdirTemp("", "convert-text-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }

	f, err := os.Open(inputPath)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	defer f.Close()

	// 处理方向和纸张大小
	orientCode := getOrientationCode(orientation)
	paperName, customSize := paperSizeToGofpdf(paperSize)

	var pdf *gofpdf.Fpdf
	if paperName != "" {
		// 使用标准纸张
		pdf = gofpdf.New(orientCode, "mm", paperName, "")
	} else {
		// 使用自定义尺寸
		pdf = gofpdf.NewCustom(&gofpdf.InitType{
			UnitStr:        "mm",
			Size:           customSize,
			OrientationStr: orientCode,
		})
	}

	pdf.SetMargins(pdfPageMarginMM, pdfPageMarginMM, pdfPageMarginMM)
	pdf.SetAutoPageBreak(false, pdfPageMarginMM)
	pdf.AddPage()
	if err := setPdfTextFont(pdf, 10); err != nil {
		cleanup()
		return "", nil, err
	}

	_, pageH := pdf.GetPageSize()
	lineHeight := (pageH - 2*pdfPageMarginMM) / float64(textLinesPerPage)
	if lineHeight <= 0 {
		lineHeight = 4
	}

	pageW, _ := pdf.GetPageSize()
	cellW := pageW - 2*pdfPageMarginMM

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineIndex := 0
	for scanner.Scan() {
		if lineIndex >= textLinesPerPage {
			pdf.AddPage()
			lineIndex = 0
		}
		y := pdfPageMarginMM + lineHeight*float64(lineIndex)
		pdf.SetXY(pdfPageMarginMM, y)
		pdf.CellFormat(cellW, lineHeight, scanner.Text(), "", 0, "LM", false, 0, "")
		lineIndex++
	}
	if err := scanner.Err(); err != nil {
		cleanup()
		return "", nil, err
	}

	outPath := filepath.Join(tmpDir, "text.pdf")
	if err := pdf.OutputFileAndClose(outPath); err != nil {
		cleanup()
		return "", nil, err
	}
	return outPath, cleanup, nil
}

// convertImagesMultiToPDF 将多张图片合并为单个 PDF。
// 每张图片占据一页,按等比例缩放居中绘制,页面大小与方向由 orientation / paperSize 决定。
// perPage 支持把多张图片排到同一页(issue #37,类 WinXP 打印图片向导):
//
//	1(默认) → 每张一页
//	2/4/6/9 → 每页对应张数,按行优先的等分网格居中
//
// 非法或 <=1 的 perPage 一律按 1 处理。
// 调用方负责在使用完输出 PDF 后调用返回的 cleanup 清理临时目录。
func convertImagesMultiToPDF(fileHeaders []*multipart.FileHeader, orientation string, paperSize string, rotations []int, invert bool, perPage int, compress bool, options ...imagePDFOptions) (string, func(), error) {
	if len(fileHeaders) == 0 {
		return "", nil, errors.New("no image files provided")
	}
	tmpDir, err := os.MkdirTemp("", "photo-inputs-")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(tmpDir)
	paths := make([]string, 0, len(fileHeaders))
	for idx, fh := range fileHeaders {
		src, err := fh.Open()
		if err != nil {
			return "", nil, err
		}
		ext := strings.ToLower(filepath.Ext(fh.Filename))
		path := filepath.Join(tmpDir, "img_"+itoa(idx)+ext)
		dst, err := os.Create(path)
		if err != nil {
			src.Close()
			return "", nil, err
		}
		_, copyErr := io.Copy(dst, src)
		closeErr := dst.Close()
		src.Close()
		if copyErr != nil {
			return "", nil, copyErr
		}
		if closeErr != nil {
			return "", nil, closeErr
		}
		paths = append(paths, path)
	}
	return convertImagePathsToPDF(paths, orientation, paperSize, rotations, invert, perPage, compress, options...)
}

// Both preview and printing use original paths and the same photo layout.
func convertImagePathsToPDF(paths []string, orientation, paperSize string, rotations []int, invert bool, perPage int, compress bool, options ...imagePDFOptions) (string, func(), error) {
	var layout imagePDFOptions
	if len(options) > 0 {
		layout = options[0]
	}
	if len(paths) == 0 {
		return "", nil, errors.New("no image files provided")
	}
	tmpDir, err := os.MkdirTemp("", "convert-imgs-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }
	type savedImage struct {
		date, path string
		cfg        image.Config
		rotation   int
	}
	saved := make([]savedImage, 0, len(paths))
	for idx, path := range paths {
		date := resolvedPhotoDate(path, layout.PhotoDateWatermark, layout.PhotoDates, idx)
		// Inversion uses a lossless intermediate. Rotation stays in the PDF matrix.
		finalPath, cfg, err := applyImageTransforms(path, tmpDir, 0, invert)
		if err != nil {
			cleanup()
			return "", nil, err
		}
		if compress {
			finalPath, cfg, err = compressImageIfNeeded(finalPath, tmpDir)
			if err != nil {
				cleanup()
				return "", nil, err
			}
		}
		rotation := 0
		if idx < len(rotations) {
			rotation = ((rotations[idx] % 360) + 360) % 360
		}
		saved = append(saved, savedImage{path: finalPath, cfg: cfg, date: date, rotation: rotation})
	}

	// 构造 PDF
	marginMM := pdfPageMarginMM
	if layout.Borderless {
		marginMM = 0
	}
	pdf := newPhotoPDF(orientation, paperSize)
	if layout.PrintScaling == "fill" {
		pdf.SetKeywords(photoFillLayoutKeyword, false)
	}
	pdf.SetMargins(marginMM, marginMM, marginMM)
	pdf.SetAutoPageBreak(false, marginMM)

	cols, rows := gridDimsForPerPage(perPage)
	slotsPerPage := cols * rows

	// 网格间距:格子之间留 4mm 视觉分隔,过大会把每张图挤得太小,过小又贴在一起。
	const gapMM = 4.0

	drawImage := func(img savedImage, slotX, slotY, slotW, slotH float64) {
		drawPDFImage(pdf, img.path, img.cfg, slotX, slotY, slotW, slotH, layout.PrintScaling, img.date, img.rotation)
	}

	for i, img := range saved {
		slot := i % slotsPerPage
		if slot == 0 {
			pdf.AddPage()
		}
		pageW, pageH := pdf.GetPageSize()
		maxW := pageW - 2*marginMM
		maxH := pageH - 2*marginMM

		if slotsPerPage == 1 {
			drawImage(img, marginMM, marginMM, maxW, maxH)
			continue
		}
		// 按行优先(row-major)分配:slot=0 在左上,横向填满一行再换行。
		col := slot % cols
		row := slot / cols
		slotW := (maxW - float64(cols-1)*gapMM) / float64(cols)
		slotH := (maxH - float64(rows-1)*gapMM) / float64(rows)
		slotX := marginMM + float64(col)*(slotW+gapMM)
		slotY := marginMM + float64(row)*(slotH+gapMM)
		drawImage(img, slotX, slotY, slotW, slotH)
	}

	outPath := filepath.Join(tmpDir, "images.pdf")
	if err := pdf.OutputFileAndClose(outPath); err != nil {
		cleanup()
		return "", nil, err
	}
	return outPath, cleanup, nil
}

// gridDimsForPerPage 把 perPage 映射成 cols × rows 布局(row-major)。
// 只支持 issue #37 覆盖的常用张数;其他值一律回退到 1×1(每张一页)。
func gridDimsForPerPage(perPage int) (cols, rows int) {
	switch perPage {
	case 2:
		return 1, 2
	case 4:
		return 2, 2
	case 6:
		return 2, 3
	case 9:
		return 3, 3
	default:
		return 1, 1
	}
}

// itoa 是一个小助手，避免再引入 strconv 仅为了格式化索引
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

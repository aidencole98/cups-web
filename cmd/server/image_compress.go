package main

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"

	"golang.org/x/image/draw"
)

// 每张图片的编码大小上限（十进制 800KB），不是 PDF 或打印机内存上限。
const imageCompressionMaxBytes = 800_000

// compressImageIfNeeded 在旋转/反色之后执行，避免后续 JPEG 重编码突破上限。
// 先降低质量，仍超限时缩小尺寸；任何失败都返回错误，不静默发送超限图片。
func compressImageIfNeeded(inputPath, tmpDir string) (string, image.Config, error) {
	f, err := os.Open(inputPath)
	if err != nil {
		return "", image.Config{}, err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return "", image.Config{}, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return "", image.Config{}, errors.New("invalid image dimensions")
	}
	info, err := f.Stat()
	if err != nil {
		return "", image.Config{}, err
	}
	if info.Size() <= imageCompressionMaxBytes {
		return inputPath, cfg, nil
	}
	if _, err := f.Seek(0, 0); err != nil {
		return "", image.Config{}, err
	}
	src, _, err := image.Decode(f)
	if err != nil {
		return "", image.Config{}, err
	}
	w, h := cfg.Width, cfg.Height
	scale := math.Min(1, float64(imageDownscaleMaxEdge)/float64(max(w, h)))
	w, h = max(1, int(math.Round(float64(w)*scale))), max(1, int(math.Round(float64(h)*scale)))
	var encoded bytes.Buffer
	for {
		dst := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
		draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
		for quality := imageDownscaleJPEGQ; quality >= 45; quality -= 10 {
			encoded.Reset()
			if err := jpeg.Encode(&encoded, dst, &jpeg.Options{Quality: quality}); err != nil {
				return "", image.Config{}, err
			}
			if encoded.Len() <= imageCompressionMaxBytes {
				seq := atomic.AddUint64(&downscaleSeq, 1)
				outPath := filepath.Join(tmpDir, "compressed_"+itoa(int(seq))+".jpg")
				if err := os.WriteFile(outPath, encoded.Bytes(), 0600); err != nil {
					_ = os.Remove(outPath)
					return "", image.Config{}, err
				}
				return outPath, image.Config{Width: w, Height: h, ColorModel: color.YCbCrModel}, nil
			}
		}
		if w == 1 && h == 1 {
			return "", image.Config{}, errors.New("无法将图片压缩至 800KB 以下")
		}
		w, h = max(1, w*4/5), max(1, h*4/5)
	}
}

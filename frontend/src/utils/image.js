// 客户端图片下采样工具。
//
// 背景（Issue #42）：
// 移动端拍摄的原图常常 >10MiB，多张合并成 PDF 时累计 POST 体积容易撞到反向代理
// 的 client_max_body_size（nginx 默认 1MiB），返回 HTTP 413 "Request Entity Too Large"，
// 前端表现为"服务端转换失败：Request Entity Too Large"。
// 由于反向代理是用户自己的部署环境，无法在容器内修复，最稳妥的办法是上传前在浏览器
// 端先把超大图压成合理尺寸，让请求体直接落到 1MiB 以下。
//
// 阈值与后端 cmd/server/pdf_utils.go 的 imageDownscaleMaxEdge / imageDownscaleJPEGQ
// 对齐：长边 3000px / JPEG 质量 0.85。这是 A4/A3 在 300dpi 下的合理上限，画质损失
// 在打印场景里可忽略。

const MAX_EDGE = 3000
const JPEG_QUALITY = 0.85
const MAX_BYTES = 800_000

// 默认长边 ≤ MAX_EDGE 的图片原样返回，避免无意义的二次有损编码。
// 解码 / 绘制 / 编码任一步失败时也回退到原文件，让后端按原路径处理（最差情况
// 退化到旧行为，不会比修复前更糟）。
// compress=true 时额外保证编码大小 ≤ 800KB，无法压缩则报错，不能回退超限原图。
export async function downscaleImageIfNeeded(file, compress = false) {
  if (!file || typeof file !== 'object') return file
  // 仅处理图片：HEIC 已在上层 heic2any 转成 JPEG，所以这里只看 image/* 即可。
  const type = (file.type || '').toLowerCase()
  if (!type.startsWith('image/')) return file

  let bitmap
  try {
    bitmap = await createImageBitmap(file)
  } catch (e) {
    if (compress) throw new Error('图片解码失败，无法压缩：' + file.name)
    console.warn('[downscaleImageIfNeeded] createImageBitmap failed, fallback to original:', e)
    return file
  }

  const longEdge = Math.max(bitmap.width, bitmap.height)
  if (longEdge <= MAX_EDGE && (!compress || file.size <= MAX_BYTES)) {
    bitmap.close?.()
    return file
  }

  const scale = Math.min(1, MAX_EDGE / longEdge)
  const dstW = Math.max(1, Math.round(bitmap.width * scale))
  const dstH = Math.max(1, Math.round(bitmap.height * scale))

  const canvas = document.createElement('canvas')
  canvas.width = dstW
  canvas.height = dstH
  const ctx = canvas.getContext('2d')
  if (!ctx) {
    bitmap.close?.()
    if (compress) throw new Error('浏览器无法处理图片压缩')
    return file
  }
  // 与后端一致：JPEG 不支持透明，先整体填白再叠图，PNG 透明区在输出里表现为白色。
  let blob
  try {
    while (true) {
      ctx.fillStyle = '#ffffff'
      ctx.fillRect(0, 0, canvas.width, canvas.height)
      ctx.drawImage(bitmap, 0, 0, canvas.width, canvas.height)
      for (const quality of (compress ? [0.85, 0.75, 0.65, 0.55, 0.45] : [JPEG_QUALITY])) {
        blob = await new Promise(resolve => canvas.toBlob(resolve, 'image/jpeg', quality))
        if (!blob) {
          if (compress) throw new Error('图片编码失败，无法压缩：' + file.name)
          return file
        }
        if (!compress || blob.size <= MAX_BYTES) break
      }
      if (!compress || blob.size <= MAX_BYTES) break
      if (canvas.width === 1 && canvas.height === 1) throw new Error('无法将图片压缩至 800KB 以下')
      canvas.width = Math.max(1, Math.floor(canvas.width * 0.8))
      canvas.height = Math.max(1, Math.floor(canvas.height * 0.8))
    }
  } finally {
    bitmap.close?.()
  }

  // 原文件名保留语义但改成 .jpg 后缀（已经是 JPEG 了）
  const baseName = (file.name || 'image').replace(/\.[^/.]+$/, '')
  return new File([blob], baseName + '.jpg', {
    type: 'image/jpeg',
    lastModified: Date.now(),
  })
}

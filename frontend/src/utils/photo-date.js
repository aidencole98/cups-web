// Read only EXIF DateTimeOriginal before canvas conversion discards metadata.
// Chunk reads are bounded; image pixels and unrelated metadata are never decoded.
const MAX_EXIF_BYTES = 256 << 10
const cache = new WeakMap()

function validDate(date) {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(date)) return false
  const [y, m, d] = date.split('-').map(Number)
  const t = new Date(0)
  t.setUTCFullYear(y, m - 1, d)
  return y > 0 && t.getUTCFullYear() === y && t.getUTCMonth() === m - 1 && t.getUTCDate() === d
}

function tiffDate(bytes) {
  if (bytes.length < 8) return ''
  const v = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength)
  const little = bytes[0] === 0x49 && bytes[1] === 0x49
  if (!little && !(bytes[0] === 0x4d && bytes[1] === 0x4d)) return ''
  const u16 = p => v.getUint16(p, little)
  const u32 = p => v.getUint32(p, little)
  if (u16(2) !== 42) return ''
  const entry = (p, tag) => {
    if (p < 8 || p + 2 > bytes.length) return null
    const n = u16(p)
    if (n > 4096 || p + 2 + n * 12 > bytes.length) return null
    for (let i = 0; i < n; i++) {
      const e = p + 2 + i * 12
      if (u16(e) === tag) return e
    }
    return null
  }
  const pointer = entry(u32(4), 0x8769)
  if (pointer === null || u16(pointer + 2) !== 4 || u32(pointer + 4) !== 1) return ''
  const e = entry(u32(pointer + 8), 0x9003)
  if (e === null || u16(e + 2) !== 2 || u32(e + 4) !== 20) return ''
  const p = u32(e + 8)
  if (p + 20 > bytes.length || bytes[p + 19] !== 0) return ''
  const s = String.fromCharCode(...bytes.subarray(p, p + 19))
  if (!/^\d{4}:\d{2}:\d{2} \d{2}:\d{2}:\d{2}$/.test(s)) return ''
  const date = s.slice(0, 10).replaceAll(':', '-')
  const [h, m, sec] = s.slice(11).split(':').map(Number)
  return validDate(date) && h < 24 && m < 60 && sec < 60 ? date : ''
}

async function readDate(file) {
  const read = async (p, n) => {
    if (p < 0 || p + n > file.size) throw new Error('truncated metadata')
    return new Uint8Array(await file.slice(p, p + n).arrayBuffer())
  }
  const exif = async (p, n, jpeg) => {
    if (n < 8 || n > MAX_EXIF_BYTES) return ''
    let b = await read(p, n)
    if (jpeg) {
      if (String.fromCharCode(...b.subarray(0, 6)) !== 'Exif\x00\x00') return ''
      b = b.subarray(6)
    }
    return tiffDate(b)
  }
  try {
    const head = await read(0, 8)
    if (head[0] === 0xff && head[1] === 0xd8) {
      let p = 2
      for (let i = 0; i < 1024; i++) {
        const marker = await read(p, 2)
        if (marker[0] !== 0xff) return ''
        if (marker[1] === 0xff) { p++; continue }
        if (marker[1] === 0xda || marker[1] === 0xd9) return ''
        if (marker[1] === 1 || (marker[1] >= 0xd0 && marker[1] <= 0xd7)) { p += 2; continue }
        const size = await read(p + 2, 2)
        const n = (size[0] << 8) | size[1]
        if (n < 2) return ''
        if (marker[1] === 0xe1) {
          const date = await exif(p + 4, n - 2, true)
          if (date) return date
        }
        p += n + 2
      }
    } else if (String.fromCharCode(...head) === '\x89PNG\r\n\x1a\n') {
      let p = 8
      for (let i = 0; i < 1024; i++) {
        const chunk = await read(p, 8)
        const n = new DataView(chunk.buffer).getUint32(0)
        if (n > 0x7fffffff) return ''
        const type = String.fromCharCode(...chunk.subarray(4))
        if (type === 'eXIf') return await exif(p + 8, n, false)
        if (type === 'IEND') return ''
        p += n + 12
      }
    }
  } catch { /* Missing or malformed EXIF does not block printing. */ }
  return ''
}

export function readPhotoDate(file) {
  if (!file || typeof file.slice !== 'function') return Promise.resolve('')
  if (!cache.has(file)) cache.set(file, readDate(file))
  return cache.get(file)
}

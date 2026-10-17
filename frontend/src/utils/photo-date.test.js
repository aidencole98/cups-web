import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readPhotoDate } from './photo-date.js'

function tiff(date, little = true) {
  const b = new Uint8Array(64)
  const v = new DataView(b.buffer)
  b.set(little ? [73, 73] : [77, 77])
  const u16 = (p, n) => v.setUint16(p, n, little)
  const u32 = (p, n) => v.setUint32(p, n, little)
  u16(2, 42); u32(4, 8); u16(8, 1)
  u16(10, 0x8769); u16(12, 4); u32(14, 1); u32(18, 26)
  u16(26, 1); u16(28, 0x9003); u16(30, 2); u32(32, 20); u32(36, 44)
  b.set(new TextEncoder().encode(date), 44)
  return b
}

function jpeg(metadata) {
  const size = metadata.length + 8
  return new Blob([new Uint8Array([255, 216, 255, 225, size >> 8, size & 255]), 'Exif\x00\x00', metadata, new Uint8Array([255, 217])])
}

function png(metadata) {
  const chunk = new Uint8Array(8)
  new DataView(chunk.buffer).setUint32(0, metadata.length)
  chunk.set(new TextEncoder().encode('eXIf'), 4)
  return new Blob([new Uint8Array([137, 80, 78, 71, 13, 10, 26, 10]), chunk, metadata, new Uint8Array(4)])
}

test('JPEG and PNG retain each original date before browser compression', async () => {
  for (const little of [true, false]) {
    for (const wrap of [jpeg, png]) {
      const files = [wrap(tiff('2026:10:03 23:59:59', little)), wrap(tiff('2024:02:29 00:00:00', little))]
      assert.deepEqual(await Promise.all(files.map(readPhotoDate)), ['2026-10-03', '2024-02-29'])
      // Cached extraction stays associated with each File when the UI reorders it.
      assert.equal(await readPhotoDate(files[1]), '2024-02-29')
    }
  }
})

test('missing, malformed, modified-only and impossible dates are skipped', async () => {
  for (const date of ['2026:02:29 00:00:00', '0000:01:01 00:00:00', '2026:10:03 24:00:00', '2026:10:03 00:00:60']) {
    assert.equal(await readPhotoDate(jpeg(tiff(date))), '')
  }
  const broken = tiff('2026:10:03 00:00:00')
  new DataView(broken.buffer).setUint32(36, 0xffffffff, true)
  assert.equal(await readPhotoDate(jpeg(broken)), '')
  const modified = tiff('2026:10:03 00:00:00')
  new DataView(modified.buffer).setUint16(28, 0x0132, true)
  assert.equal(await readPhotoDate(jpeg(modified)), '')
  const valid = jpeg(tiff('2026:10:03 00:00:00'))
  for (let n = 0; n < valid.size - 2; n++) assert.equal(await readPhotoDate(valid.slice(0, n)), '')
  assert.equal(await readPhotoDate(new Blob(['GIF89a'])), '')
  assert.equal(await readPhotoDate(null), '')
})

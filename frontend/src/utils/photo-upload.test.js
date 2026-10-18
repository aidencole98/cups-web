import { test } from 'node:test'
import assert from 'node:assert/strict'
import { appendPhotoUploads, isPhotoFile } from './photo-upload.js'

test('photo recognition accepts missing MIME with a supported photo extension', () => {
  for (const name of ['one.jpg', 'two.JPEG', 'three.png', 'four.gif', 'five.heic']) {
    assert.equal(isPhotoFile(new File(['photo'], name)), true)
  }
  assert.equal(isPhotoFile(new File(['photo'], 'unknown', { type: 'image/jpeg' })), true)
  assert.equal(isPhotoFile(new File(['document'], 'one.pdf')), false)
  assert.equal(isPhotoFile(null), false)
})

test('photo uploads preserve original bytes and image layout independently of PDF previews', async () => {
  const data = new Uint8Array([255, 216, 255, 225, 12, 34, 56, 78])
  const files = ['one.jpg', 'two.jpg'].map(name => new File([data], name, { type: 'image/jpeg' }))
  const form = new FormData()
  appendPhotoUploads(form, files, { rotations: [0, 90], perPage: 2, name: '照片.pdf' })
  assert.equal(form.has('file'), false)
  assert.equal(form.get('rotations'), '0,90')
  assert.equal(form.get('per_page'), '2')
  assert.equal(form.get('name'), '照片.pdf')
  const uploaded = form.getAll('files')
  assert.equal(uploaded.length, 2)
  for (let i = 0; i < files.length; i++) {
    assert.equal(uploaded[i].name, files[i].name)
    assert.equal(uploaded[i].type, files[i].type)
    assert.deepEqual(new Uint8Array(await uploaded[i].arrayBuffer()), data)
  }
  const single = new FormData()
  appendPhotoUploads(single, files.slice(0, 1), { rotations: null })
  assert.equal(single.has('files'), false)
  assert.equal(single.get('file').name, 'one.jpg')
  assert.equal(single.get('rotations'), '0')
  assert.equal(single.get('per_page'), '1')
})

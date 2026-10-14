<template>
  <div class="p-3 sm:p-4 md:p-6 max-w-5xl mx-auto space-y-4">
    <div class="flex items-center justify-between gap-2 flex-wrap">
      <h1 class="text-lg font-bold flex items-center gap-2">
        <UIcon name="i-lucide-calendar-clock" class="w-5 h-5 text-primary" />
        定时打印
      </h1>
      <div class="flex items-center gap-2">
        <UButton
          variant="ghost"
          size="xs"
          icon="i-lucide-refresh-cw"
          :loading="loading"
          @click="loadRecords()"
        >刷新</UButton>
        <UButton
          size="sm"
          color="primary"
          icon="i-lucide-plus"
          @click="openCreate"
        >新建定时任务</UButton>
      </div>
    </div>

    <UAlert
      color="neutral"
      variant="soft"
      icon="i-lucide-info"
      title="使用说明"
      description="任务按服务器本地时区触发，执行记录按浏览器本地时间显示。错过时间不补跑；照片队列会保留当前照片。"
    />

    <div v-if="loading" class="flex items-center justify-center py-10 text-muted gap-2">
      <UIcon name="i-lucide-loader-circle" class="w-5 h-5 animate-spin" />
      加载中…
    </div>
    <div v-else-if="!records.length" class="text-center py-16 text-muted">
      <UIcon name="i-lucide-calendar-off" class="w-10 h-10 mx-auto mb-2 opacity-40" />
      <div>还没有定时任务</div>
    </div>
    <div v-else class="space-y-3">
      <UCard v-for="rec in records" :key="rec.id" class="overflow-hidden">
        <div class="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-3">
          <div class="min-w-0 space-y-1">
            <div class="flex items-center gap-2 flex-wrap">
              <span class="font-medium truncate">{{ rec.name || rec.filename }}</span>
              <UBadge
                :color="rec.enabled || rec.completed ? 'success' : rec.running ? 'primary' : 'neutral'"
                :variant="rec.enabled ? 'subtle' : 'outline'"
                size="sm"
              >{{ rec.running ? '正在提交' : rec.completed ? '已完成' : rec.enabled ? '启用' : '已暂停' }}</UBadge>
              <UBadge color="primary" variant="outline" size="sm">
                {{ scheduleLabel(rec) }}
              </UBadge>
              <UBadge
                v-if="rec.lastStatus && !rec.running"
                :color="lastStatusColor(rec.lastStatus)"
                variant="soft"
                size="sm"
              >{{ lastStatusLabel(rec.lastStatus) }}</UBadge>
            </div>
            <div class="text-xs text-muted flex flex-wrap gap-x-4 gap-y-0.5">
              <span>文件：{{ rec.files?.length ? rec.files.length + ' 张照片' : rec.filename }}</span>
              <span>打印机：{{ shortPrinter(rec.printerUri) }}</span>
              <span>下次：{{ rec.completed ? '—' : formatTime(rec.nextRunAt) }}</span>
              <span v-if="rec.lastRunAt">上次：{{ formatTime(rec.lastRunAt) }}</span>
            </div>
            <div v-if="rec.lastError && !rec.running" class="text-xs text-error truncate">
              上次错误：{{ rec.lastError }}
            </div>
          </div>
          <div class="flex items-center gap-1 flex-shrink-0">
            <UButton size="xs" variant="ghost" icon="i-lucide-play" :loading="rec.running || rec._running" :disabled="rec.completed || rec.running" @click="runNow(rec)">{{ rec.files?.length ? '打印下一批' : '立即' }}</UButton>
            <UButton size="xs" variant="ghost" :icon="rec.enabled ? 'i-lucide-pause' : 'i-lucide-play-circle'" :disabled="rec.completed || rec.running" @click="toggleEnabled(rec)">
              {{ rec.enabled ? '暂停' : '恢复' }}
            </UButton>
            <UButton size="xs" variant="ghost" icon="i-lucide-pencil" :disabled="rec.running" @click="openEdit(rec)">编辑</UButton>
            <UButton size="xs" variant="ghost" color="error" icon="i-lucide-trash-2" :disabled="rec.running" @click="confirmDelete(rec)">删除</UButton>
          </div>
        </div>
        <div v-if="rec.files?.length" class="mt-4 space-y-3">
          <div class="flex items-center justify-between text-sm">
            <span>{{ rec.loop ? '本轮已提交' : '已提交' }} {{ rec.nextFileIndex }} / {{ rec.files.length }} 张
              <span v-if="rec.loop" class="text-muted"> · 累计 {{ rec.submittedCount }} 张</span>
            </span>
            <span class="text-muted">{{ Math.round(rec.nextFileIndex / rec.files.length * 100) }}%</span>
          </div>
          <div class="h-2 rounded-full bg-elevated overflow-hidden" role="progressbar"
            :aria-valuenow="rec.nextFileIndex" :aria-valuemax="rec.files.length" aria-valuemin="0" aria-label="本轮照片提交进度">
            <div class="h-full bg-primary rounded-full transition-all" :style="{ width: (rec.nextFileIndex / rec.files.length * 100) + '%' }" />
          </div>
          <div class="flex gap-2 overflow-x-auto pb-1">
            <div v-for="(photo, index) in rec.files" :key="photo.thumbnailUrl" class="w-24 shrink-0">
              <div class="relative rounded-lg overflow-hidden border-2"
                :class="index === rec.nextFileIndex && !rec.completed ? 'border-primary' : 'border-transparent'">
                <img :src="photo.thumbnailUrl" :alt="photo.filename" loading="lazy" class="w-full h-20 object-cover bg-elevated"
                  :class="index < rec.nextFileIndex ? 'opacity-60' : ''" />
                <span v-if="index < rec.nextFileIndex" class="absolute bottom-1 right-1 rounded-full bg-primary text-white p-0.5 flex">
                  <UIcon name="i-lucide-check" class="w-3 h-3" />
                </span>
                <span v-else-if="index === rec.nextFileIndex" class="absolute bottom-1 left-1 text-xs rounded bg-primary text-white px-1">下一张</span>
              </div>
              <div class="text-xs text-muted truncate mt-1" :title="photo.filename">{{ photo.filename }}</div>
            </div>
          </div>
          <div class="rounded-lg bg-elevated/50 px-3 py-2 text-xs text-muted space-y-1">
            <div v-if="!rec.completed">下一张：{{ rec.files[rec.nextFileIndex]?.filename }}</div>
            <div>{{ rec.loop ? '全部提交后从第一张继续循环。' : '照片全部提交后，任务自动结束。' }} 每次打印 {{ rec.photosPerRun || 1 }} 张，每张 {{ rec.copies }} 份；进度表示 CUPS 已接受任务。</div>
          </div>
          <details v-if="rec.history?.length" class="text-sm">
            <summary class="cursor-pointer text-muted">最近执行（{{ rec.history.length }}）</summary>
            <div class="mt-2 divide-y divide-default">
              <div v-for="(run, index) in rec.history" :key="index" class="flex items-start gap-3 py-2 text-xs">
                <span class="text-muted shrink-0">{{ formatTime(run.runAt) }}</span>
                <span class="min-w-0 flex-1 truncate" :title="run.filename">{{ run.filename }}</span>
                <span :class="run.status === 'failed' ? 'text-error' : 'text-muted'" :title="run.error">
                  {{ run.status === 'printed' ? '已提交' : run.status === 'skipped' ? '已跳过' : '失败' }}
                </span>
              </div>
            </div>
          </details>
        </div>
      </UCard>
    </div>

    <!-- 新建 / 编辑抽屉 -->
    <UModal v-model:open="modalOpen" :title="editing ? '编辑定时任务' : '新建定时任务'" :ui="{ content: 'sm:max-w-3xl' }">
      <template #body>
        <div class="space-y-3">
          <div class="grid grid-cols-1 sm:grid-cols-2 gap-3">
            <UFormField v-if="!editing" label="任务类型">
              <USelect v-model="form.mode" :items="modeItems" value-key="value" label-key="label" class="w-full" />
            </UFormField>
            <UFormField label="任务名称（可选）">
              <UInput class="w-full" v-model="form.name" placeholder="例如：每周会议纪要" />
            </UFormField>
          </div>

          <UFormField v-if="form.mode === 'queue'" label="照片队列" required>
            <div class="flex items-center justify-between text-xs text-muted mb-2">
              <span>已选择 {{ editing ? form.existingFiles.length : form.photos.length }} 张 · 按顺序打印</span>
              <label v-if="!editing" class="cursor-pointer text-primary hover:underline">
                ＋ 添加照片
                <input type="file" multiple accept="image/jpeg,image/png,image/gif" class="sr-only" @change="onPhotoPick" />
              </label>
            </div>
            <div v-if="!editing" class="grid grid-cols-3 sm:grid-cols-5 gap-2">
              <div v-for="(photo, index) in form.photos" :key="photo.key" draggable="true"
                class="rounded-lg border border-default p-1 cursor-grab"
                @dragstart="dragIndex = index" @dragover.prevent @drop.prevent="movePhoto(dragIndex, index)" @dragend="dragIndex = null">
                <div class="relative">
                  <img :src="photo.url" :alt="photo.file.name" class="w-full h-20 object-cover rounded bg-elevated" />
                  <span class="absolute bottom-1 left-1 bg-black/60 text-white text-xs px-1 rounded">{{ index + 1 }}</span>
                </div>
                <div class="text-xs truncate mt-1" :title="photo.file.name">{{ photo.file.name }}</div>
                <div class="flex justify-between mt-1">
                  <UButton size="xs" variant="ghost" icon="i-lucide-chevron-left" :disabled="index === 0" :aria-label="'前移 ' + photo.file.name" @click="movePhoto(index, index - 1)" />
                  <UButton size="xs" variant="ghost" icon="i-lucide-chevron-right" :disabled="index === form.photos.length - 1" :aria-label="'后移 ' + photo.file.name" @click="movePhoto(index, index + 1)" />
                  <UButton size="xs" variant="ghost" color="error" icon="i-lucide-x" :aria-label="'移除 ' + photo.file.name" @click="removePhoto(index)" />
                </div>
              </div>
            </div>
            <div v-else class="flex gap-2 overflow-x-auto">
              <div v-for="photo in form.existingFiles" :key="photo.thumbnailUrl" class="w-24 shrink-0">
                <img :src="photo.thumbnailUrl" :alt="photo.filename" class="w-full h-20 object-cover rounded" />
                <div class="text-xs truncate mt-1">{{ photo.filename }}</div>
              </div>
            </div>
            <template #help>{{ editing ? '更换照片或顺序请新建任务；已提交的进度会保留。' : '支持 JPG / PNG / GIF，最多 100 张。拖动或使用箭头调整顺序，按设定张数依次打印。' }}</template>
          </UFormField>
          <UFormField v-else-if="!editing" label="文件" required>
            <input type="file"
              class="block w-full text-sm text-default file:mr-3 file:py-1.5 file:px-3 file:rounded-md file:border-0 file:bg-primary/10 file:text-primary hover:file:bg-primary/20"
              @change="onFilePick" />
            <div v-if="form.file" class="text-xs text-muted mt-1 truncate">
              已选择：{{ form.file.name }}（{{ formatSize(form.file.size) }}）
            </div>
          </UFormField>
          <UFormField v-else label="文件">
            <div class="text-sm text-muted truncate">{{ form.filename }}（更换文件请重新创建任务）</div>
          </UFormField>

          <div class="grid grid-cols-1 sm:grid-cols-2 gap-3">
            <UFormField label="打印机" required>
              <USelect class="w-full"
                v-model="form.printerUri"
                :items="printerItems"
                value-key="value"
                label-key="label"
                placeholder="选择打印机"
                icon="i-lucide-printer"
              />
            </UFormField>

            <UFormField label="触发方式" required>
              <USelect class="w-full"
                v-model="form.scheduleType"
                :items="form.mode === 'queue' ? scheduleTypeItems.filter(item => item.value !== 'once') : scheduleTypeItems"
                value-key="value"
                label-key="label"
              />
            </UFormField>

            <UFormField v-if="form.scheduleType === 'once'" label="执行时间" required>
              <UInput class="w-full" v-model="form.runAtLocal" type="datetime-local" />
            </UFormField>
            <template v-else>
              <UFormField label="开始日期">
                <UInput class="w-full" v-model="form.startDate" type="date" />
                <template #help>按服务器时区执行。</template>
              </UFormField>
              <UFormField label="执行时间" required>
                <UInput class="w-full" v-model="form.scheduleTime" type="time" />
              </UFormField>
              <UFormField v-if="form.scheduleType === 'weekly'" label="星期" required>
                <USelect class="w-full"
                  v-model.number="form.scheduleWeekday"
                  :items="weekdayItems"
                  value-key="value"
                  label-key="label"
                />
              </UFormField>
              <UFormField v-if="form.scheduleType === 'monthly'" label="日期" required>
                <UInput class="w-full" v-model.number="form.scheduleDay" type="number" min="1" max="31" />
                <template #help>该日期超出当月天数时，在月末执行。</template>
              </UFormField>
            </template>


          </div>
          <PrinterMediaOptions :key="editing?.id || 'new'" :printer-uri="form.printerUri"
            v-model:driverOptions="form.driverOptions" v-model:paperSize="form.paperSize" v-model:paperType="form.paperType"
            :paper-size-items="paperSizeItems" :paper-type-items="paperTypeItems" />
          <UFormField v-if="form.mode === 'queue'" label="每次打印张数">
            <UInput class="w-full" v-model.number="form.photosPerRun" type="number" min="1" max="100" />
            <template #help>按队列顺序打印；每张的份数单独设置。</template>
          </UFormField>
          <div class="grid grid-cols-2 sm:grid-cols-4 gap-3">
            <UFormField label="缩放">
              <USelect class="w-full" v-model="form.printScaling" :items="scalingItems" value-key="value" label-key="label" />
            </UFormField>
            <UFormField label="方向">
              <USelect class="w-full" v-model="form.orientation" :items="orientationItems" value-key="value" label-key="label" />
            </UFormField>
            <UFormField label="双面">
              <USelect class="w-full" v-model="form.duplex" :items="duplexItems" value-key="value" label-key="label" />
            </UFormField>
            <UFormField label="色彩">
              <USelect class="w-full" v-model="form.color" :items="colorItems" value-key="value" label-key="label" />
            </UFormField>
            <UFormField label="份数">
              <UInput class="w-full" v-model.number="form.copies" type="number" min="1" max="99" />
            </UFormField>
          </div>

          <UFormField v-if="form.mode === 'queue'" label="全部照片提交后">
            <USelect :model-value="form.loop ? 'loop' : 'stop'" @update:model-value="form.loop = $event === 'loop'" :items="endModeItems" value-key="value" label-key="label" class="w-full" />
            <template #help>中途失败会停止本批次，已成功的照片保留进度；下一次从失败照片继续。立即打印会提前执行下一次计划。</template>
          </UFormField>
          <div v-if="form.mode === 'queue' && !editing" class="rounded-lg bg-primary/10 px-3 py-2 text-sm text-primary">
            {{ scheduleLabel(form) }} 打印 {{ form.photosPerRun }} 张，每张 {{ form.copies }} 份；
            {{ form.loop ? '按顺序循环打印' : form.photos.length + ' 张全部提交后结束' }}。
          </div>
          <UFormField label="水印文字（可选）">
            <UInput class="w-full" v-model="form.watermarkText" placeholder="留空表示不加水印" />
          </UFormField>
        </div>
      </template>
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton variant="ghost" @click="modalOpen = false">取消</UButton>
          <UButton color="primary" :loading="submitting" @click="submit">
            {{ editing ? '保存' : '创建' }}
          </UButton>
        </div>
      </template>
    </UModal>
  </div>
</template>

<script setup>
import { ref, computed, onMounted, onUnmounted, watch } from 'vue'
import PrinterMediaOptions from '../components/print/PrinterMediaOptions.vue'
import { apiFetch, readError } from '../utils/api'

const emit = defineEmits(['logout'])
const toast = useToast()

const loading = ref(false)
const records = ref([])
const printers = ref([])

const modalOpen = ref(false)
const editing = ref(null)
const submitting = ref(false)

const form = ref(emptyForm())
const dragIndex = ref(null)
let photoKey = 0
let refreshTimer
let unmounted = false
let refreshing = 0
let recordsRequestVersion = 0
const modeItems = [
  { value: 'queue', label: '照片按顺序打印' },
  { value: 'single', label: '单文件定时打印' }
]
const endModeItems = [
  { value: 'stop', label: '全部提交后结束' },
  { value: 'loop', label: '循环打印（重复执行）' }
]
const paperTypeItems = [
  { value: 'plain', label: '普通纸' }, { value: 'photo', label: '照片纸' },
  { value: 'glossy', label: '光面照片纸' }, { value: 'matte', label: '哑光照片纸' },
  { value: 'auto', label: '自动选择' }
]
const scalingItems = [
  { value: 'fit', label: '适应纸张' }, { value: 'fill', label: '填充纸张' },
  { value: 'auto', label: '自动' }, { value: 'none', label: '无缩放' }
]
watch(() => form.value.mode, (mode) => {
  if (editing.value) return
  clearPhotoPreviews()
  form.value.file = null
  form.value.scheduleType = mode === 'queue' ? 'daily' : 'once'
  form.value.paperType = mode === 'queue' ? 'photo' : 'plain'
  form.value.paperSize = mode === 'queue' ? '6inch' : 'A4'
})
watch(modalOpen, (open) => { if (!open) clearPhotoPreviews() })

function emptyForm() {
  return {
    id: null,
    name: '',
    mode: 'queue', photos: [], existingFiles: [], loop: false, photosPerRun: 1, driverOptions: {},
    startDate: defaultRunAtLocal().slice(0, 10), paperType: 'photo', printScaling: 'fit',
    file: null,
    filename: '',
    printerUri: '',
    scheduleType: 'daily',
    runAtLocal: defaultRunAtLocal(),
    scheduleTime: '09:00',
    scheduleWeekday: 1,
    scheduleDay: 1,
    copies: 1,
    duplex: 'one-sided',
    color: 'color',
    paperSize: '6inch',
    orientation: 'portrait',
    watermarkText: ''
  }
}

// 默认新任务时间：明天上午 9 点，避免用户不改时间就直接创建导致立即触发。
function defaultRunAtLocal() {
  const d = new Date()
  d.setDate(d.getDate() + 1)
  d.setHours(9, 0, 0, 0)
  const pad = (n) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}

const scheduleTypeItems = [
  { value: 'once', label: '一次性' },
  { value: 'daily', label: '每天' },
  { value: 'weekly', label: '每周' },
  { value: 'monthly', label: '每月' }
]

const weekdayItems = [
  { value: 0, label: '周日' },
  { value: 1, label: '周一' },
  { value: 2, label: '周二' },
  { value: 3, label: '周三' },
  { value: 4, label: '周四' },
  { value: 5, label: '周五' },
  { value: 6, label: '周六' }
]

const paperSizeItems = [
  { value: 'A4', label: 'A4' },
  { value: 'A3', label: 'A3' },
  { value: 'Letter', label: 'Letter' },
  { value: '5inch', label: '5 寸相纸' },
  { value: '6inch', label: '6 寸相纸' },
  { value: '7inch', label: '7 寸相纸' },
  { value: '8inch', label: '8 寸相纸' },
  { value: '10inch', label: '10 寸相纸' }
]
const orientationItems = [
  { value: 'portrait', label: '纵向' },
  { value: 'landscape', label: '横向' }
]
const duplexItems = [
  { value: 'one-sided', label: '单面' },
  { value: 'two-sided-long-edge', label: '双面（长边）' }
]
const colorItems = [
  { value: 'color', label: '彩色' },
  { value: 'monochrome', label: '黑白' }
]

const printerItems = computed(() =>
  printers.value.map((p) => ({ value: p.uri || p.deviceUri || p.name, label: p.name || p.uri }))
)

function shortPrinter(uri) {
  if (!uri) return ''
  const idx = uri.lastIndexOf('/')
  return idx >= 0 ? uri.slice(idx + 1) : uri
}

function formatTime(iso) {
  if (!iso) return '—'
  try {
    return new Date(iso).toLocaleString()
  } catch (e) {
    return iso
  }
}

function formatSize(bytes) {
  if (!bytes) return ''
  const units = ['B', 'KB', 'MB', 'GB']
  let n = bytes
  let i = 0
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++ }
  return `${n.toFixed(1)} ${units[i]}`
}

function scheduleLabel(rec) {
  switch (rec.scheduleType) {
    case 'once': return '一次性'
    case 'daily': return `每天 ${rec.scheduleTime}`
    case 'weekly': return `每${weekdayLabel(rec.scheduleWeekday)} ${rec.scheduleTime}`
    case 'monthly': return `每月 ${rec.scheduleDay} 日 ${rec.scheduleTime}`
    default: return rec.scheduleType
  }
}
function weekdayLabel(w) {
  return ['周日', '周一', '周二', '周三', '周四', '周五', '周六'][((w % 7) + 7) % 7]
}
function lastStatusLabel(s) {
  if (s === 'running') return '结果待确认'
  if (s === 'printed') return '上次已提交'
  if (s === 'failed') return '上次失败'
  if (s === 'skipped') return '错过窗口'
  return s
}
function lastStatusColor(s) {
  if (s === 'printed') return 'success'
  if (s === 'running') return 'warning'
  if (s === 'failed') return 'error'
  if (s === 'skipped') return 'warning'
  return 'neutral'
}

async function loadRecords(silent = false) {
  const version = ++recordsRequestVersion
  refreshing++
  if (!silent) loading.value = true
  try {
    const resp = await apiFetch('/api/scheduled-prints', {}, () => emit('logout'))
    if (!resp.ok) throw new Error(await readError(resp))
    const data = await resp.json()
    if (!unmounted && version === recordsRequestVersion) records.value = data
  } catch (e) {
    if (!silent && !unmounted && version === recordsRequestVersion) toast.add({ title: '加载失败', description: e.message, color: 'error', icon: 'i-lucide-x-circle' })
  } finally {
    refreshing--
    if (version === recordsRequestVersion) loading.value = false
  }
}

async function loadPrinters() {
  try {
    const resp = await apiFetch('/api/printers', {}, () => emit('logout'))
    if (!resp.ok) return
    printers.value = await resp.json()
  } catch (e) {
    // 静默失败：定时任务表单没有打印机数据时会给出必填提示
  }
}

function openCreate() {
  // 清除上一操作的提示，避免提示浮层挡住弹窗按钮。
  toast.clear()
  clearPhotoPreviews()
  editing.value = null
  form.value = emptyForm()
  if (printerItems.value.length) {
    form.value.printerUri = printerItems.value[0].value
  }
  modalOpen.value = true
}

function openEdit(rec) {
  toast.clear()
  clearPhotoPreviews()
  editing.value = rec
  form.value = {
    id: rec.id,
    mode: rec.files?.length ? 'queue' : 'single',
    photos: [], existingFiles: rec.files || [], loop: rec.loop || false,
    startDate: rec.startDate || '', paperType: rec.paperType || 'plain', printScaling: rec.printScaling || 'fit',
    name: rec.name || '',
    file: null,
    filename: rec.filename,
    printerUri: rec.printerUri,
    scheduleType: rec.scheduleType,
    runAtLocal: rec.runAt ? toLocalInput(rec.runAt) : defaultRunAtLocal(),
    scheduleTime: rec.scheduleTime || '09:00',
    scheduleWeekday: rec.scheduleWeekday,
    scheduleDay: rec.scheduleDay || 1,
    copies: rec.copies || 1,
    duplex: rec.isDuplex ? 'two-sided-long-edge' : 'one-sided',
    color: rec.isColor ? 'color' : 'monochrome',
    paperSize: rec.paperSize || 'A4',
    driverOptions: rec.driverOptions || {}, photosPerRun: rec.photosPerRun || 1,
    orientation: rec.orientation || 'portrait',
    watermarkText: rec.watermarkText || ''
  }
  modalOpen.value = true
}

// toLocalInput 把服务器返回的 UTC RFC3339 转成 <input type="datetime-local"> 需要的
// 本地时区 YYYY-MM-DDTHH:mm 字符串。用户在本地看到的日期时间与实际触发对齐。
function toLocalInput(iso) {
  const d = new Date(iso)
  const pad = (n) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}

function clearPhotoPreviews() {
  for (const photo of form.value.photos || []) URL.revokeObjectURL(photo.url)
  form.value.photos = []
}

function onPhotoPick(e) {
  const selected = Array.from(e.target.files || [])
  if (form.value.photos.length + selected.length > 100) {
    toast.add({ title: '最多选择 100 张照片', color: 'warning' })
    e.target.value = ''
    return
  }
  if (selected.some(file => !/\.(jpe?g|png|gif)$/i.test(file.name))) {
    toast.add({ title: '请选择 JPG、PNG 或 GIF 图片', color: 'warning' })
    e.target.value = ''
    return
  }
  for (const file of selected) {
    form.value.photos.push({ key: ++photoKey, file, url: URL.createObjectURL(file) })
  }
  e.target.value = ''
}

function removePhoto(index) {
  const [photo] = form.value.photos.splice(index, 1)
  if (photo) URL.revokeObjectURL(photo.url)
}

function movePhoto(from, to) {
  if (from == null || from === to || from < 0 || to < 0 || from >= form.value.photos.length || to >= form.value.photos.length) return
  const [photo] = form.value.photos.splice(from, 1)
  form.value.photos.splice(to, 0, photo)
}

function onFilePick(e) {
  const files = e.target?.files
  form.value.file = files && files.length ? files[0] : null
}

async function submit() {
  const v = form.value
  if (!v.printerUri) { toast.add({ title: '请选择打印机', color: 'warning' }); return }
  if (!editing.value && (v.mode === 'queue' ? !v.photos.length : !v.file)) { toast.add({ title: '请选择文件', color: 'warning' }); return }
  if (v.scheduleType === 'once' && !v.runAtLocal) { toast.add({ title: '请选择执行时间', color: 'warning' }); return }

  submitting.value = true
  try {
    if (editing.value) {
      await saveEdit(v)
    } else {
      await createNew(v)
    }
    modalOpen.value = false
    await loadRecords()
  } catch (e) {
    toast.add({ title: '保存失败', description: e.message, color: 'error', icon: 'i-lucide-x-circle' })
  } finally {
    submitting.value = false
  }
}

async function createNew(v) {
  const fd = new FormData()
  if (v.mode === 'queue') {
    if (!Number.isInteger(v.photosPerRun) || v.photosPerRun < 1 || v.photosPerRun > 100) throw new Error('每次打印张数应为 1–100')
    for (const photo of v.photos) fd.append('files', photo.file, photo.file.name)
    fd.append('loop', String(v.loop))
  } else {
    fd.append('file', v.file, v.file.name)
  }
  fd.append('start_date', v.scheduleType === 'once' ? '' : v.startDate)
  fd.append('name', v.name)
  fd.append('printer', v.printerUri)
  fd.append('copies', String(v.copies))
  fd.append('duplex', v.duplex === 'two-sided-long-edge' ? 'true' : 'false')
  fd.append('color', v.color === 'color' ? 'true' : 'false')
  fd.append('orientation', v.orientation)
  fd.append('paper_size', v.paperSize)
  fd.append('paper_type', v.paperType)
  fd.append('driver_options', JSON.stringify(v.driverOptions))
  fd.append('photos_per_run', String(v.photosPerRun))
  fd.append('media_source', 'auto')
  fd.append('print_scaling', v.printScaling)
  fd.append('page_set', 'all')
  fd.append('number_up', '1')
  fd.append('number_up_layout', 'lrtb')
  fd.append('page_border', 'none')
  if (v.watermarkText) fd.append('watermark_text', v.watermarkText)
  fd.append('schedule_type', v.scheduleType)
  if (v.scheduleType === 'once') {
    fd.append('run_at', new Date(v.runAtLocal).toISOString())
  } else {
    fd.append('schedule_time', v.scheduleTime)
    fd.append('schedule_weekday', String(v.scheduleWeekday))
    fd.append('schedule_day', String(v.scheduleDay))
  }
  const resp = await apiFetch('/api/scheduled-prints', { method: 'POST', body: fd }, () => emit('logout'))
  if (!resp.ok) throw new Error(await readError(resp))
  toast.add({ title: '已创建定时任务', color: 'success', icon: 'i-lucide-check-circle' })
}

async function saveEdit(v) {
  if (v.mode === 'queue' && (!Number.isInteger(v.photosPerRun) || v.photosPerRun < 1 || v.photosPerRun > 100)) throw new Error('每次打印张数应为 1–100')
  const payload = buildPayloadFromRecord(editing.value, {
    name: v.name,
    loop: v.loop,
    startDate: v.scheduleType === 'once' ? '' : v.startDate,
    printerUri: v.printerUri,
    isDuplex: v.duplex === 'two-sided-long-edge',
    isColor: v.color === 'color',
    copies: v.copies,
    orientation: v.orientation,
    paperSize: v.paperSize,
    driverOptions: v.driverOptions, photosPerRun: v.photosPerRun,
    paperType: v.paperType,
    printScaling: v.printScaling,
    watermarkText: v.watermarkText,
    scheduleType: v.scheduleType,
    scheduleTime: v.scheduleTime,
    scheduleWeekday: v.scheduleWeekday,
    scheduleDay: v.scheduleDay,
    runAt: v.scheduleType === 'once' ? new Date(v.runAtLocal).toISOString() : ''
  })
  // 编辑参数时保留服务端最新启用状态；状态切换由暂停/恢复按钮处理。
  delete payload.enabled
  const resp = await apiFetch(`/api/scheduled-prints/${v.id}`, {
    method: 'PUT',
    body: JSON.stringify(payload)
  }, () => emit('logout'))
  if (!resp.ok) throw new Error(await readError(resp))
  toast.add({ title: '已更新', color: 'success', icon: 'i-lucide-check-circle' })
}

async function runNow(rec) {
  rec._running = true
  try {
    const resp = await apiFetch(`/api/scheduled-prints/${rec.id}/run`, { method: 'POST' }, () => emit('logout'))
    if (!resp.ok) throw new Error(await readError(resp))
    toast.add({ title: '正在提交到打印机', color: 'success', icon: 'i-lucide-check-circle' })
    await loadRecords()
  } catch (e) {
    toast.add({ title: '立即执行失败', description: e.message, color: 'error', icon: 'i-lucide-x-circle' })
  } finally {
    rec._running = false
  }
}

async function toggleEnabled(rec) {
  try {
    const resp = await apiFetch(`/api/scheduled-prints/${rec.id}`, {
      method: 'PUT',
      body: JSON.stringify(buildPayloadFromRecord(rec, { enabled: !rec.enabled }))
    }, () => emit('logout'))
    if (!resp.ok) throw new Error(await readError(resp))
    await loadRecords()
  } catch (e) {
    toast.add({ title: '操作失败', description: e.message, color: 'error', icon: 'i-lucide-x-circle' })
  }
}

async function confirmDelete(rec) {
  if (!confirm(`确认删除任务「${rec.name || rec.filename}」？源文件也会一并清理，无法恢复。`)) return
  try {
    const resp = await apiFetch(`/api/scheduled-prints/${rec.id}`, { method: 'DELETE' }, () => emit('logout'))
    if (!resp.ok) throw new Error(await readError(resp))
    toast.add({ title: '已删除', color: 'success', icon: 'i-lucide-check-circle' })
    await loadRecords()
  } catch (e) {
    toast.add({ title: '删除失败', description: e.message, color: 'error', icon: 'i-lucide-x-circle' })
  }
}

function buildPayloadFromRecord(rec, overrides = {}) {
  return {
    name: rec.name || '',
    loop: rec.loop || false, startDate: rec.startDate || '',
    printerUri: rec.printerUri,
    enabled: rec.enabled,
    isDuplex: rec.isDuplex,
    isColor: rec.isColor,
    copies: rec.copies || 1,
    orientation: rec.orientation || 'portrait',
    paperSize: rec.paperSize || 'A4',
    driverOptions: rec.driverOptions || {}, photosPerRun: rec.photosPerRun || 1,
    paperType: rec.paperType || 'plain',
    mediaSource: rec.mediaSource || 'auto',
    printScaling: rec.printScaling || 'fit',
    pageRange: rec.pageRange || '',
    pageSet: rec.pageSet || 'all',
    mirror: !!rec.mirror,
    watermarkText: rec.watermarkText || '',
    numberUp: rec.numberUp || 1,
    numberUpLayout: rec.numberUpLayout || 'lrtb',
    pageBorder: rec.pageBorder || 'none',
    scheduleType: rec.scheduleType,
    scheduleTime: rec.scheduleTime || '09:00',
    scheduleWeekday: rec.scheduleWeekday || 0,
    scheduleDay: rec.scheduleDay || 1,
    runAt: rec.runAt || '',
    ...overrides
  }
}

onMounted(async () => {
  await Promise.all([loadRecords(), loadPrinters()])
  if (unmounted) return
  refreshTimer = setInterval(() => {
    if (!document.hidden && !refreshing) loadRecords(true)
  }, 5000)
})
onUnmounted(() => {
  unmounted = true
  clearInterval(refreshTimer)
  clearPhotoPreviews()
})
</script>

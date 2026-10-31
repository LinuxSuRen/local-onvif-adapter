<script setup>
import { computed, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import {
  getCameras,
  getMosaics,
  createMosaic,
  updateMosaic,
  deleteMosaic,
  startMosaicPush,
  stopMosaicPush
} from '../api'

// 融合布局编辑器：
//   画布按百分比渲染 cells（x/y/w/h 为画布绝对像素，服务端校验并偶数对齐）；
//   编辑期改动 800ms 防抖自动保存——布局变化会重启合成流，防抖保证拖拽结束才落一次。
const emit = defineEmits(['updated'])

const mosaics = ref([])
const cameras = ref([])
const statuses = reactive({}) // id → status（5s 轮询刷新；编辑期不碰 draft）
const selectedId = ref('')
const loading = ref(false)

const editing = ref(false)
const draft = ref(null)
const dirty = ref(false)
const selectedCell = ref(-1)

const canvasRef = ref(null)
const snapTick = ref(0)
// 预览图：快照生成需 2-3 秒（ffmpeg 冷启动 + 等关键帧），直接换 src 会把画面
// 清空成黑屏——改为后台 Image() 预载，成功后才替换显示，期间保留最后一帧。
const previewSrcs = reactive({})

const pushTarget = ref('')
const createVisible = ref(false)
const createForm = reactive({
  name: '',
  width: 1920,
  height: 1080,
  framerate: 15,
  bitrate_kbps: 4096,
  loss_policy: 'blackframe'
})

let statusTimer = null
let snapTimer = null
let saveTimer = null

const current = computed(() => mosaics.value.find((m) => m.id === selectedId.value) || null)
// 防御：历史数据/异常响应可能给出 cells:null，统一归一为空数组，
// 否则模板里 .length 会抛 TypeError 把整个面板炸掉。
const displayCells = computed(() => {
  if (editing.value && draft.value) {
    return draft.value.cells || []
  }
  return (current.value && current.value.cells) || []
})
const status = computed(() => statuses[selectedId.value] || (current.value && current.value.status) || null)
const cameraOptions = computed(() =>
  cameras.value
    .filter((c) => c.type !== 'mosaic')
    .map((c) => ({ value: c.id, label: c.enabled ? c.name : `${c.name}（已停用）` }))
)
function lossPolicy() {
  return (draft.value && draft.value.loss_policy) || (current.value && current.value.loss_policy) || 'blackframe'
}

function cameraName(id) {
  const c = cameras.value.find((x) => x.id === id)
  return c ? c.name : id
}

// ---- 加载与轮询 ----

async function load() {
  loading.value = true
  try {
    const list = await getMosaics()
    mosaics.value = Array.isArray(list) ? list : []
    if (!mosaics.value.some((m) => m.id === selectedId.value)) {
      selectedId.value = mosaics.value.length ? mosaics.value[0].id : ''
      exitEdit(false)
    }
    for (const m of mosaics.value) {
      statuses[m.id] = m.status || null
    }
    refreshPreviews()
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    loading.value = false
  }
}

async function loadCameras() {
  try {
    const list = await getCameras()
    cameras.value = Array.isArray(list) ? list : []
  } catch (e) {
    ElMessage.error(e.message)
  }
}

onMounted(() => {
  load()
  loadCameras()
  statusTimer = setInterval(load, 5000)
  snapTimer = setInterval(refreshPreviews, 3000)
})

onUnmounted(() => {
  clearInterval(statusTimer)
  clearInterval(snapTimer)
  clearTimeout(saveTimer)
})

watch(selectedId, () => exitEdit(false))

// ---- 编辑与保存 ----

function enterEdit() {
  if (!current.value) return
  const { status: _s, ...cfg } = current.value
  draft.value = JSON.parse(JSON.stringify(cfg))
  draft.value.cells = draft.value.cells || []
  // 下拉控件需要显式值（后端空串 = 默认 blackframe）
  draft.value.loss_policy = draft.value.loss_policy || 'blackframe'
  dirty.value = false
  selectedCell.value = -1
  editing.value = true
}

function exitEdit(save = true) {
  if (editing.value && save && dirty.value) {
    saveNow()
  }
  editing.value = false
  draft.value = null
  dirty.value = false
  selectedCell.value = -1
}

function scheduleSave() {
  dirty.value = true
  clearTimeout(saveTimer)
  saveTimer = setTimeout(saveNow, 800)
}

async function saveNow() {
  clearTimeout(saveTimer)
  if (!draft.value || !dirty.value) return
  const body = JSON.parse(JSON.stringify(draft.value))
  delete body.status
  try {
    const updated = await updateMosaic(draft.value.id, body)
    dirty.value = false
    if (updated) {
      const idx = mosaics.value.findIndex((m) => m.id === updated.id)
      if (idx >= 0) mosaics.value[idx] = updated
      statuses[updated.id] = updated.status || null
    }
    emit('updated') // 虚拟通道（名称/启用状态）随布局变化，摄像头列表需刷新
  } catch (e) {
    ElMessage.error(e.message)
  }
}

// ---- 画布几何：拖拽 / 缩放 / 吸附 ----

function even(v) {
  return v - (v % 2)
}

function clamp(v, min, max) {
  return Math.min(max, Math.max(min, v))
}

const SNAP = computed(() => {
  const w = (draft.value && draft.value.width) || 1920
  const s = Math.max(2, Math.round(w / 48))
  return even(s)
})

function snap(v) {
  return even(Math.round(v / SNAP.value) * SNAP.value)
}

function canvasScale() {
  const el = canvasRef.value
  if (!el || !draft.value) return 1
  return draft.value.width / el.clientWidth
}

function overlap(a, b) {
  return a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h
}

function autoPlace() {
  const W = draft.value.width
  const H = draft.value.height
  const g = Math.ceil(Math.sqrt(draft.value.cells.length + 1))
  const cw = even(Math.floor(W / g))
  const ch = even(Math.floor(H / g))
  for (let gy = 0; gy < g; gy++) {
    for (let gx = 0; gx < g; gx++) {
      const rect = { x: even(Math.floor((gx * W) / g)), y: even(Math.floor((gy * H) / g)), w: cw, h: ch }
      if (!draft.value.cells.some((c) => overlap(c, rect))) return rect
    }
  }
  return { x: 0, y: 0, w: cw, h: ch }
}

function addCell(cameraId) {
  if (!cameraId || !draft.value) return
  const rect = autoPlace()
  draft.value.cells.push({ camera_id: cameraId, ...rect })
  selectedCell.value = draft.value.cells.length - 1
  scheduleSave()
}

function removeCell(i) {
  draft.value.cells.splice(i, 1)
  selectedCell.value = -1
  scheduleSave()
}

function onCellPointerDown(e, i) {
  if (!editing.value) return
  e.preventDefault()
  selectedCell.value = i
  const cell = draft.value.cells[i]
  const orig = { ...cell }
  const startX = e.clientX
  const startY = e.clientY
  const move = (ev) => {
    const k = canvasScale()
    cell.x = snap(orig.x + Math.round((ev.clientX - startX) * k))
    cell.y = snap(orig.y + Math.round((ev.clientY - startY) * k))
    cell.x = clamp(cell.x, 0, draft.value.width - cell.w)
    cell.y = clamp(cell.y, 0, draft.value.height - cell.h)
    scheduleSave()
  }
  const up = () => window.removeEventListener('pointermove', move)
  window.addEventListener('pointermove', move)
  window.addEventListener('pointerup', up, { once: true })
}

const HANDLES = [
  { dir: 'nw', cx: 0, cy: 0 },
  { dir: 'n', cx: 0.5, cy: 0 },
  { dir: 'ne', cx: 1, cy: 0 },
  { dir: 'e', cx: 1, cy: 0.5 },
  { dir: 'se', cx: 1, cy: 1 },
  { dir: 's', cx: 0.5, cy: 1 },
  { dir: 'sw', cx: 0, cy: 1 },
  { dir: 'w', cx: 0, cy: 0.5 }
]

function onHandlePointerDown(e, i, dir) {
  if (!editing.value) return
  e.preventDefault()
  e.stopPropagation()
  const cell = draft.value.cells[i]
  const orig = { ...cell }
  const startX = e.clientX
  const startY = e.clientY
  const move = (ev) => {
    const k = canvasScale()
    let dx = Math.round((ev.clientX - startX) * k)
    let dy = Math.round((ev.clientY - startY) * k)
    if (dir.includes('e')) cell.w = snap(clamp(orig.w + dx, 16, draft.value.width - orig.x))
    if (dir.includes('s')) cell.h = snap(clamp(orig.h + dy, 16, draft.value.height - orig.y))
    if (dir.includes('w')) {
      const x2 = orig.x + orig.w
      cell.x = snap(clamp(orig.x + dx, 0, x2 - 16))
      cell.w = even(x2 - cell.x)
    }
    if (dir.includes('n')) {
      const y2 = orig.y + orig.h
      cell.y = snap(clamp(orig.y + dy, 0, y2 - 16))
      cell.h = even(y2 - cell.y)
    }
    scheduleSave()
  }
  const up = () => window.removeEventListener('pointermove', move)
  window.addEventListener('pointermove', move)
  window.addEventListener('pointerup', up, { once: true })
}

function cellStyle(c) {
  const W = editing.value && draft.value ? draft.value.width : current.value.width
  const H = editing.value && draft.value ? draft.value.height : current.value.height
  return {
    left: `${(c.x / W) * 100}%`,
    top: `${(c.y / H) * 100}%`,
    width: `${(c.w / W) * 100}%`,
    height: `${(c.h / H) * 100}%`
  }
}

// 画面格预览：按当前画布（编辑期取 draft）收集摄像头，逐路后台预载快照。
function activeCellCameraIds() {
  const ids = new Set()
  for (const c of displayCells.value) ids.add(c.camera_id)
  return [...ids]
}

function refreshPreviews() {
  for (const id of activeCellCameraIds()) {
    loadPreview(id)
  }
}

function loadPreview(id) {
  const url = `/api/cameras/${encodeURIComponent(id)}/snapshot?t=${Date.now()}&k=${snapTick.value}`
  const im = new Image()
  im.onload = () => {
    previewSrcs[id] = url
  }
  im.src = url
}

function freshOf(cameraId) {
  if (!status.value || !Array.isArray(status.value.cells)) return null
  const c = status.value.cells.find((x) => x.camera_id === cameraId)
  return c ? c.fresh : null
}

function formatUptime(sec) {
  if (!sec || sec <= 0) return '—'
  if (sec < 60) return `${sec} 秒`
  if (sec < 3600) return `${Math.floor(sec / 60)} 分 ${sec % 60} 秒`
  return `${Math.floor(sec / 3600)} 时 ${Math.floor((sec % 3600) / 60)} 分`
}

// ---- 布局级操作 ----

async function doCreate() {
  if (!createForm.name.trim()) {
    ElMessage.warning('请填写布局名称')
    return
  }
  try {
    const created = await createMosaic({ ...createForm })
    ElMessage.success('已创建')
    createVisible.value = false
    createForm.name = ''
    await load()
    selectedId.value = created.id
    emit('updated')
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function handleRemove() {
  if (!current.value) return
  try {
    await ElMessageBox.confirm(`确定删除融合布局「${current.value.name}」？合成流与推流会话将一并停止。`, '删除确认', {
      confirmButtonText: '删除',
      cancelButtonText: '取消',
      type: 'warning'
    })
  } catch {
    return
  }
  try {
    await deleteMosaic(current.value.id)
    ElMessage.success('已删除')
    selectedId.value = ''
    await load()
    emit('updated')
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function copyStream() {
  const url = status.value && status.value.stream_url
  if (!url) {
    ElMessage.warning('布局未启用，暂无 RTSP 地址')
    return
  }
  try {
    await navigator.clipboard.writeText(url)
    ElMessage.success(`已复制 ${url}`)
  } catch {
    ElMessage.error('复制失败，请手动复制：' + url)
  }
}

// ---- 推流会话 ----

async function addPush() {
  const target = pushTarget.value.trim()
  if (!target) {
    ElMessage.warning('请填写推流目标地址')
    return
  }
  try {
    await startMosaicPush(selectedId.value, target)
    ElMessage.success('已开始推流')
    pushTarget.value = ''
    await load()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function removePush(token) {
  try {
    await stopMosaicPush(selectedId.value, token)
    ElMessage.success('已停止推流')
    await load()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function finishEdit() {
  if (dirty.value) {
    await saveNow()
    // 保存失败（校验不过/网络错误）时停留在编辑态，让用户修正后重试，
    // 不静默丢弃未保存的布局改动。
    if (dirty.value) return
  }
  exitEdit(false)
  await load()
}
</script>

<template>
  <el-card shadow="never">
    <div class="toolbar">
      <span class="card-title">融合布局</span>
      <div class="toolbar-actions">
        <el-select
          v-model="selectedId"
          placeholder="选择布局"
          style="width: 180px"
          :disabled="editing"
        >
          <el-option v-for="m in mosaics" :key="m.id" :value="m.id" :label="m.name" />
        </el-select>
        <el-button type="primary" @click="createVisible = true">
          <el-icon><Plus /></el-icon>新建布局
        </el-button>
        <el-button v-if="!editing" type="primary" plain @click="enterEdit">
          <el-icon><Edit /></el-icon>编辑布局
        </el-button>
        <el-button v-if="editing" type="primary" @click="finishEdit">
          <el-icon><Check /></el-icon>完成
        </el-button>
        <el-button v-if="!editing" type="danger" plain @click="handleRemove">
          <el-icon><Delete /></el-icon>删除
        </el-button>
        <el-button :loading="loading" @click="load">
          <el-icon><Refresh /></el-icon>刷新
        </el-button>
      </div>
    </div>

    <el-empty v-if="!current" description="还没有融合布局，新建后把多个画面拖进画布合成一路流" />

    <template v-else>
      <!-- 无画面源引导：空布局的合成流是纯黑画面，先加摄像头再编辑布局 -->
      <el-alert
        v-if="!cameraOptions.length"
        class="mo-alert"
        type="warning"
        :closable="false"
        show-icon
        title="还没有可用作画面源的摄像头"
        description="融合布局把多路摄像头的画面拼成一路流；没有画面格时输出纯黑画面。请先在上方「摄像头列表」添加摄像头（本地设备 / RTSP 拉流 / 测试彩条），再回到这里编辑布局添加画面。"
      />

      <!-- 画布 -->
      <div
        ref="canvasRef"
        class="mo-canvas"
        :class="{ 'mo-canvas--edit': editing }"
        :style="{ aspectRatio: `${(editing && draft ? draft.width : current.width)} / ${(editing && draft ? draft.height : current.height)}` }"
        @pointerdown.self="selectedCell = -1"
      >
        <div
          v-for="(cell, i) in displayCells"
          :key="i"
          class="mo-cell"
          :class="{
            'mo-cell--selected': editing && selectedCell === i,
            'mo-cell--editing': editing
          }"
          :style="cellStyle(cell)"
          @pointerdown="onCellPointerDown($event, i)"
        >
          <img
            v-if="previewSrcs[cell.camera_id]"
            class="mo-preview"
            :src="previewSrcs[cell.camera_id]"
            alt=""
          />
          <div v-else class="mo-preview mo-preview--loading"></div>
          <div class="mo-cell-bar">
            <span class="mo-cell-name">{{ cameraName(cell.camera_id) }}</span>
            <span
              v-if="!editing && freshOf(cell.camera_id) !== null"
              class="mo-dot"
              :class="freshOf(cell.camera_id) ? 'mo-dot--ok' : 'mo-dot--bad'"
              :title="freshOf(cell.camera_id) ? '画面正常' : '画面中断，铺黑帧中'"
            ></span>
          </div>
          <template v-if="editing && selectedCell === i">
            <span
              v-for="h in HANDLES"
              :key="h.dir"
              class="mo-handle"
              :style="{ left: `${h.cx * 100}%`, top: `${h.cy * 100}%` }"
              @pointerdown="onHandlePointerDown($event, i, h.dir)"
            ></span>
          </template>
        </div>
        <div v-if="editing && draft && !displayCells.length" class="mo-hint">
          点击「添加画面」选择摄像头加入画布
        </div>
        <div v-else-if="!editing && current && !displayCells.length" class="mo-hint">
          画布为空，点击右上角「编辑布局」添加摄像头画面
        </div>
      </div>

      <!-- 编辑工具条 -->
      <div v-if="editing && draft" class="mo-editbar">
        <el-dropdown trigger="click" @command="(v) => addCell(v)">
          <el-button type="primary">
            <el-icon><Plus /></el-icon>添加画面<el-icon class="el-icon--right"><ArrowDown /></el-icon>
          </el-button>
          <template #dropdown>
            <el-dropdown-menu>
              <el-dropdown-item
                v-for="o in cameraOptions"
                :key="o.value"
                :command="o.value"
                :disabled="!o.value"
              >
                {{ o.label }}
              </el-dropdown-item>
              <el-dropdown-item v-if="!cameraOptions.length" disabled>
                暂无可用摄像头，请先在摄像头列表添加
              </el-dropdown-item>
            </el-dropdown-menu>
          </template>
        </el-dropdown>
        <el-button v-if="selectedCell >= 0 && draft.cells[selectedCell]" @click="removeCell(selectedCell)">
          <el-icon><Delete /></el-icon>删除选中格
        </el-button>
        <span class="mo-meta-hint">点击画面格选中；拖动移动位置，拖角缩放；改动自动保存（{{ dirty ? '待保存' : '已保存' }}）</span>
      </div>

      <!-- 布局参数（二次编辑：创建后仍可调整全部参数） -->
      <div v-if="editing && draft" class="mo-settings">
        <el-form inline size="small">
          <el-form-item label="名称">
            <el-input v-model="draft.name" style="width: 140px" @change="scheduleSave()" />
          </el-form-item>
          <el-form-item label="帧率">
            <el-input-number v-model="draft.framerate" :min="1" :max="30" @change="scheduleSave()" />
          </el-form-item>
          <el-form-item label="码率（kbps）">
            <el-input-number v-model="draft.bitrate_kbps" :min="256" :max="8192" :step="64" @change="scheduleSave()" />
          </el-form-item>
          <el-form-item label="断流策略">
            <el-select v-model="draft.loss_policy" style="width: 210px" @change="scheduleSave()">
              <el-option value="blackframe" label="黑帧兜底（单路断流只黑一格）" />
              <el-option value="restart" label="整体重启（任一路断流重启合成流）" />
            </el-select>
          </el-form-item>
          <el-form-item label="音频源">
            <el-select
              v-model="draft.audio_camera_id"
              clearable
              placeholder="不携带音频"
              style="width: 160px"
              @change="scheduleSave()"
            >
              <el-option v-for="o in cameraOptions" :key="o.value" :value="o.value" :label="o.label" />
            </el-select>
          </el-form-item>
          <el-form-item label="启用">
            <el-switch v-model="draft.enabled" @change="scheduleSave()" />
          </el-form-item>
        </el-form>
      </div>

      <!-- 选中格参数（摄像头与位置微调） -->
      <div v-if="editing && draft && selectedCell >= 0 && draft.cells[selectedCell]" class="mo-cellform">
        <span class="mo-cellform-title">画面格 {{ selectedCell + 1 }}</span>
        <el-select
          v-model="draft.cells[selectedCell].camera_id"
          style="width: 180px"
          @change="scheduleSave()"
        >
          <el-option v-for="o in cameraOptions" :key="o.value" :value="o.value" :label="o.label" />
        </el-select>
        <el-input-number
          v-model="draft.cells[selectedCell].x"
          :min="0" :max="draft.width - draft.cells[selectedCell].w" :step="2"
          size="small" @change="scheduleSave()"
        />
        <el-input-number
          v-model="draft.cells[selectedCell].y"
          :min="0" :max="draft.height - draft.cells[selectedCell].h" :step="2"
          size="small" @change="scheduleSave()"
        />
        <el-input-number
          v-model="draft.cells[selectedCell].w"
          :min="16" :max="draft.width - draft.cells[selectedCell].x" :step="2"
          size="small" @change="scheduleSave()"
        />
        <el-input-number
          v-model="draft.cells[selectedCell].h"
          :min="16" :max="draft.height - draft.cells[selectedCell].y" :step="2"
          size="small" @change="scheduleSave()"
        />
        <span class="mo-meta-hint">X / Y / 宽 / 高（像素，偶数）</span>
      </div>

      <!-- 参数与状态 -->
      <div class="mo-statusbar">
        <div class="mo-status-left">
          <span class="run-status">
            <span class="dot" :class="status && status.running ? 'dot-running' : 'dot-stopped'"></span>
            {{ status && status.running ? '运行中' : '未运行' }}
          </span>
          <span class="mo-meta-hint">
            {{ current.width }}×{{ current.height }}@{{ current.framerate || 15 }} ·
            {{ displayCells.length }} 画面格 · {{ lossPolicy() === 'blackframe' ? '黑帧兜底' : '整体重启' }}
            <template v-if="status && status.restarts"> · 重启 {{ status.restarts }} 次</template>
            <template v-if="status && status.uptime_seconds"> · 已运行 {{ formatUptime(status.uptime_seconds) }}</template>
          </span>
          <el-tooltip v-if="status && status.last_error" :content="status.last_error" placement="top">
            <el-tag size="small" type="danger">最近错误</el-tag>
          </el-tooltip>
        </div>
        <div>
          <el-button link type="primary" :disabled="!status || !status.stream_url" @click="copyStream">
            <el-icon><Link /></el-icon>复制 RTSP
          </el-button>
        </div>
      </div>

      <!-- 推流目标 -->
      <div class="mo-push">
        <div class="mo-push-head">
          <span class="mo-push-title">整流推送目标</span>
          <el-input
            v-model="pushTarget"
            placeholder="rtsp://服务器:8554/路径（支持 rtsp/rtmp/srt）"
            style="width: 360px"
            :disabled="!current.enabled"
            @keyup.enter="addPush"
          />
          <el-button type="primary" :disabled="!current.enabled" @click="addPush">开始推流</el-button>
        </div>
        <el-table
          v-if="status && status.pushes && status.pushes.length"
          :data="status.pushes"
          size="small"
        >
          <el-table-column prop="target" label="目标" min-width="320" show-overflow-tooltip />
          <el-table-column label="状态" width="110">
            <template #default="{ row }">
              <el-tag size="small" :type="row.status === 'running' ? 'success' : row.status === 'failed' ? 'danger' : 'info'">
                {{ { running: '推送中', failed: '已失败', starting: '启动中' }[row.status] || row.status }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="操作" width="100">
            <template #default="{ row }">
              <el-button link type="danger" @click="removePush(row.token)">停止</el-button>
            </template>
          </el-table-column>
        </el-table>
        <p v-else class="mo-meta-hint">还没有推流目标。合成流同时支持多路推送，本地 RTSP 地址可直接被 NVR 拉流。</p>
      </div>
    </template>

    <!-- 新建对话框 -->
    <el-dialog v-model="createVisible" title="新建融合布局" width="480px">
      <el-form label-width="110px">
        <el-form-item label="名称" required>
          <el-input v-model="createForm.name" placeholder="如：四分屏" />
        </el-form-item>
        <el-form-item label="画布尺寸">
          <el-input-number v-model="createForm.width" :min="64" :max="3840" :step="2" />
          <span class="mo-form-sep">×</span>
          <el-input-number v-model="createForm.height" :min="64" :max="2160" :step="2" />
        </el-form-item>
        <el-form-item label="帧率">
          <el-input-number v-model="createForm.framerate" :min="1" :max="30" />
        </el-form-item>
        <el-form-item label="码率（kbps）">
          <el-input-number v-model="createForm.bitrate_kbps" :min="256" :max="8192" :step="64" />
        </el-form-item>
        <el-form-item label="断流策略">
          <el-select v-model="createForm.loss_policy" style="width: 100%">
            <el-option value="blackframe" label="黑帧兜底（单路断流只黑一格）" />
            <el-option value="restart" label="整体重启（任一路断流重启合成流）" />
          </el-select>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="createVisible = false">取消</el-button>
        <el-button type="primary" @click="doCreate">创建</el-button>
      </template>
    </el-dialog>
  </el-card>
</template>

<style scoped>
.toolbar {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 16px;
}
.toolbar-actions {
  display: flex;
  gap: 8px;
  align-items: center;
}
.card-title {
  font-weight: 600;
}
.mo-alert {
  margin-bottom: 12px;
}
.mo-canvas {
  position: relative;
  width: 100%;
  background: #000;
  border-radius: 6px;
  overflow: hidden;
  user-select: none;
}
.mo-canvas--edit {
  background-image: linear-gradient(rgba(255, 255, 255, 0.06) 1px, transparent 1px),
    linear-gradient(90deg, rgba(255, 255, 255, 0.06) 1px, transparent 1px);
  background-size: 6.25% 6.25%;
  cursor: crosshair;
}
.mo-cell {
  position: absolute;
  border: 1px solid rgba(255, 255, 255, 0.25);
  box-sizing: border-box;
  overflow: hidden;
  background: #101014;
}
.mo-cell--editing {
  cursor: move;
  border-color: rgba(64, 158, 255, 0.6);
}
.mo-cell--editing:hover {
  border-color: #409eff;
}
.mo-cell--selected {
  border: 1px solid #409eff;
  box-shadow: 0 0 0 1px #409eff inset;
  z-index: 2;
}
.mo-preview {
  position: absolute;
  inset: 0;
  width: 100%;
  height: 100%;
  object-fit: cover;
  pointer-events: none;
}
.mo-preview--loading {
  background: repeating-linear-gradient(45deg, #101014, #101014 12px, #16161c 12px, #16161c 24px);
}
.mo-cell-bar {
  position: absolute;
  left: 0;
  right: 0;
  bottom: 0;
  display: flex;
  justify-content: space-between;
  align-items: center;
  padding: 2px 6px;
  background: linear-gradient(transparent, rgba(0, 0, 0, 0.65));
  color: #fff;
  font-size: 12px;
  pointer-events: none;
}
.mo-cell-name {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.mo-dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  margin-left: 6px;
  flex: none;
}
.mo-dot--ok {
  background: #67c23a;
}
.mo-dot--bad {
  background: #909399;
}
.mo-handle {
  position: absolute;
  width: 10px;
  height: 10px;
  background: #409eff;
  border: 1px solid #fff;
  border-radius: 2px;
  transform: translate(-50%, -50%);
  z-index: 3;
}
.mo-hint {
  position: absolute;
  inset: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  color: #909399;
  font-size: 13px;
}
.mo-editbar {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-top: 12px;
  flex-wrap: wrap;
}
.mo-settings {
  margin-top: 12px;
}
.mo-settings :deep(.el-form-item) {
  margin-bottom: 8px;
}
.mo-cellform {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 10px;
  flex-wrap: wrap;
}
.mo-cellform-title {
  font-weight: 600;
  font-size: 13px;
  color: #606266;
}
.mo-statusbar {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-top: 14px;
  flex-wrap: wrap;
  gap: 8px;
}
.mo-status-left {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}
.run-status {
  display: inline-flex;
  align-items: center;
}
.dot {
  display: inline-block;
  width: 8px;
  height: 8px;
  border-radius: 50%;
  margin-right: 6px;
}
.dot-running {
  background: #67c23a;
}
.dot-stopped {
  background: #c0c4cc;
}
.mo-meta-hint {
  color: #909399;
  font-size: 13px;
}
.mo-push {
  margin-top: 14px;
}
.mo-push-head {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: 8px;
  flex-wrap: wrap;
}
.mo-push-title {
  font-weight: 600;
  font-size: 13px;
}
.mo-form-sep {
  margin: 0 4px;
  color: #909399;
}
</style>

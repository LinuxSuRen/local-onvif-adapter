<script setup>
import { computed, nextTick, reactive, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { createCamera, updateCamera, listDevices, listRealsenseDevices, createRealsenseCameras, updateRealsenseSettings } from '../api'

const props = defineProps({
  visible: { type: Boolean, default: false },
  camera: { type: Object, default: null }
})
const emit = defineEmits(['update:visible', 'saved'])

const show = computed({
  get: () => props.visible,
  set: (v) => emit('update:visible', v)
})

const formRef = ref()
const saving = ref(false)

const DEFAULT_FORM = {
  name: '',
  type: '',
  source: '',
  width: 0,
  height: 0,
  framerate: 15,
  bitrate_kbps: 2048,
  infrared: false,
  audio_enabled: false,
  audio_source: '',
  enabled: true
}

const form = reactive({ ...DEFAULT_FORM })

// 用户只选择"来源方式"，平台差异（v4l2/avfoundation/dshow）由设备枚举结果自动携带。
const LOCAL_TYPES = ['v4l2', 'avfoundation', 'dshow']
const MODE_OPTIONS = [
  { value: 'local', label: '本地摄像头' },
  { value: 'screen', label: '屏幕采集' },
  { value: 'realsense', label: '深度相机' },
  { value: 'rtsp', label: '网络摄像头（RTSP）' },
  { value: 'rpicam', label: '树莓派 CSI 摄像头' },
  { value: 'testsrc', label: '测试彩条' }
]
const mode = ref('local')

// RealSense 深度相机：一键创建三路（彩色/深度伪彩色/红外），rs-pipe 细节全部隐藏。
const rsDevices = ref([])
const rsLoading = ref(false)
const rsBinAvailable = ref(true)
const rsHint = ref('')
const rsBinInput = ref('')
const rsBinSaving = ref(false)
const RS_GEOMETRY_PRESETS = [
  { label: '640×480@15', width: 640, height: 480, framerate: 15 },
  { label: '848×480@30', width: 848, height: 480, framerate: 30 },
  { label: '1280×720@30', width: 1280, height: 720, framerate: 30 }
]

async function loadRealsenseDevices(showToast = false) {
  rsLoading.value = true
  try {
    const result = await listRealsenseDevices()
    rsBinAvailable.value = !!result.bin_available
    rsDevices.value = Array.isArray(result.devices) ? result.devices : []
    rsHint.value = result.hint || ''
    rsBinInput.value = (result.realsense_bin || '').trim()
  } catch (e) {
    rsBinAvailable.value = false
    rsDevices.value = []
    rsHint.value = e.message
    if (showToast) ElMessage.warning(e.message)
  } finally {
    rsLoading.value = false
  }
}

async function saveRsBinAndRetry() {
  if (!rsBinInput.value.trim()) {
    ElMessage.warning('请填写采集器路径')
    return
  }
  rsBinSaving.value = true
  try {
    await updateRealsenseSettings(rsBinInput.value.trim())
    ElMessage.success('采集器路径已保存，正在重新检测…')
    await loadRealsenseDevices()
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    rsBinSaving.value = false
  }
}

function applyRsGeometry(p) {
  form.width = p.width
  form.height = p.height
  form.framerate = p.framerate
}

// 本地设备枚举。
const devices = ref([])
const audioDevices = ref([])
const devicesLoading = ref(false)
const devicesEnumFailed = ref(false)
const defaultLocalType = ref('')
let devicesLoaded = false

async function loadDevices(showToast = false) {
  devicesLoading.value = true
  try {
    const result = await listDevices()
    devices.value = Array.isArray(result && result.devices) ? result.devices : []
    audioDevices.value = Array.isArray(result && result.audio_devices) ? result.audio_devices : []
    defaultLocalType.value = (result && result.default_type) || ''
    devicesEnumFailed.value = false
  } catch (e) {
    devices.value = []
    audioDevices.value = []
    devicesEnumFailed.value = true
    if (showToast) ElMessage.warning(e.message)
  } finally {
    devicesLoading.value = false
    devicesLoaded = true
  }
}

watch(
  () => [props.visible, mode.value],
  ([visible, m]) => {
    if (visible && (m === 'local' || m === 'screen') && !devicesLoaded) {
      loadDevices()
    }
    if (visible && m === 'realsense' && !props.camera) {
      loadRealsenseDevices()
    }
  }
)

function kindLabel(kind) {
  if (kind === 'builtin') return '内置'
  if (kind === 'usb') return 'USB'
  if (kind === 'screen') return '屏幕'
  return ''
}

function deviceLabel(d) {
  const k = kindLabel(d.kind)
  return k ? `${d.name}（${k}）` : d.name
}

// 设备下拉的实际取值：优先用唯一别名（同型号多台摄像头友好名称相同，
// DirectShow 按名称只会解析到同一台物理设备；PnP 别名可精确打开对应设备）。
function deviceValue(d) {
  return d.alt_source || d.source
}

// 音频可用的来源：本地摄像头（麦克风）与 RTSP 源（透传源音轨）。
const audioAvailable = computed(() => mode.value === 'local' || mode.value === 'rtsp')
// 本地摄像头需要指定音频设备；RTSP 源直接用源流音轨，无需选择。
const audioNeedsSource = computed(() => mode.value === 'local')

// 当前模式下可选的设备列表：本地摄像头排除屏幕，屏幕模式只显示屏幕。
const filteredDevices = computed(() => {
  if (mode.value === 'screen') {
    return devices.value.filter((d) => d.kind === 'screen' || d.type === 'screen')
  }
  return devices.value.filter((d) => d.kind !== 'screen' && d.type !== 'screen')
})

// 选择设备：类型随设备携带。
function onDeviceChange(source) {
  const dev = filteredDevices.value.find((d) => deviceValue(d) === source)
  if (dev) {
    form.type = dev.type
    // 名称未填写时，用所选设备名自动补齐（如 "WL24A #1"），减少手动输入。
    if (!form.name.trim()) {
      form.name = dev.name
    }
  } else if (mode.value === 'screen') {
    form.type = 'screen'
  } else {
    form.type = defaultLocalType.value
  }
}

watch(mode, (m) => {
  if (m === 'rtsp') form.type = 'rtsp'
  if (m === 'testsrc') form.type = 'testsrc'
  if (m === 'screen') form.type = 'screen'
  if (m === 'realsense') {
    form.type = 'realsense'
    // 深度相机默认几何 640×480@15（三路一致，rs-pipe 统一启用）。
    if (!form.width && !form.height) {
      form.width = 640
      form.height = 480
      form.framerate = 15
    }
    if (!form.name.trim()) form.name = 'RealSense'
  }
  if (m === 'local' && !form.type) form.type = defaultLocalType.value
  nextTick(() => formRef.value && formRef.value.clearValidate('source'))
})

const SOURCE_PLACEHOLDER = {
  rtsp: 'rtsp://user:pass@ip:554/stream',
  testsrc: '留空即可，生成 1280×720 彩条'
}

const rules = {
  name: [{ required: true, message: '请输入名称', trigger: 'blur' }],
  source: [
    {
      validator: (rule, value, callback) => {
        if (mode.value === 'testsrc' || mode.value === 'realsense') return callback()
        if (!value || !String(value).trim()) {
          return callback(new Error(mode.value === 'local' || mode.value === 'screen' ? '请选择设备' : '请输入源'))
        }
        if (mode.value === 'rtsp' && !String(value).trim().startsWith('rtsp://')) {
          return callback(new Error('RTSP 源必须以 rtsp:// 开头'))
        }
        callback()
      },
      trigger: ['blur', 'change']
    }
  ]
}

watch(
  () => props.visible,
  (v) => {
    if (!v) return
    const c = props.camera
    if (c) {
      Object.assign(form, DEFAULT_FORM, {
        name: c.name != null ? c.name : '',
        type: c.type != null ? c.type : '',
        source: c.source != null ? c.source : '',
        width: c.width != null ? c.width : 0,
        height: c.height != null ? c.height : 0,
        framerate: c.framerate != null ? c.framerate : 15,
        bitrate_kbps: c.bitrate_kbps != null ? c.bitrate_kbps : 2048,
        infrared: !!c.infrared,
        audio_enabled: !!c.audio_enabled,
        audio_source: c.audio_source != null ? c.audio_source : '',
        enabled: !!c.enabled
      })
      mode.value = LOCAL_TYPES.includes(c.type) ? 'local' : c.type || 'local'
    } else {
      Object.assign(form, DEFAULT_FORM)
      mode.value = 'local'
      form.type = defaultLocalType.value
    }
    nextTick(() => formRef.value && formRef.value.clearValidate())
  }
)

async function handleSave() {
  try {
    await formRef.value.validate()
  } catch (e) {
    return
  }
  // 深度相机新建走专用端点：一次创建三路（彩色/深度/红外），rs-pipe 细节由服务端处理。
  if (mode.value === 'realsense' && !(props.camera && props.camera.id)) {
    if (!rsBinAvailable.value) {
      ElMessage.warning('请先配置可用的深度相机采集器（rs-pipe）')
      return
    }
    saving.value = true
    try {
      await createRealsenseCameras({
        name_prefix: form.name.trim(),
        width: form.width || 640,
        height: form.height || 480,
        framerate: form.framerate || 15
      })
      ElMessage.success('已创建三路摄像头：彩色 / 深度（伪彩色）/ 红外')
      emit('saved')
      show.value = false
    } catch (e) {
      ElMessage.error(e.message)
    } finally {
      saving.value = false
    }
    return
  }
  // 兜底：本地模式下类型始终跟随设备或平台默认值。
  let type = form.type
  if (mode.value === 'local' || mode.value === 'screen') {
    const dev = filteredDevices.value.find((d) => deviceValue(d) === form.source)
    type = dev ? dev.type : mode.value === 'screen' ? 'screen' : defaultLocalType.value
  } else {
    type = mode.value
  }
  saving.value = true
  const body = {
    name: form.name.trim(),
    type,
    source: mode.value === 'testsrc' ? '' : form.source.trim(),
    width: form.width,
    height: form.height,
    framerate: form.framerate,
    bitrate_kbps: form.bitrate_kbps,
    infrared: form.infrared,
    audio_enabled: audioAvailable.value && form.audio_enabled,
    audio_source: audioAvailable.value && form.audio_enabled ? form.audio_source.trim() : '',
    enabled: form.enabled
  }
  try {
    if (props.camera && props.camera.id) {
      await updateCamera(props.camera.id, body)
    } else {
      await createCamera(body)
    }
    ElMessage.success('已保存')
    emit('saved')
    show.value = false
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <el-dialog v-model="show" :title="camera ? '编辑摄像头' : '添加摄像头'" width="560px">
    <el-form ref="formRef" :model="form" :rules="rules" label-width="90px">
      <el-form-item label="名称" prop="name">
        <el-input v-model="form.name" :placeholder="mode === 'realsense' && !camera ? '名称前缀，将生成 前缀-彩色/深度/红外' : '请输入名称'" />
      </el-form-item>
      <el-form-item label="来源方式">
        <el-radio-group v-model="mode">
          <el-radio-button v-for="m in MODE_OPTIONS" :key="m.value" :value="m.value">
            {{ m.label }}
          </el-radio-button>
        </el-radio-group>
      </el-form-item>
      <el-form-item v-if="mode === 'local' || mode === 'screen'" :label="mode === 'screen' ? '屏幕' : '摄像头'" prop="source">
        <div class="num-row">
          <el-select
            v-model="form.source"
            class="full-width"
            filterable
            allow-create
            default-first-option
            :loading="devicesLoading"
            :placeholder="mode === 'screen' ? '选择要采集的屏幕' : '选择检测到的摄像头，或直接输入设备源'"
            @change="onDeviceChange"
          >
            <el-option v-for="d in filteredDevices" :key="deviceValue(d)" :label="deviceLabel(d)" :value="deviceValue(d)" />
          </el-select>
          <el-button class="enum-refresh" :loading="devicesLoading" @click="loadDevices(true)">刷新</el-button>
        </div>
        <div class="form-tip">
          {{ devicesEnumFailed
            ? '设备枚举不可用，请确认 ffmpeg 已安装；也可手动输入设备源'
            : mode === 'screen'
              ? (filteredDevices.length > 0 ? '列表为主机已连接的屏幕，多屏可各自添加为独立摄像头' : '未检测到屏幕，可手动输入源（macOS 填屏幕索引，Linux 填 :0.0，Windows 填 desktop）')
              : devices.length === 0 && !devicesLoading
                ? '未检测到本地摄像头，接入后点刷新；也可手动输入设备源'
                : '列表为当前主机检测到的内置与 USB 摄像头' }}
        </div>
      </el-form-item>
      <el-form-item v-if="mode === 'realsense' && !camera" label="设备">
        <div class="rs-panel">
          <template v-if="rsLoading">
            <div class="form-tip">正在检测深度相机…</div>
          </template>
          <template v-else-if="!rsBinAvailable">
            <el-alert type="warning" :closable="false" show-icon class="rs-alert"
              title="未检测到深度相机采集器（rs-pipe）"
              :description="rsHint || '深度相机由 rs-pipe 采集器统一取流；请填写其路径（编译说明见 README）'" />
            <div class="num-row">
              <el-input v-model="rsBinInput" class="full-width" placeholder="如 D:\tools\rs-pipe.exe" />
              <el-button class="enum-refresh" :loading="rsBinSaving" @click="saveRsBinAndRetry">保存并检测</el-button>
            </div>
          </template>
          <template v-else-if="rsDevices.length === 0">
            <div class="form-tip">
              采集器可用，但未检测到 RealSense 设备；插入相机后
              <el-button text type="primary" size="small" @click="loadRealsenseDevices(true)">重新检测</el-button>
            </div>
          </template>
          <template v-else>
            <div v-for="d in rsDevices" :key="d.serial" class="rs-device">
              <el-tag type="success" size="small">{{ d.name }}</el-tag>
              <span class="rs-serial">SN {{ d.serial }}</span>
            </div>
            <div class="form-tip">将创建三路摄像头：彩色 / 深度（伪彩色 300–8000mm）/ 红外</div>
          </template>
        </div>
      </el-form-item>
      <el-form-item v-else-if="mode !== 'local' && mode !== 'screen'" label="源" prop="source">
        <el-input
          v-model="form.source"
          :disabled="mode === 'testsrc'"
          :placeholder="SOURCE_PLACEHOLDER[mode]"
        />
        <div v-if="mode === 'realsense'" class="form-tip">
          源为采集器分发地址（彩色/深度/红外各一路），由"深度相机"一键添加自动生成，无需修改
        </div>
      </el-form-item>
      <el-form-item v-if="mode === 'realsense' && !camera" label="常用档位">
        <div class="num-row">
          <el-button v-for="p in RS_GEOMETRY_PRESETS" :key="p.label" size="small"
            :type="form.width === p.width && form.height === p.height && form.framerate === p.framerate ? 'primary' : 'default'"
            @click="applyRsGeometry(p)">{{ p.label }}</el-button>
        </div>
        <div class="form-tip">三路共用同一几何参数（采集器统一启用）；如需自定义可直接改下方分辨率/帧率</div>
      </el-form-item>
      <el-form-item label="分辨率">
        <div class="num-row">
          <el-input-number v-model="form.width" :min="0" :max="3840" controls-position="right" placeholder="宽" />
          <span class="num-sep">×</span>
          <el-input-number v-model="form.height" :min="0" :max="3840" controls-position="right" placeholder="高" />
        </div>
        <div class="form-tip">{{ mode === 'realsense' ? '深度相机建议 0 自动改为常用档位' : '宽高为 0 表示自动' }}</div>
      </el-form-item>
      <el-form-item label="帧率">
        <el-input-number v-model="form.framerate" :min="1" :max="60" />
      </el-form-item>
      <el-form-item v-if="mode !== 'realsense'" label="码率">
        <div class="num-row">
          <el-input-number v-model="form.bitrate_kbps" :min="256" :max="8192" />
          <span class="num-sep">kbps</span>
        </div>
      </el-form-item>
      <el-form-item v-if="mode !== 'realsense'" label="红外">
        <el-switch v-model="form.infrared" />
        <div class="form-tip">标记为红外通道后，ONVIF profile 名称会带 infrared，客户端可自动识别</div>
      </el-form-item>
      <el-form-item v-if="audioAvailable" label="音频">
        <div class="num-row">
          <el-switch v-model="form.audio_enabled" />
          <el-select
            v-if="form.audio_enabled && audioNeedsSource"
            v-model="form.audio_source"
            class="full-width audio-source"
            filterable
            allow-create
            default-first-option
            clearable
            :loading="devicesLoading"
            placeholder="选择麦克风，或输入音频源标识"
          >
            <el-option v-for="d in audioDevices" :key="d.source" :label="d.name" :value="d.source" />
          </el-select>
        </div>
        <div class="form-tip">
          {{
            audioNeedsSource
              ? '启用后在推流中附带麦克风音轨（AAC）；Linux 需填 pulse 源名（留空默认 default）'
              : '启用后透传 RTSP 源自带的音轨（统一转码 AAC），无需选择设备'
          }}
        </div>
      </el-form-item>
      <el-form-item label="启用">
        <el-switch v-model="form.enabled" />
      </el-form-item>
    </el-form>
    <template #footer>
      <el-button @click="show = false">取消</el-button>
      <el-button type="primary" :loading="saving" @click="handleSave">确定</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.full-width {
  width: 100%;
}
.num-row {
  display: flex;
  align-items: center;
  width: 100%;
}
.num-row .el-input-number {
  flex: 1;
}
.num-sep {
  margin: 0 8px;
  color: #909399;
}
.form-tip {
  width: 100%;
  font-size: 12px;
  color: #909399;
  line-height: 1.5;
  margin-top: 4px;
}
.enum-refresh {
  margin-left: 8px;
}
.audio-source {
  margin-left: 8px;
}
.rs-panel {
  width: 100%;
}
.rs-alert {
  margin-bottom: 8px;
}
.rs-device {
  display: flex;
  align-items: center;
  gap: 8px;
}
.rs-serial {
  font-size: 12px;
  color: #909399;
}
</style>

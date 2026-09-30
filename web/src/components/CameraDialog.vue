<script setup>
import { computed, nextTick, reactive, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { createCamera, updateCamera, listDShowDevices } from '../api'

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
  type: 'v4l2',
  source: '',
  width: 0,
  height: 0,
  framerate: 15,
  bitrate_kbps: 2048,
  infrared: false,
  enabled: true
}

const form = reactive({ ...DEFAULT_FORM })

const TYPE_OPTIONS = [
  { value: 'v4l2', label: 'v4l2 设备' },
  { value: 'avfoundation', label: 'macOS 摄像头' },
  { value: 'dshow', label: 'Windows 摄像头' },
  { value: 'rtsp', label: 'RTSP 拉流' },
  { value: 'testsrc', label: '测试彩条' }
]

const SOURCE_PLACEHOLDER = {
  v4l2: '/dev/video0',
  avfoundation: '0（设备索引）',
  dshow: '设备名（如 Integrated Camera）',
  rtsp: 'rtsp://user:pass@ip:554/stream',
  testsrc: '留空即可，生成 1280×720 彩条'
}

const isTestSrc = computed(() => form.type === 'testsrc')
const isDShow = computed(() => form.type === 'dshow')

// Windows 设备枚举：可选下拉，失败时回退为手填。
const dshowDevices = ref([])
const dshowLoading = ref(false)
const dshowEnumFailed = ref(false)
let dshowLoaded = false

async function loadDShowDevices(showToast = false) {
  dshowLoading.value = true
  try {
    const devices = await listDShowDevices()
    dshowDevices.value = Array.isArray(devices) ? devices : []
    dshowEnumFailed.value = false
  } catch (e) {
    dshowDevices.value = []
    dshowEnumFailed.value = true
    if (showToast) ElMessage.warning(e.message)
  } finally {
    dshowLoading.value = false
    dshowLoaded = true
  }
}

watch(
  () => [props.visible, form.type],
  ([visible, type]) => {
    if (visible && type === 'dshow' && !dshowLoaded) {
      loadDShowDevices()
    }
  }
)

const rules = {
  name: [{ required: true, message: '请输入名称', trigger: 'blur' }],
  source: [
    {
      validator: (rule, value, callback) => {
        if (form.type === 'testsrc') return callback()
        if (!value || !String(value).trim()) return callback(new Error('请输入源'))
        if (form.type === 'rtsp' && !String(value).trim().startsWith('rtsp://')) {
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
        type: c.type != null ? c.type : 'v4l2',
        source: c.source != null ? c.source : '',
        width: c.width != null ? c.width : 0,
        height: c.height != null ? c.height : 0,
        framerate: c.framerate != null ? c.framerate : 15,
        bitrate_kbps: c.bitrate_kbps != null ? c.bitrate_kbps : 2048,
        infrared: !!c.infrared,
        enabled: !!c.enabled
      })
    } else {
      Object.assign(form, DEFAULT_FORM)
    }
    nextTick(() => formRef.value && formRef.value.clearValidate())
  }
)

watch(
  () => form.type,
  () => {
    nextTick(() => formRef.value && formRef.value.clearValidate('source'))
  }
)

async function handleSave() {
  try {
    await formRef.value.validate()
  } catch (e) {
    return
  }
  saving.value = true
  const body = {
    name: form.name.trim(),
    type: form.type,
    source: form.type === 'testsrc' ? '' : form.source.trim(),
    width: form.width,
    height: form.height,
    framerate: form.framerate,
    bitrate_kbps: form.bitrate_kbps,
    infrared: form.infrared,
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
        <el-input v-model="form.name" placeholder="请输入名称" />
      </el-form-item>
      <el-form-item label="类型" prop="type">
        <el-select v-model="form.type" class="full-width">
          <el-option v-for="t in TYPE_OPTIONS" :key="t.value" :label="t.label" :value="t.value" />
        </el-select>
      </el-form-item>
      <el-form-item label="源" prop="source">
        <template v-if="isDShow && !dshowEnumFailed && dshowDevices.length">
          <div class="num-row">
            <el-select
              v-model="form.source"
              class="full-width"
              filterable
              allow-create
              default-first-option
              :loading="dshowLoading"
              placeholder="选择设备，或直接输入设备名"
            >
              <el-option v-for="d in dshowDevices" :key="d" :label="d" :value="d" />
            </el-select>
            <el-button
              class="enum-refresh"
              :loading="dshowLoading"
              @click="loadDShowDevices(true)"
            >刷新</el-button>
          </div>
        </template>
        <template v-else>
          <el-input
            v-model="form.source"
            :disabled="isTestSrc"
            :placeholder="SOURCE_PLACEHOLDER[form.type]"
          />
          <div v-if="isDShow" class="form-tip">
            {{ dshowEnumFailed
              ? '设备枚举不可用（仅 Windows 支持枚举），请手动填设备名；可用 ffmpeg -list_devices true -f dshow -i dummy 查询'
              : '未枚举到设备，请确认摄像头已连接后点刷新，或直接输入设备名' }}
          </div>
        </template>
      </el-form-item>
      <el-form-item label="分辨率">
        <div class="num-row">
          <el-input-number v-model="form.width" :min="0" :max="3840" controls-position="right" placeholder="宽" />
          <span class="num-sep">×</span>
          <el-input-number v-model="form.height" :min="0" :max="3840" controls-position="right" placeholder="高" />
        </div>
        <div class="form-tip">宽高为 0 表示自动</div>
      </el-form-item>
      <el-form-item label="帧率">
        <el-input-number v-model="form.framerate" :min="1" :max="60" />
      </el-form-item>
      <el-form-item label="码率">
        <div class="num-row">
          <el-input-number v-model="form.bitrate_kbps" :min="256" :max="8192" />
          <span class="num-sep">kbps</span>
        </div>
      </el-form-item>
      <el-form-item label="红外">
        <el-switch v-model="form.infrared" />
        <div class="form-tip">标记为红外通道后，ONVIF profile 名称会带 infrared，客户端可自动识别</div>
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
</style>

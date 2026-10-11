<script setup>
import { computed, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { updateDiscovery, updateRtspPort } from '../api'

const props = defineProps({
  system: { type: Object, default: null }
})
const emit = defineEmits(['updated'])
const switching = ref(false)

const rtspConfigured = computed(() =>
  props.system && props.system.rtsp_port_configured != null
    ? props.system.rtsp_port_configured
    : props.system && props.system.rtsp_port
)
// 配置端口与实际监听端口不一致时提示待重启（端口漂移或修改未生效）。
const rtspPending = computed(() => {
  if (!props.system) return false
  const cfg = props.system.rtsp_port_configured
  return cfg != null && cfg !== props.system.rtsp_port
})

const portDialogVisible = ref(false)
const portInput = ref(8554)
const portSaving = ref(false)

function openPortDialog() {
  portInput.value = rtspConfigured.value || 8554
  portDialogVisible.value = true
}

async function savePort() {
  if (!portInput.value || portInput.value < 1 || portInput.value > 65535) {
    ElMessage.warning('RTSP 端口取值范围 1-65535')
    return
  }
  portSaving.value = true
  try {
    const res = await updateRtspPort(portInput.value)
    ElMessage.success(
      res && res.restart_required ? 'RTSP 端口已保存，重启服务后生效' : 'RTSP 端口已保存'
    )
    portDialogVisible.value = false
    emit('updated')
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    portSaving.value = false
  }
}

async function copyEndpoint() {
  const text = props.system && props.system.onvif_endpoint
  if (!text) return
  try {
    await navigator.clipboard.writeText(text)
    ElMessage.success('已复制')
  } catch (e) {
    ElMessage.error('复制失败，请手动复制')
  }
}

async function onToggleDiscovery(val) {
  switching.value = true
  try {
    await updateDiscovery(val)
    ElMessage.success(val ? 'WS-Discovery 已开启' : 'WS-Discovery 已关闭')
    emit('updated')
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    switching.value = false
  }
}
</script>

<template>
  <el-card shadow="never">
    <template #header>
      <span class="card-title">系统信息</span>
    </template>
    <el-descriptions :column="2" border>
      <el-descriptions-item label="ONVIF 接入地址" :span="2">
        <span class="endpoint">{{ system && system.onvif_endpoint ? system.onvif_endpoint : '—' }}</span>
        <el-button size="small" text type="primary" class="copy-btn" @click="copyEndpoint">
          <el-icon><CopyDocument /></el-icon>复制
        </el-button>
      </el-descriptions-item>
      <el-descriptions-item label="RTSP 端口">
        <span>{{ system && system.rtsp_port != null ? system.rtsp_port : '—' }}</span>
        <el-tag v-if="rtspPending" size="small" type="warning" class="port-pending">
          已配置 {{ system.rtsp_port_configured }}，重启后生效
        </el-tag>
        <el-button
          v-if="system"
          size="small"
          text
          type="primary"
          class="copy-btn"
          @click="openPortDialog"
        >
          <el-icon><Edit /></el-icon>修改
        </el-button>
      </el-descriptions-item>
      <el-descriptions-item label="对外 IP">
        {{ system && system.advertise_ip ? system.advertise_ip : '—' }}
      </el-descriptions-item>
      <el-descriptions-item label="WS-Discovery">
        <el-switch
          v-if="system"
          :model-value="system.discovery_enabled"
          :loading="switching"
          @change="(v) => onToggleDiscovery(v)"
        />
        <span v-else>—</span>
      </el-descriptions-item>
      <el-descriptions-item label="版本">
        {{ system && system.version ? system.version : '—' }}
        <a
          href="https://github.com/LinuxSuRen/local-onvif-adapter"
          target="_blank"
          rel="noopener noreferrer"
          class="gh-link"
        >
          <el-icon><Link /></el-icon>GitHub
        </a>
      </el-descriptions-item>
      <el-descriptions-item label="已启用 profile 数">
        {{ system && system.profile_count != null ? system.profile_count : '—' }}
      </el-descriptions-item>
    </el-descriptions>

    <el-dialog
      v-model="portDialogVisible"
      title="修改 RTSP 端口"
      width="420px"
      :close-on-click-modal="false"
    >
      <el-form label-width="90px" @submit.prevent>
        <el-form-item label="RTSP 端口">
          <el-input-number
            v-model="portInput"
            :min="1"
            :max="65535"
            :step="1"
            step-strictly
            controls-position="right"
            style="width: 200px"
          />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="portDialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="portSaving" @click="savePort">保存</el-button>
      </template>
    </el-dialog>
  </el-card>
</template>

<style scoped>
.card-title {
  font-weight: 600;
}
.endpoint {
  word-break: break-all;
}
.copy-btn {
  margin-left: 8px;
}
.port-pending {
  margin-left: 8px;
}
.gh-link {
  margin-left: 12px;
  font-size: 13px;
  text-decoration: none;
  color: #409eff;
  display: inline-flex;
  align-items: center;
  gap: 2px;
}
.gh-link:hover {
  text-decoration: underline;
}
</style>

<script setup>
import { ref } from 'vue'
import { ElMessage } from 'element-plus'
import { updateDiscovery } from '../api'

const props = defineProps({
  system: { type: Object, default: null }
})
const emit = defineEmits(['updated'])
const switching = ref(false)

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
        {{ system && system.rtsp_port != null ? system.rtsp_port : '—' }}
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

<script setup>
import { ElMessage } from 'element-plus'

const props = defineProps({
  system: { type: Object, default: null }
})

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
        <el-tag v-if="system" :type="system.discovery_enabled ? 'success' : 'info'" size="small">
          {{ system.discovery_enabled ? '已开启' : '已关闭' }}
        </el-tag>
        <span v-else>—</span>
      </el-descriptions-item>
      <el-descriptions-item label="版本">
        {{ system && system.version ? system.version : '—' }}
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
</style>

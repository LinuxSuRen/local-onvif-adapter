<script setup>
import { computed, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { updateDiscovery, updateRtspPort, updateAuth } from '../api'

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

// 设备面认证（ONVIF SOAP + RTSP 共用账号；管理台本身不设防）
const authDialog = ref(false)
const authSaving = ref(false)
const authForm = ref({ enabled: false, username: '', password: '' })

function openAuthDialog() {
  authForm.value = {
    enabled: !!(props.system && props.system.auth_enabled),
    username: '',
    password: ''
  }
  authDialog.value = true
}

async function saveAuth() {
  if (authForm.value.enabled && (!authForm.value.username || !authForm.value.password)) {
    ElMessage.warning('开启认证需要填写用户名和密码')
    return
  }
  authSaving.value = true
  try {
    // 关闭认证时不传凭证，保留原账号仅在再次开启时输入
    await updateAuth(
      authForm.value.enabled,
      authForm.value.username,
      authForm.value.password
    )
    ElMessage.success(authForm.value.enabled ? '设备认证已开启' : '设备认证已关闭')
    authDialog.value = false
    emit('updated')
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    authSaving.value = false
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
      <el-descriptions-item label="设备认证">
        <el-tag v-if="system && system.auth_enabled" type="success" size="small" @click="openAuthDialog" style="cursor: pointer;">
          已开启
        </el-tag>
        <el-button v-else size="small" text type="primary" @click="openAuthDialog">
          未开启，配置
        </el-button>
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

    <el-dialog v-model="authDialog" title="设备访问认证" width="420px">
      <el-form label-width="80px">
        <el-form-item label="开启认证">
          <el-switch v-model="authForm.enabled" />
        </el-form-item>
        <template v-if="authForm.enabled">
          <el-form-item label="用户名">
            <el-input v-model="authForm.username" placeholder="如 admin" autocomplete="off" />
          </el-form-item>
          <el-form-item label="密码">
            <el-input v-model="authForm.password" type="password" show-password placeholder="RTSP/ONVIF 共用" autocomplete="new-password" />
          </el-form-item>
        </template>
        <el-alert
          v-if="authForm.enabled"
          type="info"
          :closable="false"
          show-icon
          title="ONVIF SOAP 与 RTSP 取流将要求该账号；对时/能力发现保持免认证；管理台不受影响；变更后取流进程自动重启"
        />
      </el-form>
      <template #footer>
        <el-button @click="authDialog = false">取消</el-button>
        <el-button type="primary" :loading="authSaving" @click="saveAuth">保存</el-button>
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

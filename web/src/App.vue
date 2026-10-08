<script setup>
import { onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { getSystem, getCameras, deleteCamera } from './api'
import SystemCard from './components/SystemCard.vue'
import CameraTable from './components/CameraTable.vue'
import CameraDialog from './components/CameraDialog.vue'
import SnapshotDialog from './components/SnapshotDialog.vue'
import PtzDrawer from './components/PtzDrawer.vue'

const system = ref(null)
const cameras = ref([])
const loading = ref(false)

const dialogVisible = ref(false)
const editingCamera = ref(null)
const snapshotVisible = ref(false)
const snapshotCamera = ref(null)
const ptzVisible = ref(false)
const ptzCamera = ref(null)

async function loadAll() {
  loading.value = true
  try {
    const [sys, cams] = await Promise.all([getSystem(), getCameras()])
    system.value = sys
    cameras.value = Array.isArray(cams) ? cams : []
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    loading.value = false
  }
}

onMounted(loadAll)

function openCreate() {
  editingCamera.value = null
  dialogVisible.value = true
}

function openEdit(camera) {
  editingCamera.value = camera
  dialogVisible.value = true
}

function openSnapshot(camera) {
  snapshotCamera.value = camera
  snapshotVisible.value = true
}

function openPtz(camera) {
  ptzCamera.value = camera
  ptzVisible.value = true
}

async function handleRemove(camera) {
  try {
    await deleteCamera(camera.id)
    ElMessage.success('已删除')
    await loadAll()
  } catch (e) {
    ElMessage.error(e.message)
  }
}
</script>

<template>
  <div class="page">
    <header class="page-header">
      <div class="header-left">
        <img src="/logo.svg" alt="logo" class="logo" />
        <div>
          <h1>local-onvif-adapter 管理台</h1>
          <p>将本地摄像头通过 ONVIF 协议暴露给客户端</p>
        </div>
      </div>
      <a
        href="https://github.com/LinuxSuRen/local-onvif-adapter"
        target="_blank"
        rel="noopener noreferrer"
        class="gh-btn"
        title="GitHub 项目主页"
      >
        <svg viewBox="0 0 16 16" width="22" height="22" fill="currentColor" aria-hidden="true">
          <path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27s1.36.09 2 .27c1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0 0 16 8c0-4.42-3.58-8-8-8Z"/>
        </svg>
      </a>
    </header>

    <SystemCard :system="system" class="block" />

    <CameraTable
      class="block"
      :cameras="cameras"
      :loading="loading"
      @add="openCreate"
      @refresh="loadAll"
      @edit="openEdit"
      @snapshot="openSnapshot"
      @ptz="openPtz"
      @remove="handleRemove"
    />

    <CameraDialog v-model:visible="dialogVisible" :camera="editingCamera" @saved="loadAll" />
    <SnapshotDialog v-model:visible="snapshotVisible" :camera="snapshotCamera" />
    <PtzDrawer v-model:visible="ptzVisible" :camera="ptzCamera" />
  </div>
</template>

<style>
body {
  margin: 0;
  background: #f5f7fa;
}
</style>

<style scoped>
.page {
  max-width: 1200px;
  margin: 0 auto;
  padding: 24px 16px 48px;
}
.page-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
}
.header-left {
  display: flex;
  align-items: center;
  gap: 12px;
}
.logo {
  width: 42px;
  height: 42px;
}
.gh-btn {
  color: #303133;
  text-decoration: none;
  display: flex;
  align-items: center;
  padding: 6px;
  border-radius: 6px;
  transition: background 0.2s;
}
.gh-btn:hover {
  background: #e8eaed;
}
.page-header h1 {
  margin: 0 0 4px;
  font-size: 22px;
  color: #303133;
}
.page-header p {
  margin: 0;
  color: #909399;
  font-size: 14px;
}
.block {
  margin-top: 16px;
}
</style>

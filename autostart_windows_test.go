//go:build windows

package main

import "testing"

// TestAutostartRegistryRoundtrip 在真实 HKCU Run 键上做注册/读取/删除往返，
// 结束时清理干净（值名固定为本程序的启动项名）。
func TestAutostartRegistryRoundtrip(t *testing.T) {
	want := `wscript.exe //B //Nologo "C:\some where\autostart.vbs"`
	if err := registerAutostart(want); err != nil {
		t.Fatalf("registerAutostart: %v", err)
	}
	t.Cleanup(func() { _ = unregisterAutostart() })

	got, ok := autostartRegistered()
	if !ok || got != want {
		t.Fatalf("autostartRegistered = %q, %v; want %q, true", got, ok, want)
	}
	if err := unregisterAutostart(); err != nil {
		t.Fatalf("unregisterAutostart: %v", err)
	}
	if _, ok := autostartRegistered(); ok {
		t.Fatal("value should be removed after unregister")
	}
	// 再次删除应幂等成功。
	if err := unregisterAutostart(); err != nil {
		t.Fatalf("unregisterAutostart should be idempotent: %v", err)
	}
}

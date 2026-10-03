//go:build node

package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// 首启生成 → 重读一致 → 权限 0600 → 指纹稳定
func TestNodeKeyLifecycle(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "node.key")

	priv, fp, err := loadOrCreateNodeKey(p, "", false)
	if err != nil {
		t.Fatalf("首启应能生成: %v", err)
	}
	if len(priv) != ed25519.PrivateKeySize || fp == "" {
		t.Fatalf("私钥/指纹异常: %d %q", len(priv), fp)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	// Windows 没有 POSIX 权限位（报 0666），权限验证放到 Linux 部署后做
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("私钥权限必须 0600，实际 %o", st.Mode().Perm())
	}
	// 文件内容是 32 字节种子的 hex
	raw, _ := os.ReadFile(p)
	b, err := hex.DecodeString(string(trimNL(raw)))
	if err != nil || len(b) != ed25519.SeedSize {
		t.Fatalf("文件内容应为 32 字节 hex: %v len=%d", err, len(b))
	}
	// 再读一次：同一把密钥、同一指纹
	priv2, fp2, err := loadOrCreateNodeKey(p, fp, false)
	if err != nil {
		t.Fatal(err)
	}
	if fp2 != fp || !priv2.Equal(priv) {
		t.Fatalf("重读应得到同一身份: %q vs %q", fp2, fp)
	}
	// 指纹定义：对原始 32 字节公钥算 SHA-256 前 16 位
	want := pubkeyFingerprint(priv.Public().(ed25519.PublicKey))
	if want != fp {
		t.Fatalf("指纹算法不一致: %q vs %q", want, fp)
	}
}

// 密钥丢失（配置里有指纹、文件没了）必须报错，除非显式 KeyRegen
func TestNodeKeyLostRefuses(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "node.key")
	_, fp, err := loadOrCreateNodeKey(p, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadOrCreateNodeKey(p, fp, false); err == nil {
		t.Fatalf("密钥丢失时必须报错拒绝启动（不能静默换身份）")
	}
	// 显式接受换身份 → 允许生成，且新指纹与旧的不同
	_, fp2, err := loadOrCreateNodeKey(p, fp, true)
	if err != nil {
		t.Fatalf("KeyRegen=true 时应允许重新生成: %v", err)
	}
	if fp2 == fp {
		t.Fatalf("重新生成的身份不应与旧指纹相同")
	}
	// 全新节点（没有记录过指纹）→ 直接生成，不报错
	if _, _, err := loadOrCreateNodeKey(filepath.Join(dir, "n2.key"), "", false); err != nil {
		t.Fatalf("全新节点应能生成: %v", err)
	}
}

// 私钥被换成另一把（例如把别的机器的密钥拷过来）必须被发现
func TestNodeKeyTamperDetected(t *testing.T) {
	dir := t.TempDir()
	pa := filepath.Join(dir, "a.key")
	pb := filepath.Join(dir, "b.key")
	_, fpA, _ := loadOrCreateNodeKey(pa, "", false)
	if _, _, err := loadOrCreateNodeKey(pb, "", false); err != nil {
		t.Fatal(err)
	}
	// 把 b 的密钥覆盖到 a 上
	rawB, _ := os.ReadFile(pb)
	if err := os.WriteFile(pa, rawB, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadOrCreateNodeKey(pa, fpA, false); err == nil {
		t.Fatalf("指纹与文件不一致时必须报错")
	}
}

// 并发首启（两个进程同时创建）必须收敛到同一把密钥
func TestNodeKeyConcurrentCreate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "race.key")
	var wg sync.WaitGroup
	fps := make([]string, 8)
	for i := range fps {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			priv, fp, err := loadOrCreateNodeKey(p, "", false)
			if err == nil && priv != nil {
				fps[i] = fp
			}
		}(i)
	}
	wg.Wait()
	first := ""
	for _, fp := range fps {
		if fp == "" {
			t.Fatalf("并发创建中有失败: %v", fps)
		}
		if first == "" {
			first = fp
		} else if fp != first {
			t.Fatalf("并发首启产生了不同身份: %v", fps)
		}
	}
}

// 签名与验签：换名字/换 nonce 必须失败
func TestSignHandshakeAndVerify(t *testing.T) {
	dir := t.TempDir()
	priv, _, err := loadOrCreateNodeKey(filepath.Join(dir, "k.key"), "", false)
	if err != nil {
		t.Fatal(err)
	}
	pub := priv.Public().(ed25519.PublicKey)
	nonce := "n-abc"
	sig, err := hex.DecodeString(signHandshake(priv, "node-x", nonce))
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(pub, []byte(authMsgHandshake("node-x", nonce)), sig) {
		t.Fatalf("合法签名应通过")
	}
	if ed25519.Verify(pub, []byte(authMsgHandshake("node-y", nonce)), sig) {
		t.Fatalf("换名字不应通过")
	}
	if ed25519.Verify(pub, []byte(authMsgHandshake("node-x", "n-zzz")), sig) {
		t.Fatalf("换 nonce 不应通过")
	}
	// HTTP 签名绑定请求体
	body := []byte(`{"a":1}`)
	s2, _ := hex.DecodeString(signHTTP(priv, "node-x", 1700000000, body))
	if !ed25519.Verify(pub, []byte(authMsgHTTP("node-x", 1700000000, bodyDigestHex(body))), s2) {
		t.Fatalf("HTTP 合法签名应通过")
	}
	if ed25519.Verify(pub, []byte(authMsgHTTP("node-x", 1700000000, bodyDigestHex([]byte(`{"a":2}`)))), s2) {
		t.Fatalf("请求体被改不应通过")
	}
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}

//go:build node

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// 节点私钥管理。
//
// 设计要点（与中心侧的协议约定一致）：
//   - 私钥**只在这台机器上生成**，永不发送、永不打印（只打印公钥指纹供人工核对）；
//   - 文件权限 0600，先写临时文件 + fsync + rename，避免写一半被读到或断电损坏；
//   - 用 O_CREATE|O_EXCL 解决"两个进程同时首启"的竞态：输的那个回读赢家的文件；
//   - "密钥丢失"必须**明确报错**而不是静默换一个身份：判据是配置文件里记录的指纹
//     （不能用"有没有 token"判断 —— 新节点在首次启动前就已经拿到 token 了）。

const defaultKeyPath = "/etc/pingatlas/node.key"

// nodeKeyMu 串行化本进程内的密钥初始化：多个 goroutine 同时首启时只能有一个赢家，
// 其余都回读赢家写下的文件（跨进程的竞态由 O_EXCL/硬链接保证）。
var nodeKeyMu sync.Mutex

// loadOrCreateNodeKey 读取私钥；不存在时按 savedFingerprint 决定"创建"还是"报错"。
//
//	savedFingerprint 为空  → 全新节点，创建密钥并把指纹记进配置
//	savedFingerprint 非空  → 曾经有过密钥；此时密钥文件不见了 = 丢失/被删 → 报错拒绝启动
//	allowRegen 为 true     → 运维明确接受"重新生成身份"（配置里显式打开）
func loadOrCreateNodeKey(path, savedFingerprint string, allowRegen bool) (ed25519.PrivateKey, string, error) {
	nodeKeyMu.Lock()
	defer nodeKeyMu.Unlock()
	if path == "" {
		path = defaultKeyPath
	}
	priv, err := readNodeKey(path)
	if err == nil {
		fp := pubkeyFingerprint(priv.Public().(ed25519.PublicKey))
		if savedFingerprint != "" && savedFingerprint != fp {
			// 文件被换成另一把密钥（例如把别的机器的密钥拷过来了）——必须让运维知道
			return nil, "", fmt.Errorf("私钥与配置里记录的指纹不一致（配置 %s / 实际 %s）："+
				"请确认没有把其它机器的 node.key 拷过来；确实要换身份请设置 KeyRegen=true", savedFingerprint, fp)
		}
		return priv, fp, nil
	}
	if !os.IsNotExist(err) {
		return nil, "", err
	}
	if savedFingerprint != "" && !allowRegen {
		return nil, "", fmt.Errorf("私钥文件 %s 不存在，但配置里记录过指纹 %s —— 密钥丢失。"+
			"出于安全考虑拒绝静默生成新身份：请从备份恢复该文件，或在中心重新登记，"+
			"或确认要换身份时设置 KeyRegen=true", path, savedFingerprint)
	}
	priv, fp, err := createNodeKey(path)
	if err != nil {
		return nil, "", err
	}
	return priv, fp, nil
}

// readNodeKey 读取 hex 编码的 32 字节种子
func readNodeKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := strings.TrimSpace(string(raw))
	seed, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("私钥文件格式错误（应为 hex）: %v", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("私钥长度错误：期望 %d 字节种子，实际 %d", ed25519.SeedSize, len(seed))
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// createNodeKey 生成并落盘（0600 + 唯一临时文件 + fsync + 原子占位）。
//
// 并发安全：先把种子写进**唯一的**临时文件并 fsync，再用 os.Link 原子占位目标路径；
// link 失败说明别人已经建好了 → 回读磁盘上的那份，保证全系统只有一把密钥（不会出现
// "两个进程各持一把、互相顶替"的情况）。
func createNodeKey(path string) (ed25519.PrivateKey, string, error) {
	dir := filepath.Dir(path)
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, "", err
		}
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, "", err
	}
	tmp := fmt.Sprintf("%s.tmp-%d-%s", path, os.Getpid(), hex.EncodeToString(seed[:4]))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, "", err
	}
	_, werr := f.WriteString(hex.EncodeToString(seed) + "\n")
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(tmp)
		return nil, "", werr
	}

	linked := false
	if err := os.Link(tmp, path); err == nil {
		linked = true
	}
	os.Remove(tmp)
	if !linked {
		// 别人赢了 → 用它写的密钥（绝不各用一把）
		if priv, rerr := readNodeKey(path); rerr == nil {
			return priv, pubkeyFingerprint(priv.Public().(ed25519.PublicKey)), nil
		}
		// 文件系统不支持硬链接等极端情况：退化为"排他创建 + 直接写"
		return writeNodeKeyDirect(path, seed)
	}
	// 目录 fsync：保证 link 本身也落盘（断电后文件仍在）
	if d, derr := os.Open(filepath.Dir(path)); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return priv, pubkeyFingerprint(priv.Public().(ed25519.PublicKey)), nil
}

// writeNodeKeyDirect 退化路径：排他创建目标文件再写入（O_EXCL 保证只有一个能建成）
func writeNodeKeyDirect(path string, seed []byte) (ed25519.PrivateKey, string, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if priv, rerr := readNodeKey(path); rerr == nil {
			return priv, pubkeyFingerprint(priv.Public().(ed25519.PublicKey)), nil
		}
		return nil, "", err
	}
	_, werr := f.WriteString(hex.EncodeToString(seed) + "\n")
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(path)
		return nil, "", werr
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return priv, pubkeyFingerprint(priv.Public().(ed25519.PublicKey)), nil
}

// signHandshake 用私钥签"名字|nonce"
func signHandshake(priv ed25519.PrivateKey, name, nonce string) string {
	return hex.EncodeToString(ed25519.Sign(priv, []byte(authMsgHandshake(name, nonce))))
}

// signHTTP 用私钥签"名字|时间戳|请求体摘要"
func signHTTP(priv ed25519.PrivateKey, name string, ts int64, body []byte) string {
	return hex.EncodeToString(ed25519.Sign(priv, []byte(authMsgHTTP(name, ts, bodyDigestHex(body)))))
}

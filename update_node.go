//go:build node

package main

// ---------- 节点自升级 ----------
//
// 中心 cmd{update, ver, sha256} → 节点比对版本 → HTTPS 从中心拉对应架构二进制
// → sha256 校验 → 写 exe.new → 原子 rename 覆盖（Linux 下覆盖运行中二进制安全：
// 旧进程持有 inode）→ os.Exit(0) 交 systemd/OpenRC 拉起 → 重连后心跳版本号即回执。

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

func (n *Node) selfUpdate(ver, sha string) {
	if ver == "" || ver == Version {
		log.Printf("node: 自升级跳过（版本已为 %s）", Version)
		return
	}
	// sha256 必须由中心下发：/agent/download 返回的是"当前放在 center 旁边的二进制"，
	// 它未必就是管理员想要的那个 ver。空校验等于让节点盲装一个未经确认的文件。
	if sha == "" {
		log.Printf("node: 自升级拒绝（未提供 sha256，无法确认下载到的是 %s）", ver)
		return
	}
	c := n.conf.Load().(NodeConf)
	if c.Center == "" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		log.Printf("node: 自升级失败（定位二进制）: %v", err)
		return
	}
	if p, aerr := filepath.Abs(exe); aerr == nil {
		exe = p
	}
	url := c.Center + "/agent/download?arch=" + runtime.GOARCH
	tmp := exe + ".new"

	log.Printf("node: 自升级 %s -> %s，下载 %s", Version, ver, url)
	dl := n.dlClient
	if dl == nil { // 兜底：测试或异常构造的 Node 没有配置下载 client
		dl = &http.Client{Timeout: 10 * time.Minute}
	}
	resp, err := dl.Get(url)
	if err != nil {
		log.Printf("node: 自升级下载失败: %v", err)
		return
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		log.Printf("node: 自升级下载失败 status=%d", resp.StatusCode)
		return
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		resp.Body.Close()
		log.Printf("node: 自升级写文件失败: %v", err)
		return
	}
	h := sha256.New()
	_, werr := io.Copy(io.MultiWriter(f, h), resp.Body)
	resp.Body.Close()
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(tmp)
		log.Printf("node: 自升级写文件失败: %v", werr)
		return
	}
	got := hex.EncodeToString(h.Sum(nil))
	// sha 已在上方做过非空校验，这里必须严格比对：
	// /agent/download 给出的是 center 目录里"当前那个"二进制，不保证等于目标 ver。
	if got != sha {
		os.Remove(tmp)
		log.Printf("node: 自升级校验失败 sha256 不匹配（期望 %s... 实际 %s...），已丢弃", sha[:min(12, len(sha))], got[:12])
		return
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		log.Printf("node: 自升级 chmod 失败: %v", err)
		return
	}
	// 替换前先留一份 .bak（B6）：不自动回滚，但人工一条 cp 就能退回去，
	// 避免"新二进制本身有问题 ⇒ Restart=always 无限重启循环 ⇒ 节点彻底失联"。
	if err := copyFile(exe, exe+".bak"); err != nil {
		log.Printf("node: 自升级警告：备份旧版本失败(%v)，继续升级（无回滚副本）", err)
	} else {
		log.Printf("node: 旧版本已备份到 %s.bak", exe)
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		log.Printf("node: 自升级替换失败: %v", err)
		return
	}
	log.Printf("node: 已升级到 %s（sha256 %s...），进程退出等待拉起", ver, got[:12])
	time.Sleep(500 * time.Millisecond) // 给日志一点落盘时间
	os.Exit(0)
}

// copyFile 复制 src 到 dst（保留权限），用于升级前留 .bak
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

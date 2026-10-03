//go:build center

package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
)

// NodeGeo 探测节点的中文显示名（首页节点胶囊/表格/汇总用），定义见 matrix.go。
// 地图上不再标注探测节点的位置，所以**不需要经纬度**，新增节点也不必填坐标：
//
//	"NodeGeo": {
//	  "node-bj-01": {"Label":"北京一号"},
//	  "node-xm-01": {"Label":"厦门一号"}
//	}
//
// 缺省会退回节点 id 作为显示名。旧配置里残留的 lng/lat 会被忽略（不影响启动）。

// CenterConf 中心端配置
type CenterConf struct {
	Listen   string // 监听地址，如 :18991 或 127.0.0.1:18991
	AdminPwd string // 管理面板登录密码（与节点鉴权彻底分离）
	Token    string // 全局节点上报鉴权 token（兼容；节点也可用各自独立 token）
	DB       string // postgres dsn；空 = 内存模式（测试用）
	// TrustedProxies 额外的可信反向代理 IP/CIDR（回环地址默认可信）。
	// 只有来自可信代理的请求才采信 X-Forwarded-For 的最后一跳；跨机反代必须在此登记，
	// 否则限流按代理 IP 计数。
	TrustedProxies []string `json:"TrustedProxies,omitempty"`
	// NodeGeo 节点显示名与坐标（首页矩阵/地图用，纯展示，不参与鉴权）
	NodeGeo map[string]NodeGeo `json:"NodeGeo,omitempty"`
	// SelfNode 与中心同机部署的节点名（首页卡片上打"本机"标记）；留空则不打标记
	SelfNode string `json:"SelfNode,omitempty"`
}

// loadCenterConf 读取中心配置 center.json：
//   {"Listen":"127.0.0.1:18991","AdminPwd":"...","Token":"xxx","DB":"postgres://..."}
// 命令行 --listen / --db 可覆盖文件值。
// 兼容迁移：老配置只有 Token（旧版 admin 密码与节点 token 共用此字段），
// 首次加载把 Token 值复制给 AdminPwd，之后改密码只动 AdminPwd，不再牵连节点鉴权。
// Token 缺失时不允许静默兜底弱默认值：随机生成并写回配置文件。
func loadCenterConf(path, listenFlag, dbFlag string) CenterConf {
	c := CenterConf{Listen: ":18991"}
	tokenKeyPresent := false
	if path != "" {
		raw, err := os.ReadFile(path)
		if err == nil {
			// 要区分"配置里没有 Token 键"和"配置里写了 Token 但为空"：
			// 前者=没配过（保持随机生成并写回的便利行为），
			// 后者=明确要求"每节点独立 token、不要全局万能钥匙"。
			var doc map[string]json.RawMessage
			if json.Unmarshal(raw, &doc) == nil {
				if v, ok := doc["Token"]; ok {
					tokenKeyPresent = true
					_ = v
				}
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				fatal("center: 解析配置失败: %v", err)
			}
		} else if !os.IsNotExist(err) {
			fatal("center: 读取配置失败: %v", err)
		}
	}
	if listenFlag != "" {
		c.Listen = listenFlag
	}
	if dbFlag != "" {
		c.DB = dbFlag
	}
	if c.AdminPwd == "" && c.Token != "" {
		c.AdminPwd = c.Token // 一次性迁移：老配置密码=token
		log.Printf("center: 检测到旧版配置，AdminPwd 已从 Token 迁移（改密码不再影响节点鉴权）")
	}
	switch {
	case c.Token != "":
		// 配了全局 token：照旧（兼容尚未换独立 token 的老节点）
	case tokenKeyPresent:
		// 显式留空：不再生成，也不再接受全局 token —— 每个节点必须用自己的独立 token
		log.Printf("center: 全局 Token 显式留空 → 只接受各节点独立 token（万能钥匙已关闭）")
		if c.AdminPwd == "" {
			log.Printf("center: 警告：AdminPwd 为空，管理接口会全部返回 401；请在配置里设置 AdminPwd")
		}
	default:
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			fatal("center: 生成随机 token 失败: %v", err)
		}
		c.Token = hex.EncodeToString(raw)
		if path != "" {
			if err := persistCenterToken(path, c.Token); err != nil {
				log.Printf("center: 随机 token 写回配置失败(仅本次运行有效): %v", err)
			} else {
				log.Printf("center: 配置缺 Token，已随机生成并写回 %s", path)
			}
		} else {
			log.Printf("center: 配置缺 Token，已随机生成(未持久化)")
		}
	}
	return c
}

// persistCenterToken 把随机生成的 token 原子写回配置文件（0600）
func persistCenterToken(path, token string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	m["Token"] = token
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

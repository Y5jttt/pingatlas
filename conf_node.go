//go:build node

package main

import (
	"encoding/json"
	"os"
)

// NodeConf 节点端配置（兼容旧 pingatlas config.json 的 Chinamap 格式）
// center 模式下，远程下发的目标列表会回写进 config.json 的 targets/targets_ver
// 字段持久化：节点重启即使中心不可达，也能用最近一次下发的列表继续探测。
type NodeConf struct {
	Name       string   `json:"Name"`
	Center     string   `json:"Center,omitempty"`
	Token      string   `json:"Token,omitempty"`
	Targets    []Target `json:"targets,omitempty"`     // 远程下发并已持久化的目标（优先于 Chinamap）
	TargetsVer int64    `json:"targets_ver,omitempty"` // 持久化对应的远程配置版本
	SrcIP      string   `json:"SrcIP,omitempty"`
	DBPath     string   `json:"DBPath,omitempty"` // SQLite 绝对路径，缺省 /var/lib/pingatlas/pingatlas_node.db
	// KeyPath Ed25519 私钥路径（缺省 /etc/pingatlas/node.key，权限 0600，永不外发）
	KeyPath string `json:"KeyPath,omitempty"`
	// PubkeyFP 上次运行的公钥指纹：文件丢失时可据此判定"身份丢了"而不是"全新节点"
	PubkeyFP string `json:"pubkey_fp,omitempty"`
	// KeyRegen 明确接受"重新生成身份"（密钥丢失又无法恢复时才打开）
	KeyRegen bool `json:"KeyRegen,omitempty"`
}

// ---- 旧 config.json 兼容结构 ----

type legacyCenter struct {
	Endpoint string `json:"Endpoint"`
	Token    string `json:"Token"`
}

type legacyChinamap struct {
	Name string   `json:"Name"`
	Ips  []string `json:"Ips"`
}

type legacyConfig struct {
	Name     string           `json:"Name"`
	Center   *legacyCenter    `json:"Center"`
	Chinamap []legacyChinamap `json:"Chinamap"`
}

// loadNodeConf 读取节点配置。在旧 config.json 里追加 Center 段即可接入中心：
//   "Center": {"Endpoint":"http://ip:18991","Token":"xxx"}
// Center 为空时节点以纯本地模式运行（数据只进本地 SQLite）。
func loadNodeConf(path string) NodeConf {
	c := NodeConf{}
	raw, err := os.ReadFile(path)
	if err != nil {
		fatal("node: 读取配置失败: %v", err)
	}
	var lg legacyConfig
	if err := json.Unmarshal(raw, &lg); err != nil {
		fatal("node: 解析配置失败: %v", err)
	}
	c.Name = lg.Name
	if c.Name == "" {
		h, _ := os.Hostname()
		c.Name = h
	}
	if lg.Center != nil {
		c.Center = lg.Center.Endpoint
		c.Token = lg.Center.Token
	}
	// DBPath 单独取（其余字段走 legacy 结构解析）
	var misc struct {
		DBPath string `json:"DBPath"`
	}
	if err := json.Unmarshal(raw, &misc); err == nil {
		c.DBPath = misc.DBPath
	}
	// 已持久化的远程目标优先（节点重启时中心可能不可达，用最近一次下发的列表）。
	// 关键：**以"targets 键是否存在"为准，而不是"列表是否非空"**。
	// 管理员清空目标表是合法终态，saveNodeTargets 会写入 `"targets": []`；
	// 旧实现要求 len>0 才认，于是清空后重启会回退到旧 config.json 里的 Chinamap，
	// 节点继续探测早已删除的目标，而中心把这批数据的行全部按"不在目标表内"拒收。
	var persist struct {
		Targets    []Target `json:"targets"`
		TargetsVer int64    `json:"targets_ver"`
		KeyPath    string   `json:"KeyPath"`
		PubkeyFP   string   `json:"pubkey_fp"`
		KeyRegen   bool     `json:"KeyRegen"`
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) == nil {
		if _, hasTargets := doc["targets"]; hasTargets {
			if err := json.Unmarshal(raw, &persist); err == nil {
				c.Targets = persist.Targets
				if c.Targets == nil {
					c.Targets = []Target{} // 显式空表：不要再回退 Chinamap
				}
				c.TargetsVer = persist.TargetsVer
				c.KeyPath = persist.KeyPath
				c.PubkeyFP = persist.PubkeyFP
				c.KeyRegen = persist.KeyRegen
				return c
			}
		}
	}
	// 没有 targets 键时也要把身份相关字段读出来（否则"密钥丢失"判据会失效）
	var misc2 struct {
		KeyPath  string `json:"KeyPath"`
		PubkeyFP string `json:"pubkey_fp"`
		KeyRegen bool   `json:"KeyRegen"`
	}
	if json.Unmarshal(raw, &misc2) == nil {
		c.KeyPath, c.PubkeyFP, c.KeyRegen = misc2.KeyPath, misc2.PubkeyFP, misc2.KeyRegen
	}
	for _, cm := range lg.Chinamap {
		for _, ip := range cm.Ips {
			if ip == "" {
				continue
			}
			c.Targets = append(c.Targets, Target{Alias: ip, IP: ip})
		}
	}
	return c
}

// saveNodePubkeyFP 把公钥指纹写回 config.json（只改这一个键，原子写 + 0600）。
// 用途：下次启动若发现"配置里有指纹但私钥文件不见了"，就能判定为密钥丢失并拒绝启动，
// 而不是静默换一个新身份上线。
func saveNodePubkeyFP(path, fp string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	fb, _ := json.Marshal(fp)
	doc["pubkey_fp"] = fb
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(out); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// saveNodeTargets 把远程下发的目标列表持久化回 config.json（center 模式热加载后调用）。
// 只替换 targets/targets_ver 两个键，其余字段（Name/Center/Token/Chinamap 等）原样保留；
// 原子写入：先写 .tmp + fsync，再 rename，避免写一半被读到或断电损坏。
func saveNodeTargets(path string, targets []Target, ver int64) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	tb, _ := json.Marshal(targets)
	vb, _ := json.Marshal(ver)
	doc["targets"] = tb
	doc["targets_ver"] = vb
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	// 0600：config.json 含节点 Token，回写不能把 install.sh 设置的 600 权限冲成全局可读
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(out); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

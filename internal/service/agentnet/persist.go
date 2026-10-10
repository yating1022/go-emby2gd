package agentnet

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// agentsFileVersion agents.json 的 schema 版本
//
// 版本号不匹配时拒绝加载而不是猜测: 注册表是"节点全部凭据"的唯一来源,
// 猜错的代价是全员离线。
const agentsFileVersion = 1

// agentsFilePerm agents.json 的文件权限
//
// 文件里存着每个节点的 secret 与 sign_key(等同于节点身份的凭据),
// 因此固定 0600: 与 agent 侧的 /etc/gd-agent/config.env 同一级别。
const agentsFilePerm = 0o600

// stateDirPerm 状态目录的权限
const stateDirPerm = 0o755

// agentsFile agents.json 的落盘结构
type agentsFile struct {
	// Version schema 版本号
	Version int `json:"version"`
	// Agents 全部节点记录
	Agents []agentFileEntry `json:"agents"`
}

// agentFileEntry 单个节点的落盘结构
//
// 字段名固定 snake_case: 与协议风格一致, 且让这个文件对运维可读。
type agentFileEntry struct {
	ID            string `json:"id"`
	MachineID     string `json:"machine_id"`
	Name          string `json:"name"`
	Secret        string `json:"secret"`
	SignKey       string `json:"sign_key"`
	PublicBaseURL string `json:"public_base_url"`
	ListenPort    int    `json:"listen_port"`
	Version       string `json:"version"`
	LastIP        string `json:"last_ip"`
	// Role 节点角色(node / hub); omitempty 与语义一致:
	// 缺省角色 node 不写, 旧文件没有该字段时也按 node 处理。
	Role string `json:"role,omitempty"`
	// Priority 调度优先级(越小越优先); omitempty 与语义一致:
	// 缺省值 0 就是"未设置 = 最优先", 旧文件没有该字段时也按 0 处理。
	Priority  int       `json:"priority,omitempty"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// persistLocked 把当前注册表原子写入磁盘(同目录临时文件 + rename)
//
// 调用方必须持有写锁; 写盘失败返回中文错误, 由调用方负责回滚内存变更 ——
// 保证"内存与磁盘一致", 并且绝不在落盘成功之前向节点/管理员返回成功。
func (r *registry) persistLocked(path string) error {
	entries := make([]agentFileEntry, 0, len(r.records))
	for _, rec := range r.records {
		entries = append(entries, rec.toFileEntry())
	}
	// 固定输出顺序: 文件内容对 diff / 备份友好
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].ID < entries[j].ID
	})

	payload, err := json.MarshalIndent(agentsFile{Version: agentsFileVersion, Agents: entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 agent 注册表失败: %w", err)
	}
	payload = append(payload, '\n')

	if err := os.MkdirAll(filepath.Dir(path), stateDirPerm); err != nil {
		return fmt.Errorf("创建 agent 状态目录失败: %w", err)
	}

	// 先写临时文件再改名: 避免写一半被读到残缺注册表
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, payload, agentsFilePerm); err != nil {
		return fmt.Errorf("写入 agent 注册表失败: %w", err)
	}
	// 显式 chmod: 文件已存在(旧版本可能权限更宽)时 WriteFile 不会收紧权限
	if err := os.Chmod(tmp, agentsFilePerm); err != nil {
		return fmt.Errorf("设置 agent 注册表权限失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("保存 agent 注册表失败: %w", err)
	}
	return nil
}

// loadAgentsFile 读取注册表文件
//
// 文件不存在视为首次启动(空表); 解析失败返回中文错误与处置建议 ——
// 宁可启动失败, 也不静默清空注册表: 清空会让全部节点凭空消失,
// 现场现象是"所有节点莫名离线", 比一条明确的启动错误难查得多。
func loadAgentsFile(path string) (map[string]*agentRecord, error) {
	data, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return map[string]*agentRecord{}, nil
	case err != nil:
		return nil, fmt.Errorf("读取 agent 注册表 %s 失败: %w", path, err)
	}

	var file agentsFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("解析 agent 注册表 %s 失败: %w。该文件可能已损坏或被手工改错; "+
			"请修复它, 或删除后重启(删除会丢失全部节点注册, 需在各节点重跑安装脚本)", path, err)
	}
	if file.Version != agentsFileVersion {
		return nil, fmt.Errorf("agent 注册表 %s 的版本号 %d 不受支持(当前程序支持 %d)。请勿手工修改该文件; "+
			"如确认要重置, 删除它后重启, 并在各节点重跑安装脚本", path, file.Version, agentsFileVersion)
	}

	records := make(map[string]*agentRecord, len(file.Agents))
	machines := make(map[string]string, len(file.Agents))
	for i, entry := range file.Agents {
		// 缺凭据的记录无法参与鉴权与调度, 属于损坏而不是"可以容忍的残缺"
		if entry.ID == "" || entry.MachineID == "" || entry.Secret == "" || entry.SignKey == "" {
			return nil, fmt.Errorf("agent 注册表 %s 的第 %d 条记录字段不完整(id/machine_id/secret/sign_key 均为必填)。"+
				"请修复它, 或删除后重启并在各节点重跑安装脚本", path, i+1)
		}
		if _, dup := records[entry.ID]; dup {
			return nil, fmt.Errorf("agent 注册表 %s 存在重复的节点 id(%s), 文件已损坏; 请删除后重启并在各节点重跑安装脚本",
				path, entry.ID)
		}
		if owner, dup := machines[entry.MachineID]; dup {
			return nil, fmt.Errorf("agent 注册表 %s 存在重复的 machine_id(%s, 同时属于节点 %s 与 %s), 文件已损坏; "+
				"请删除后重启并在各节点重跑安装脚本", path, entry.MachineID, owner, entry.ID)
		}
		records[entry.ID] = agentRecordFromFileEntry(entry)
		machines[entry.MachineID] = entry.ID
	}
	return records, nil
}

// machineIndex 由节点记录重建 machine_id → 节点 id 的索引
func machineIndex(records map[string]*agentRecord) map[string]string {
	machines := make(map[string]string, len(records))
	for id, rec := range records {
		machines[rec.MachineID] = id
	}
	return machines
}

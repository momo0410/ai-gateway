// Package auth 解析 WorkBuddy auth 文件（嵌套形/扁平形双形态），
// 提供 refresh 后的原子写回。
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Auth 是归一化后的账号凭证（来源可以是插件 OAuth 嵌套形或手写扁平形）。
type Auth struct {
	// mu 串行化 RefreshToken 写与 SaveAtomic 读，防止并发写回半更新 token。
	mu sync.Mutex

	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
	Domain       string
	UID          string
	EnterpriseID string
	Nickname     string
	FilePath     string // 来源文件；refresh 后原子写回此处

	// SoonestExpireAt 仍有剩余积分的套餐中最早的到期时刻（Unix 秒）；0 = 未知。
	//
	// 由凭证文件里的 credit 块带入（宿主 workbuddy-switch 查询积分后写入），
	// 或由网关自身的签到任务刷新。账号池据此做「先烧快过期额度」的分层选号。
	// 只读元数据，不参与 token 刷新与写回。
	SoonestExpireAt int64
}

// Lock 供同进程内其他包（upstream.RefreshToken）在改写 Auth 字段期间加锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// Region 账号所属服务区域。
//
// 存在的意义：**同名模型在两个区域可能是不同的后端模型**，能力并不一致。
// 实测（2026-09-16，/v3/config 元数据 + 逐账号发图验证）：
//
//	glm-5.3  国服「旗舰模型，擅长复杂软件工程与长程 Agent 任务」out=64000
//	         国际版「能力均衡，适合日常使用」                out=48000
//	         —— 国服后端能读图；国际版后端把图片换成固定占位符（token 增量
//	         恒为 +29，与图片体积无关），模型只能回答「无法查看图片」。
//
// 因此「同一个 glm-5.3 时好时坏」的真实原因是**选号随机命中了两个不同后端**，
// 而不是模型本身不稳定。见 pool.PickForModelRegion 与 server 的区域路由。
type Region int

const (
	// RegionAny 不限区域（默认；等价于引入本概念之前的行为）。
	RegionAny Region = iota
	// RegionCN 国服（*.workbuddy.cn / *.codebuddy.cn）。
	RegionCN
	// RegionIntl 国际版（*.workbuddy.ai / *.codebuddy.ai）。
	RegionIntl
)

func (r Region) String() string {
	switch r {
	case RegionCN:
		return "cn"
	case RegionIntl:
		return "intl"
	default:
		return "any"
	}
}

// Region 返回账号所属区域。
//
// 判据与 upstream.IsIntl 完全一致（依据凭证里的 domain 字段）：
// 以 .ai 结尾为国际版，其余（含 domain 缺失）按国服处理 —— 历史上只存在
// 国服账号，缺失时按国服保持向后兼容。
func (a *Auth) Region() Region {
	if a.IsIntl() {
		return RegionIntl
	}
	return RegionCN
}

// IsIntl 报告账号是否属于国际版。
func (a *Auth) IsIntl() bool {
	if a == nil {
		return false
	}
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(a.Domain)), ".ai")
}

// Parse 兼容两种磁盘形态：
//
//	嵌套形 {"auth":{...},"account":{...}}  （插件 OAuth 输出）
//	扁平形 {"accessToken":...,"uid":...}   （手写/旧版）
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var a Auth
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
				Domain       string `json:"domain"`
			} `json:"auth"`
			Account struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			} `json:"account"`
			Credit creditBlock `json:"credit"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:     n.Auth.AccessToken,
			RefreshToken:    n.Auth.RefreshToken,
			ExpiresAt:       n.Auth.ExpiresAt,
			Domain:          n.Auth.Domain,
			UID:             n.Account.UID,
			EnterpriseID:    n.Account.EnterpriseID,
			Nickname:        n.Account.Nickname,
			SoonestExpireAt: normalizeEpoch(n.Credit.SoonestExpireAt),
		}
	} else {
		var f struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
			// 扁平形把 credit 字段平铺在顶层（兼容旧版手写凭证）。
			SoonestExpireAt int64 `json:"soonestExpireAt"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:     f.AccessToken,
			RefreshToken:    f.RefreshToken,
			ExpiresAt:       f.ExpiresAt,
			Domain:          f.Domain,
			UID:             f.UID,
			EnterpriseID:    f.EnterpriseID,
			Nickname:        f.Nickname,
			SoonestExpireAt: normalizeEpoch(f.SoonestExpireAt),
		}
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	return &a, nil
}

// creditBlock 凭证文件里的积分到期元数据（宿主写入，网关只读）。
type creditBlock struct {
	// SoonestExpireAt 最近到期时刻。上游混用秒/毫秒，此处按量级归一。
	SoonestExpireAt int64 `json:"soonestExpireAt"`
}

// normalizeEpoch 把秒/毫秒 epoch 统一成秒（上游混用两种精度）。
func normalizeEpoch(n int64) int64 {
	if n <= 0 {
		return 0
	}
	if n > 1_000_000_000_000 {
		return n / 1000
	}
	return n
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename），保持嵌套形（插件可读）格式。
// 全程持 a.mu：防止与 RefreshToken 修改 token 字段并发，杜绝写回半更新。
// 防御：accessToken 为空时拒绝写回，避免误用空凭证覆盖有效文件。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("save refused: empty accessToken (uid=%s)", a.UID)
	}
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
	}
	// 积分到期元数据必须原样保留：它由宿主（workbuddy-switch）或签到任务写入，
	// 而 token 刷新会重写整个文件。若这里丢掉，一次保活就会抹掉选号依据
	// （表现为分层均衡静默退化成原来的三因子随机）。
	if a.SoonestExpireAt > 0 {
		doc["credit"] = map[string]any{"soonestExpireAt": a.SoonestExpireAt}
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.FilePath)
}

// LoadDir 扫描并解析 dir 下 workbuddy*.json；解析失败的文件静默跳过（启动日志由调用方统计）。
func LoadDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "workbuddy*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f
		out = append(out, a)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 区域（realm）判定
//
// 账号分两个区域：国服（cn，copilot.tencent.com / codebuddy.cn）与国际版
//（global，workbuddy.ai）。两者的可用模型、活动、端点都不同，
// 因此「把请求发给哪个区域的账号」是一个需要显式表达的约束。
//
// 判定依据是登录域名后缀，与 upstream 侧的口径一致。
// 放在 auth 包是因为 pool 需要它，而 pool 不能 import upstream（会循环依赖）。
// ---------------------------------------------------------------------------

// Realm 常量。
const (
	RealmCN     = "cn"
	RealmGlobal = "global"
)

// Realm 返回账号所属区域（"cn" / "global"）。
//
// 为什么保留这个字符串版（上游已有类型化的 Region）：模型名前缀协议
//（`cn:glm-5.2` / `global:...`）与前端展示用的都是字符串，growtask 的
// AccountResult.Realm 要直接序列化给界面。两者是**同一判定的两种表示**，
// 都委托给 IsIntl()，不存在两套独立逻辑（改判定只需改 IsIntl 一处）。
func (a *Auth) Realm() string {
	if a.IsIntl() {
		return RealmGlobal
	}
	return RealmCN
}
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/strs"
)

// GDriveApiTokenEnvName 覆盖 gdrive.api-token 的环境变量名
//
// 环境变量非空时优先生效, 便于容器部署不往配置文件里写明文令牌。
const GDriveApiTokenEnvName = "GDRIVE_API_TOKEN"

// GDrive GD 管理面板直链配置
//
// 启用后, strm 内容是 Google Drive 挂载点下的本地路径时, 不再回源给 Emby 读取,
// 而是去掉挂载前缀得到团队盘内的逻辑路径, 交给 GD 管理面板换取直链与请求头,
// 再由本项目把字节流代理给客户端。
//
// 任何环节失败都会回退到回源处理, 因此可以安全地灰度启用。
type GDrive struct {
	// Enable 总开关, 关闭时行为与未部署本功能完全一致
	Enable bool `yaml:"enable"`
	// ApiBase 面板地址, 如 https://gd.bjyt.de
	//
	// 归一化时去掉结尾的 '/', 因此 /api/dl 的拼接不会出现双斜杠。
	ApiBase string `yaml:"api-base"`
	// ApiToken 面板的直链服务 Token, 属敏感凭据, 不得出现在任何日志中
	//
	// 注意与缓存服务 Token 是两枚, 用错会得到 401。
	ApiToken string `yaml:"api-token"`
	// MountPrefix strm 内容中指向 Google Drive 挂载点的前缀
	//
	// strm 内容是 rclone 挂载点下的本地路径(如 /home/googleDrive/影视库/...),
	// 命中该前缀后去掉它, 剩余部分即团队盘内的路径 —— 该前缀是为 Emby 读取
	// 元数据而存在的, 团队盘里并没有这一层。
	//
	// 留空表示不按挂载路径触发本功能(行为与未部署本功能一致)。
	MountPrefix string `yaml:"mount-prefix"`
}

// Init 配置初始化
func (g *GDrive) Init() error {
	// 0 统一去除首尾空白, 避免从 yaml 复制粘贴时带入不可见字符
	g.ApiBase = strings.TrimSpace(g.ApiBase)
	g.ApiToken = strings.TrimSpace(g.ApiToken)
	g.MountPrefix = strings.TrimSpace(g.MountPrefix)

	// 1 面板地址: 只要配置了就提前校验, 与 enable 无关
	//
	// 放在启用校验之前, 是为了让"配错了但还没启用"也能在启动阶段就暴露出来。
	normalized, err := normalizeGDriveApiBase(g.ApiBase)
	if err != nil {
		return err
	}
	g.ApiBase = normalized

	// 2 令牌的环境变量覆盖
	//
	// 非空才覆盖: 留空表示"没有设置", 而不是"把配置值清空"。
	if envToken := strings.TrimSpace(os.Getenv(GDriveApiTokenEnvName)); envToken != "" {
		g.ApiToken = envToken
	}

	// 3 挂载前缀: 只要配置了就提前校验格式
	//
	// 留空是允许的(且与 enable 无关): 它表示"不按挂载路径触发本功能",
	// 行为与未部署本功能完全一致, 因此不能因为留空而拒绝启动。
	if g.MountPrefix != "" {
		if err := validateGDriveMountPrefix(g.MountPrefix); err != nil {
			return err
		}
	}

	// 4 未启用时不再校验凭据, 行为与未部署本功能完全一致
	//
	// 校验错误消息里只提字段名, 绝不回显凭据值。
	if !g.Enable {
		return nil
	}

	if strs.AnyEmpty(g.ApiBase) {
		return errors.New("gdrive.api-base 配置不能为空")
	}
	if strs.AnyEmpty(g.ApiToken) {
		return errors.New("gdrive.api-token 配置不能为空")
	}

	return nil
}

// normalizeGDriveApiBase 归一化并校验面板地址
//
// 归一化: 去首尾空白 + 去结尾的 '/'。
// 校验: 必须是 http/https 绝对地址且带主机名; 不接受 userinfo、查询参数与片段 ——
// 接口路径固定拼 `{base}/api/dl`, 带上后两者只会拼出无意义的地址;
// 而 userinfo 里的口令会被 net/http 原样带进请求, 并在请求失败时经
// "请求面板直链接口失败: %w" 进日志。
//
// 报错时【不回显配置值】: 这几类错法本身就可能把口令写进被拒绝的那一段
// (查询串或 userinfo), 回显等于把它打进 stdout 与 journal。
//
// 传入空字符串返回空字符串与 nil: "未配置"不是错误, 是否必填由启用状态决定。
func normalizeGDriveApiBase(raw string) (string, error) {
	base := strings.TrimSpace(raw)
	if base == "" {
		return "", nil
	}

	u, err := url.Parse(base)
	if err != nil {
		return "", errors.New("gdrive.api-base 配置错误: 不是合法的地址")
	}

	// scheme 不是敏感信息, 单独回显以便定位 "少写了 https://" 这类笔误
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("gdrive.api-base 配置错误: 只支持 http/https, 当前 scheme: %s", scheme)
	}
	if u.Hostname() == "" {
		return "", errors.New("gdrive.api-base 配置错误: 缺少主机名")
	}
	if u.User != nil {
		return "", errors.New("gdrive.api-base 配置错误: 不能包含用户名或密码")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("gdrive.api-base 配置错误: 不能包含查询参数或片段")
	}

	return strings.TrimRight(base, "/"), nil
}

// validateGDriveMountPrefix 校验挂载前缀的格式
//
// 必须以 '/' 开头: 否则相对路径也会被当成前缀命中, 结果不可预期。
// 不能以 '/' 结尾: 否则去掉前缀后会拼出以 '//' 开头的路径。
func validateGDriveMountPrefix(prefix string) error {
	if !strings.HasPrefix(prefix, "/") {
		return fmt.Errorf("gdrive.mount-prefix 配置错误: 必须以 / 开头, 当前: %s", prefix)
	}
	if strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("gdrive.mount-prefix 配置错误: 不能以 / 结尾, 当前: %s", prefix)
	}
	return nil
}

// IsEnabled Google Drive 直链是否启用
func (g *GDrive) IsEnabled() bool {
	if g == nil {
		return false
	}
	return g.Enable
}

package agentnet

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 本文件用沙盒方式（GD_AGENT_INSTALL_ROOT）真跑一遍安装脚本：
// 幂等/升级路径的 `--public-url` 只锚定替换 config.env 的 PUBLIC_BASE_URL 行，
// 失败不改文件。这些行为都在 shell 里，只有真执行才能钉住。

// installScriptPath 是脚本相对路径（go test 的工作目录就是本包目录）。
const installScriptPath = "installshell/agent-install.sh"

// sandboxConfigBody 是一份 enroll 写出的典型配置（含空 PUBLIC_BASE_URL 行）。
const sandboxConfigBody = "MASTER_URL=http://master.example:8000\n" +
	"AGENT_ID=agent-uuid\n" +
	"AGENT_SECRET=" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + "\n" +
	"SIGN_KEY=" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" + "\n" +
	"LISTEN_PORT=8790\n" +
	"# 注释里的 PUBLIC_BASE_URL 不该被锚定替换\n" +
	"PUBLIC_BASE_URL=\n" +
	"MAX_CONCURRENT=32\n"

// installSandbox 是一次沙盒安装所需的目录
type installSandbox struct {
	installRoot string // GD_AGENT_INSTALL_ROOT
	configPath  string
	downloadDir string
}

// run 在沙盒里执行一次安装脚本；返回合并输出与错误（失败本身由用例断言）。
func (sb installSandbox) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmdArgs := append([]string{installScriptPath, "--download-base", sb.downloadDir}, args...)
	cmd := exec.Command("bash", cmdArgs...)
	cmd.Env = append(os.Environ(), "GD_AGENT_INSTALL_ROOT="+sb.installRoot)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// readConfig 读取沙盒里的 config.env
func (sb installSandbox) readConfig(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(sb.configPath)
	if err != nil {
		t.Fatalf("读取 config.env 失败：%v", err)
	}
	return string(raw)
}

// setupInstallSandbox 准备沙盒：假下载源 + 已存在的 config.env（模拟升级路径）
func setupInstallSandbox(t *testing.T, configBody string) installSandbox {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("安装脚本只面向 Linux（uname -m / install / sha256sum）")
	}
	for _, tool := range []string{"bash", "sha256sum", "mktemp", "sed"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("环境缺少 %s：%v", tool, err)
		}
	}

	dir := t.TempDir()
	sb := installSandbox{
		installRoot: filepath.Join(dir, "root"),
		downloadDir: filepath.Join(dir, "download"),
	}
	sb.configPath = filepath.Join(sb.installRoot, "etc", "gd-agent", "config.env")
	if err := os.MkdirAll(filepath.Dir(sb.configPath), 0o755); err != nil {
		t.Fatalf("创建配置目录失败：%v", err)
	}
	if err := os.WriteFile(sb.configPath, []byte(configBody), 0o600); err != nil {
		t.Fatalf("准备 config.env 失败：%v", err)
	}
	writeSandboxDownloadSource(t, sb.downloadDir)
	return sb
}

// writeSandboxDownloadSource 造一个本地下载源：假 gd-agent 二进制 + 匹配的 checksums.txt
//
// 两个架构的资产都放一份，免得用例依赖宿主机的 uname -m。脚本对该二进制的唯一
// 依赖是 `gd-agent version` 能被调用（沙盒路径不 enroll）。
func writeSandboxDownloadSource(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("创建下载源目录失败：%v", err)
	}
	fake := []byte("#!/usr/bin/env bash\necho \"gd-agent sandbox\"\n")
	sum := sha256.Sum256(fake)
	var checksums strings.Builder
	for _, name := range []string{"gd-agent-linux-amd64", "gd-agent-linux-arm64"} {
		if err := os.WriteFile(filepath.Join(dir, name), fake, 0o755); err != nil {
			t.Fatalf("写入假资产 %s 失败：%v", name, err)
		}
		fmt.Fprintf(&checksums, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	}
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(checksums.String()), 0o644); err != nil {
		t.Fatalf("写入 checksums.txt 失败：%v", err)
	}
}

// TestInstallScript_PublicURLUpdateIsAnchoredAndIdempotent
//
// 升级路径给 --public-url：只改 PUBLIC_BASE_URL 那一行（注释里出现的同名字样
// 不受影响），尾斜杠归一化，权限保持 0600；重复给出同一地址内容不变（幂等）。
func TestInstallScript_PublicURLUpdateIsAnchoredAndIdempotent(t *testing.T) {
	sb := setupInstallSandbox(t, sandboxConfigBody)

	const v6URL = "http://[2408:8207:1234::5]:8790"
	// 尾斜杠应被归一化掉（enroll 也这么处理）
	out, err := sb.run(t, "--public-url", v6URL+"/")
	if err != nil {
		t.Fatalf("脚本执行失败：%v\n%s", err, out)
	}
	if !strings.Contains(out, "PUBLIC_BASE_URL="+v6URL) {
		t.Errorf("脚本应打印更新后的地址，实际输出：\n%s", out)
	}

	got := sb.readConfig(t)
	if n := strings.Count(got, "PUBLIC_BASE_URL="); n != 1 {
		t.Fatalf("锚定替换后 PUBLIC_BASE_URL 应恰好一行，实际 %d 行：\n%s", n, got)
	}
	if !strings.Contains(got, "PUBLIC_BASE_URL="+v6URL+"\n") {
		t.Errorf("PUBLIC_BASE_URL 行未被替换为 v6 地址：\n%s", got)
	}
	// 其余行（含注释）原样保留
	for _, want := range []string{
		"MASTER_URL=http://master.example:8000\n",
		"AGENT_ID=agent-uuid\n",
		"MAX_CONCURRENT=32\n",
		"# 注释里的 PUBLIC_BASE_URL 不该被锚定替换\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config.env 里应保留 %q：\n%s", want, got)
		}
	}
	// 属主/权限：脚本末尾的 chown+chmod 应覆盖到刚替换过的新文件
	info, err := os.Stat(sb.configPath)
	if err != nil {
		t.Fatalf("Stat 失败：%v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config.env 权限应保持 0600，实际 %04o", perm)
	}

	// 幂等：再跑一次同一地址，内容不变
	if out, err := sb.run(t, "--public-url", v6URL); err != nil {
		t.Fatalf("第二次执行失败：%v\n%s", err, out)
	}
	if again := sb.readConfig(t); again != got {
		t.Errorf("重复执行后内容应不变：\n第一次 %q\n第二次 %q", got, again)
	}
}

// TestInstallScript_PublicURLUpdateFailureKeepsFile
//
// 失败必须**不改动文件**：锚点行缺失（手工改坏了配置）与非法取值（换行注入）
// 两种场景都只能在原文件上留下零改动。
func TestInstallScript_PublicURLUpdateFailureKeepsFile(t *testing.T) {
	t.Run("配置里缺少 PUBLIC_BASE_URL 行", func(t *testing.T) {
		sb := setupInstallSandbox(t, "MASTER_URL=http://m\nAGENT_ID=a\nAGENT_SECRET=s\nSIGN_KEY=k\n")
		before := sb.readConfig(t)

		out, err := sb.run(t, "--public-url", "http://[::1]:8790")
		if err == nil {
			t.Fatalf("缺少锚点行时脚本应失败，实际输出：\n%s", out)
		}
		if !strings.Contains(out, "PUBLIC_BASE_URL") {
			t.Errorf("失败信息应点名 PUBLIC_BASE_URL 行，实际：\n%s", out)
		}
		if after := sb.readConfig(t); after != before {
			t.Errorf("失败后文件不应被改动：\n改动前 %q\n改动后 %q", before, after)
		}
	})

	t.Run("值里带换行", func(t *testing.T) {
		sb := setupInstallSandbox(t, sandboxConfigBody)
		before := sb.readConfig(t)

		out, err := sb.run(t, "--public-url", "http://[::1]:8790\nEVIL=1")
		if err == nil {
			t.Fatalf("非法取值脚本应失败，实际输出：\n%s", out)
		}
		if after := sb.readConfig(t); after != before {
			t.Errorf("失败后文件不应被改动：\n改动前 %q\n改动后 %q", before, after)
		}
	})

	t.Run("不是 http(s) 地址", func(t *testing.T) {
		sb := setupInstallSandbox(t, sandboxConfigBody)
		before := sb.readConfig(t)

		out, err := sb.run(t, "--public-url", "[::1]:8790")
		if err == nil {
			t.Fatalf("非 http(s) 取值脚本应失败，实际输出：\n%s", out)
		}
		if after := sb.readConfig(t); after != before {
			t.Errorf("失败后文件不应被改动：\n改动前 %q\n改动后 %q", before, after)
		}
	})
}

// TestInstallScript_PublicURLRejectsMasterInvalidShapes
//
// 预检必须拦住 master 的 parsePublicBaseURL 一定会拒绝的形态：这些值一旦写进
// config.env，节点每个心跳都会被 master 400 拒掉，离线窗口过后从调度池掉出去。
// 每个用例都必须失败，且失败后文件逐字节不变。
//
// 裸 IPv6 是唯一"master 反而会放行"的形态（url.Parse 对未加方括号的字面量过于
// 宽松），但拼出来的客户端地址浏览器解析不了，脚本比 master 更严、直接拦下。
func TestInstallScript_PublicURLRejectsMasterInvalidShapes(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"带用户名密码", "http://user:pass@node.example.com:8790"},
		{"带查询参数", "http://node.example.com?a=1"},
		{"带片段", "http://node.example.com#frag"},
		{"IPv6 方括号未闭合", "http://[2001:db8::1"},
		{"IPv6 方括号后不是端口", "http://[::1]8790"},
		{"IPv6 空方括号", "http://[]:8790"},
		{"裸 IPv6 字面量（缺方括号）", "http://2001:db8::1:8790"},
		{"裸 IPv6 无端口", "http://2001:db8::1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := setupInstallSandbox(t, sandboxConfigBody)
			before := sb.readConfig(t)

			out, err := sb.run(t, "--public-url", tc.value)
			if err == nil {
				t.Fatalf("该取值 master 会拒绝，脚本预检应拦下，实际输出：\n%s", out)
			}
			if !strings.Contains(out, "--public-url") {
				t.Errorf("失败信息应点名 --public-url，实际：\n%s", out)
			}
			if after := sb.readConfig(t); after != before {
				t.Errorf("失败后文件不应被改动：\n改动前 %q\n改动后 %q", before, after)
			}
		})
	}
}

// TestInstallScript_PublicURLSedMetacharactersAreLiteral
//
// update_public_url 用 sed 锚定替换，取值里的 sed 元字符（| & \ 与引号）必须
// 在 config.env 里逐字节原样落盘——不能被 sed 解释掉。这些取值全都能过 master
// 校验（host 之外的路径字符不受限），写错就会静默毁掉这一行。
func TestInstallScript_PublicURLSedMetacharactersAreLiteral(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"竖线", `http://node.example.com/a|b`},
		{"与号", `http://node.example.com/a&b`},
		{"反斜杠", `http://node.example.com/a\b`},
		{"结尾反斜杠", `http://node.example.com/a\`},
		{"单双引号", `http://node.example.com/a'b"c`},
		{"混合", `http://node.example.com/a|b&c\d'e"f`},
		// 命令替换的写法必须原样落盘（值只经过变量展开与 sed 替换，不会被 shell 求值）
		{"命令替换 $()", "http://node.example.com/a$(id)b"},
		{"命令替换反引号", "http://node.example.com/a`id`b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := setupInstallSandbox(t, sandboxConfigBody)
			if out, err := sb.run(t, "--public-url", tc.value); err != nil {
				t.Fatalf("脚本执行失败：%v\n%s", err, out)
			}
			// 除了 PUBLIC_BASE_URL 行，其余内容必须逐字节不变
			want := strings.Replace(sandboxConfigBody, "\nPUBLIC_BASE_URL=\n", "\nPUBLIC_BASE_URL="+tc.value+"\n", 1)
			if got := sb.readConfig(t); got != want {
				t.Errorf("config.env 与期望不符：\n实际 %q\n期望 %q", got, want)
			}
		})
	}
}

// TestInstallScript_IdempotentWithoutPublicURLKeepsConfigUntouched
//
// 幂等路径没给 --public-url 时必须对 config.env 零触碰：内容与文件本体都不能变。
// 文件本体用 os.SameFile 判定——临时文件 + rename 的写法会换掉文件本体，
// 这条断言就是对"没有把替换路径误跑一遍"的独立证据。
func TestInstallScript_IdempotentWithoutPublicURLKeepsConfigUntouched(t *testing.T) {
	sb := setupInstallSandbox(t, sandboxConfigBody)
	before := sb.readConfig(t)
	beforeInfo, err := os.Stat(sb.configPath)
	if err != nil {
		t.Fatalf("Stat 失败：%v", err)
	}

	out, err := sb.run(t)
	if err != nil {
		t.Fatalf("脚本执行失败：%v\n%s", err, out)
	}
	if !strings.Contains(out, "不重新注册") {
		t.Errorf("应走幂等路径（不重新注册），实际输出：\n%s", out)
	}
	if after := sb.readConfig(t); after != before {
		t.Errorf("未给 --public-url 时 config.env 不应变化：\n改动前 %q\n改动后 %q", before, after)
	}
	afterInfo, err := os.Stat(sb.configPath)
	if err != nil {
		t.Fatalf("Stat 失败：%v", err)
	}
	if !os.SameFile(beforeInfo, afterInfo) {
		t.Error("未给 --public-url 时不应重写 config.env（文件本体已被更换）")
	}
}

// TestInstallScript_RoleFlagAcceptedOnUpgradeWithNotice
//
// --role 在升级路径上必须被接受（解析/校验通过）但**不改变** config.env——角色只在
// 首次注册（enroll）时写进注册请求；升级路径只打印提示。非法取值必须在预检拦下，
// 且失败后文件逐字节不变（与 --public-url 同一失败契约）。
func TestInstallScript_RoleFlagAcceptedOnUpgradeWithNotice(t *testing.T) {
	sb := setupInstallSandbox(t, sandboxConfigBody)
	before := sb.readConfig(t)

	out, err := sb.run(t, "--role", "hub")
	if err != nil {
		t.Fatalf("升级路径给 --role hub 应被接受，实际失败：%v\n%s", err, out)
	}
	if !strings.Contains(out, "--role 仅在首次注册") {
		t.Errorf("应提示角色只在首次注册生效，实际输出：\n%s", out)
	}
	if after := sb.readConfig(t); after != before {
		t.Errorf("升级路径 --role 不应改动 config.env：\n改动前 %q\n改动后 %q", before, after)
	}

	t.Run("非法取值", func(t *testing.T) {
		sb := setupInstallSandbox(t, sandboxConfigBody)
		before := sb.readConfig(t)

		out, err := sb.run(t, "--role", "center")
		if err == nil {
			t.Fatalf("非法 --role 应被预检拦下，实际输出：\n%s", out)
		}
		if !strings.Contains(out, "--role") {
			t.Errorf("失败信息应点名 --role，实际：\n%s", out)
		}
		if after := sb.readConfig(t); after != before {
			t.Errorf("失败后文件不应被改动：\n改动前 %q\n改动后 %q", before, after)
		}
	})
}

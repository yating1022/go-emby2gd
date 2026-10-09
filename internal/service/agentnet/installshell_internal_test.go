package agentnet

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestInstallScript_GetServesEmbeddedShell(t *testing.T) {
	setupStateDir(t)
	setupFullConfig(t, true, "")
	simulateRestart()

	recorder := doRequest(t, newTestEngine(), http.MethodGet, "/install.sh", "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("HTTP = %d, want 200, body=%s", recorder.Code, recorder.Body.String())
	}

	// 响应体必须与嵌入的脚本逐字节一致(不能有模板替换/包装)
	if recorder.Body.String() != string(installScript) {
		t.Error("响应体与嵌入的脚本内容不一致")
	}
	if !strings.HasPrefix(recorder.Body.String(), "#!/usr/bin/env bash") {
		t.Error("脚本首行应是 bash shebang")
	}
	// 脚本必须接受安装命令里传的两个参数, 否则一键命令装不上
	for _, flag := range []string{"--master", "--token"} {
		if !strings.Contains(recorder.Body.String(), flag) {
			t.Errorf("脚本未处理参数 %s", flag)
		}
	}
	if got := recorder.Header().Get("Content-Type"); got != installScriptContentType {
		t.Errorf("Content-Type = %q, want %q", got, installScriptContentType)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	if got := recorder.Header().Get("Content-Length"); got != strconv.Itoa(len(installScript)) {
		t.Errorf("Content-Length = %q, want %d", got, len(installScript))
	}
}

func TestInstallScript_HeadHasSameHeadersWithoutBody(t *testing.T) {
	setupStateDir(t)
	setupFullConfig(t, true, "")
	simulateRestart()

	engine := newTestEngine()
	get := doRequest(t, engine, http.MethodGet, "/install.sh", "", nil)
	head := doRequest(t, engine, http.MethodHead, "/install.sh", "", nil)

	if head.Code != http.StatusOK {
		t.Fatalf("HEAD HTTP = %d, want 200", head.Code)
	}
	if head.Body.Len() != 0 {
		t.Errorf("HEAD 不应返回 body, 实际 %d 字节", head.Body.Len())
	}
	for _, key := range []string{"Content-Type", "Content-Length", "Cache-Control"} {
		if head.Header().Get(key) != get.Header().Get(key) {
			t.Errorf("%s 头不一致: HEAD=%q GET=%q", key, head.Header().Get(key), get.Header().Get(key))
		}
	}
}

func TestInstallScript_MethodAndStateRejections(t *testing.T) {
	t.Run("非 GET/HEAD", func(t *testing.T) {
		setupStateDir(t)
		setupFullConfig(t, true, "")
		simulateRestart()

		recorder := doRequest(t, newTestEngine(), http.MethodPost, "/install.sh", "", nil)
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("HTTP = %d, want 405", recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), "只支持") {
			t.Errorf("应给出中文提示, 实际: %s", recorder.Body.String())
		}
	})

	t.Run("未启用", func(t *testing.T) {
		setupStateDir(t)
		setupFullConfig(t, false, "")
		simulateRestart()

		recorder := doRequest(t, newTestEngine(), http.MethodGet, "/install.sh", "", nil)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("HTTP = %d, want 403", recorder.Code)
		}
		if code := errorCodeOf(t, decodeMap(t, recorder.Body.Bytes())); code != codeDisabled {
			t.Errorf("错误码 = %q, want %q", code, codeDisabled)
		}
	})
}

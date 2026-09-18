# GD 管理面板 · 直链接口对接文档

本文档描述如何调用 **GD 管理面板**（下文称「本服务」）的直链接口，
按路径取得 Google Drive 文件的下载地址。

> 本服务部署地址以实际为准，下文示例统一用 `https://gd.bjyt.de`，
> 请按你的部署替换。

---

## 1. 这个接口做什么

给它一个 **Google Drive 内的路径**，它返回：

1. **Google 的下载地址**（`url`）
2. **下载该地址所需的请求头**（`headers`，内含 `Authorization`）

然后由**你去请求那个 `url`**，带上它给的 `headers`，拿到文件字节。

```
       路径                        url + headers            文件字节
你  ──────────▶  GD 管理面板   ──────────────────▶  Google Drive
                     │                                     ▲
                     └──────── 不转发任何字节 ──────────────┘
```

### ⚠️ 最重要的一点：文件字节流**不经过本服务**

本服务只负责「换地址」，不代理下载。这意味着：

- 大文件不会占用本服务的带宽与内存，它的响应非常快（毫秒级）
- **但下载失败时，本服务看不到** —— 它已经成功返回了 200。
  出错的是你与 Google 之间的那一段，排查时别往本服务找

---

## 2. 先拿到 Token

本服务有两枚独立 Token：**直链服务 Token** 与 **缓存服务 Token**。
**两枚不通用** —— 用错了会得到 401。

到管理界面 **「直链服务」** 页，可以看到调用 Token（默认掩码显示）。
若需要明文，在 **「系统管理 → 服务配置」** 里点该 Token 的「重新生成」，
**新令牌会在弹窗里显示且只显示这一次**（旧令牌立即失效）。

请求时放在请求头：

```
Authorization: Bearer <直链服务 Token>
```

---

## 3. 接口契约

### 3.1 请求

```
GET {base}/api/dl?path=<URL 编码后的 Drive 路径>
Authorization: Bearer <直链服务 Token>
```

| 参数 | 位置 | 必填 | 说明 |
|---|---|---|---|
| `path` | query | 是 | Drive 内的完整逻辑路径，**必须做 URL 编码** |
| `Authorization` | header | 是 | `Bearer ` + 直链服务 Token（注意 `Bearer` 后有空格） |

**路径写法**：就是你在 Drive 里看到的层级，用 `/` 连接，例如：

```
/影视库/最新电影/72小时 (2026)/72小时 (2026).mkv
```

路径含中文、空格、括号，**必须 URL 编码**：

```
%2F%E5%BD%B1%E8%A7%86%E5%BA%93%2F%E6%9C%80%E6%96%B0%E7%94%B5%E5%BD%B1%2F...
```

> 用 `url.QueryEscape`（Go）或 `encodeURIComponent`（JS）即可。
> **不要自己拼字符串**，中文与括号很容易编错。

### 3.2 成功响应（`200`）

```json
{
  "ok": true,
  "data": {
    "url": "https://www.googleapis.com/drive/v3/files/1aBcD...?alt=media&supportsAllDrives=true",
    "headers": { "Authorization": "Bearer ya29.a0Af..." },
    "expires_at": "2026-09-18T10:35:47Z",
    "scope_note": "该 Authorization 头是【账号级】Google 凭据，不是「只读本次这一个文件」的权限：……",
    "file": {
      "id": "1aBcD...",
      "name": "72小时 (2026).mkv",
      "path": "/影视库/最新电影/72小时 (2026)/72小时 (2026).mkv",
      "size": 8589934592,
      "mime_type": "video/x-matroska"
    }
  }
}
```

| 字段 | 说明 |
|---|---|
| `url` | **Google 的下载地址**。已带 `alt=media` 与 `supportsAllDrives=true`，直接请求即可 |
| `headers` | **请求 `url` 时必须带上的头**。至少含 `Authorization` |
| `expires_at` | `headers` 里那个令牌的过期时刻（UTC，带 `Z`）。**约 1 小时** |
| `scope_note` | 中文说明该令牌的**真实权限范围**，见 §5 |
| `file` | 文件元信息：`id` / `name` / `path` / `size`（字节）/ `mime_type` |

### 3.3 失败响应

统一形状：

```json
{ "ok": false, "error": { "code": "PATH_NOT_IN_CACHE", "message": "中文错误信息" } }
```

| 状态 | `code` | 含义 | 你该怎么做 |
|---|---|---|---|
| `401` | `INVALID_TOKEN` | Token 缺失 / 错误 / 已轮换 | 换个 Token；**不要重试** |
| `404` | `PATH_NOT_IN_CACHE` | **路径尚未缓存** | 先缓存它（见 §4）；这不是"文件不存在" |
| `404` | `PATH_NOT_FOUND` | 路径在 **Drive 里确实不存在** | 路径写错了，或文件被移动/删除 |
| `400` | `PATH_IS_DIRECTORY` | 路径指向的是**目录** | 直链只对文件有效；改成具体文件路径 |
| `400` | `PATH_NOT_DOWNLOADABLE` | 指向 **Google 原生文档**（Docs/Sheets/Slides） | 这类文件没有字节流，须用 Drive 的导出接口 |
| `422` | `VALIDATION_ERROR` | 参数不合法（如缺 `path`） | 检查请求 |
| `502` | `GD_FORBIDDEN` / `GD_QUOTA_EXCEEDED` / `GD_UPSTREAM_ERROR` | 上游 Google 报错 | 看 `message` 的中文说明区分处理 |

> **`PATH_NOT_IN_CACHE` 与 `PATH_NOT_FOUND` 必须区别对待。**
> 前者说明"这个文件本服务还没记录，但 Drive 里很可能有" ——
> 先去缓存，再重试。把它当成"文件不存在"会让你白找一整轮。
>
> 本服务的 `message` 是**专门写过**的，直接把它打出来或展示给用户，
> 不要自己另编一套文案。

---

## 4. 用之前：路径必须先被缓存

**本服务的立论就是「不按路径逐层查 Drive」**（那样极慢），
所以 `/api/dl` 只查本地缓存表，**缓存里没有就直接 404**。

缓存有两条路：

### 4.1 上传后立刻登记（推荐，增量）

你的上传程序处理完**一个文件**后，把**该文件路径**报给缓存接口：

```
POST {base}/api/cache
Authorization: Bearer <缓存服务 Token>       ← 注意是另一枚 Token
Content-Type: application/json

{ "path": "/影视库/最新电影/72小时 (2026)/72小时 (2026).mkv" }
```

- **只接受文件路径**。传目录会返回 `400 PATH_IS_DIRECTORY`
- 幂等：同一路径重复调用不会产生重复记录
- 覆盖上传（同路径新文件 ID）会**更新**已有记录，不会新增

### 4.2 扫描任务（兜底，按目录）

到管理界面 **「缓存文件」** 页管理扫描任务：**一条任务 = 一个目录**，
每行可分别配置**定时全量**与**定时增量**（都可以留空），
也可以在该行点**手动全量** / **手动增量**。

> ⚠️ **增量的两条边界**（界面上也有说明）：
> - **增量发现不了删除**：它按 `modifiedTime` 查，只取新增与修改。
>   文件在 Drive 里被删掉后，缓存里的旧记录要等**全量**才清理。
> - **增量只查该目录的直接子项，不进子目录**（Drive 的查询没有递归子树条件）。
>
> 建议：**结构化嵌套的目录配定时全量；平铺的目录才适合定时增量。**

---

## 5. ⚠️ `headers` 里的令牌是**账号级**的

响应里的 `Authorization: Bearer …` 是 Google 签发的**账号级**凭据，
**不是「只读这一个文件」的权限**。在它过期前（约 1 小时），
持有它的任何一方都可以访问**整个共享盘**。

这是部署方知悉并接受的取舍（本服务对 Drive 只读，调用方只有自己），
`scope_note` 字段把这件事写在响应里就是为了提醒你。

**实践上该怎么做**：

- 这个头**只用于你与 Google 之间**，不要转发给终端用户、不要写进日志、
  不要放进任何可能被他们看到的地方
- 不要缓存它超过 `expires_at`
- 每次取直链都重新调 `/api/dl`（本服务有缓存，很快），不要自己长期保存

---

## 6. 怎么用：Go 示例

以下代码可直接放进 `go-emby2openlist` 使用。

### 6.1 取直链

```go
package gdlink

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

type Client struct {
	baseURL string       // 例如 https://gd.bjyt.de
	token   string       // 直链服务 Token
	http    *http.Client // 见 6.2：必须自定义 CheckRedirect
}

type fileInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	MimeType string `json:"mime_type"`
}

// DirectLink 是 /api/dl 成功响应里的 data 对象。
type DirectLink struct {
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers"`
	ExpiresAt time.Time         `json:"expires_at"`
	ScopeNote string            `json:"scope_note"`
	File      fileInfo          `json:"file"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// GetDirectLink 按 Drive 路径取直链。path 传**未编码**的原始路径，
// 编码由本函数负责。
func (c *Client) GetDirectLink(ctx context.Context, path string) (*DirectLink, error) {
	endpoint := c.baseURL + "/api/dl?path=" + url.QueryEscape(path)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 响应很小，1MB 足够
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		var envelope struct {
			Error apiError `json:"error"`
		}
		if json.Unmarshal(body, &envelope) == nil && envelope.Error.Code != "" {
			// 直接沿用本服务的中文文案，不要另编一套
			return nil, fmt.Errorf("取直链失败 [%s] %s", envelope.Error.Code, envelope.Error.Message)
		}
		return nil, fmt.Errorf("取直链失败：HTTP %d", resp.StatusCode)
	}

	var envelope struct {
		OK   bool       `json:"ok"`
		Data DirectLink `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("解析响应失败：%w", err)
	}
	return &envelope.Data, nil
}
```

### 6.2 ⚠️ 必须自定义 `http.Client`（否则一定失败）

**Google 的下载地址会 302 跳转到 `*.googleusercontent.com`。**
而 Go 标准库的 `http.Client` 在跳转到**不同主机名**时会**自动丢掉 `Authorization` 头** ——
于是跳到新地址后请求变成匿名，Google 返回 403 或 404。

**这个失败现象极具误导性**：你会以为是 Token 错了或文件没了，
实际只是跳转把凭据吃了。

> **实测确认**（本文档编写时验证过，不是推测）：
>
> | 跳转 | 默认 Client | 自定义 `CheckRedirect` |
> |---|---|---|
> | `127.0.0.1` → `127.0.0.1`（**同主机名**，只换路径） | `AUTH` **保留** | 保留 |
> | `127.0.0.1` → `localhost`（**不同主机名**） | `AUTH` **丢失** | 保留 |
>
> 也就是说**判据是主机名，不是笼统的「跨域」**。
> 有人用同主机换端口/路径试了一下发现"没事"，就以为不需要处理 ——
> 但 `googleapis.com` → `googleusercontent.com` 是**不同主机名**，一定会丢。
>
> 另外 `Range` 这类非敏感头**不会**被丢，所以你可能看到"能拿到内容但 403"，
> 更容易误判成权限问题。

正确做法是**在跳转时把 `headers` 补回去**：

```go
func NewClient(baseURL, token string) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()

	hc := &http.Client{
		Transport: transport,
		// 关键：默认策略会丢弃跨域跳转的 Authorization
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("跳转次数过多")
			}
			// 把原始请求的凭据补到跳转后的请求上
			for key, values := range via[0].Header {
				if req.Header.Get(key) == "" {
					for _, v := range values {
						req.Header.Add(key, v)
					}
				}
			}
			return nil
		},
	}
	return &Client{baseURL: baseURL, token: token, http: hc}
}
```

### 6.3 下载（含媒体播放所需的 Range 支持）

```go
// Fetch 用直链取文件内容。rangeHeader 传 "" 表示取全部，
// 传 "bytes=0-" 等表示分段（播放器拖动进度时要用）。
func (c *Client) Fetch(ctx context.Context, link *DirectLink, rangeHeader string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link.URL, nil)
	if err != nil {
		return nil, err
	}
	// 本服务返回的 headers 一个都不能少（至少是 Authorization）
	for key, value := range link.Headers {
		req.Header.Set(key, value)
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	// 注意：这里【不要】defer resp.Body.Close()，交给调用方读完再关
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		defer resp.Body.Close()
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("下载失败：HTTP %d %s", resp.StatusCode, string(snippet))
	}
	return resp, nil
}
```

### 6.4 完整调用

> 下面是**调用片段**（放在你已有的函数体内），不是完整可编译文件 ——
> `ctx`、`w` 等由你的上下文提供。§6.1–6.3 是完整可编译的包
> （已用 `go build` + `go vet` 验证）。

```go
c := NewClient("https://gd.bjyt.de", os.Getenv("GD_DL_TOKEN"))

link, err := c.GetDirectLink(ctx, "/影视库/最新电影/72小时 (2026)/72小时 (2026).mkv")
if err != nil {
	return err
}

resp, err := c.Fetch(ctx, link, "bytes=0-")
if err != nil {
	return err
}
defer resp.Body.Close()

// resp.Body 可以直接接到播放器 / 转交给 Emby，无需落盘
io.Copy(w, resp.Body)
```

### 6.5 curl 速查

```bash
# 取直链
curl -s -H "Authorization: Bearer $GD_DL_TOKEN" \
  --get --data-urlencode "path=/影视库/最新电影/72小时 (2026)/72小时 (2026).mkv" \
  https://gd.bjyt.de/api/dl | jq

# 用返回的 url + headers 下载（jq 取值）
URL=$(... | jq -r '.data.url')
TOKENHDR=$(... | jq -r '.data.headers.Authorization')
curl -L -H "Authorization: $TOKENHDR" "$URL" -o out.mkv
#   -L 跟随跳转。
#   实测：curl 用 -H 手动设置的头在跳转时【会】保留，所以命令行下不会遇到
#   §6.2 那个坑 —— 但 Go 里会。别因为 curl 能跑通就以为代码也没问题。
```

---

## 7. 对接时的常见问题

| 现象 | 真实原因 |
|---|---|
| `401 INVALID_TOKEN` | Token 写错，或用的是**缓存服务**的 Token（两枚不通用） |
| `404 PATH_NOT_IN_CACHE` | 该文件还没被缓存。先调 `POST /api/cache` 缓存**该文件** |
| `404 PATH_NOT_FOUND` | **Drive 里确实没有**这个路径。检查拼写，或文件已被移动/删除 |
| 直链拿到了，但下载 403 / 404 | 多半是 §6.2 的跳转丢头问题；也可能是令牌已过 `expires_at` |
| 下载只拿到一部分 | 检查是否把 `Range` 头原样转发；服务端返回的 `headers` 一个都不能少 |
| `400 PATH_NOT_DOWNLOADABLE` | 指向 Google Docs/Sheets/Slides，它们没有字节流，需用导出接口 |
| 路径明明对，却 `404 PATH_NOT_IN_CACHE` | 路径必须与 Drive 内**逐字符一致**（含空格、括号、大小写） |

---

## 8. 相关接口速查

| 用途 | 方法与路径 | Token |
|---|---|---|
| 取直链 | `GET /api/dl?path=…` | 直链服务 Token |
| 缓存**单个文件** | `POST /api/cache` `{"path": "…"}` | 缓存服务 Token |
| 列出扫描任务 | `GET /api/admin/cache/tasks` | 管理会话（Cookie） |
| 新增扫描任务 | `POST /api/admin/cache/tasks` `{"dir","full_cron","incremental_cron"}` | 管理会话 |
| 修改 / 删除任务 | `PUT` / `DELETE /api/admin/cache/tasks/{id}` | 管理会话 |
| 手动全量 / 增量 | `POST /api/admin/cache/tasks/{id}/run` `/run-incremental` | 管理会话 |

> 管理类接口走**浏览器会话**（httpOnly Cookie），不适合服务间调用。
> 上传程序只需要「取直链」与「缓存单文件」这两个 Token 接口。

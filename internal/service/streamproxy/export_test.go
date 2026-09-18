package streamproxy

import (
	"context"
	"net/http"
)

// SetGDriveFetchForTest 替换 Google Drive 取流实现, 返回恢复原实现的函数, 仅供测试使用
//
// 该文件只在测试编译期参与构建, 不会进入产物。
// ProxyGDrive 的成功路径需要真实的媒体响应才能走到 relay, 而 gdrive 包的
// Drive 端点地址是包私有的; 这里把取流实现抽成可注入的函数, 让 streamproxy
// 与 emby 的用例都能指向本地假 Drive 服务端, 不再依赖跨包的测试钩子。
func SetGDriveFetchForTest(fn func(ctx context.Context, gdPath, clientRange string) (*http.Response, error)) (restore func()) {
	old := fetchGDriveMedia
	fetchGDriveMedia = fn
	return func() { fetchGDriveMedia = old }
}

// ResetSlotsForTest 以指定上限重置并发信号量, 仅供测试使用
//
// 该文件只在测试编译期参与构建, 不会进入产物。
// 这里不再让测试去改配置对象: 运行期改配置会与 initSlots 的读取形成数据竞争,
// 也会绕过"信号量只初始化一次"的约束。
//
// 实现上先把 once 标记为已执行, 再直接写入信号量,
// 避免随后的 initSlots 从配置对象重新读取上限而覆盖测试注入的值。
//
// 调用约束: 必须在**没有任何请求在途**时调用 (即在该用例发起 Proxy 调用之前)。
// 本函数不做同步, 若在有请求占用或等待槽位时调用, 会替换掉正在被使用的 channel,
// 导致等待者永久挂起、活跃计数被破坏, 并在 -race 下报数据竞争。
// 当前全包无 t.Parallel, 所有 reset 都发生在发请求之前, 因此是安全的;
// 若将来给本包用例加上并行, 必须先给这里补同步。
func ResetSlotsForTest(limit int) {
	slotsOnce.Do(func() {})

	slotLimit = limit
	if limit > 0 {
		streamSlots = make(chan struct{}, limit)
	} else {
		streamSlots = nil
	}
	activeStreams.Store(0)
}

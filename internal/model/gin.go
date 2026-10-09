package model

// Response gin 通用响应结构
type Response struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	// Data 可选的附加数据
	//
	// 不设置时(nil 接口)被 omitempty 省略: 既有不含 data 的响应序列化结果不变。
	// 注意 omitempty 对 any 字段只判接口本身是否为空 —— 一旦设置, 即使值是
	// 空数组 / 空 map 也会照常序列化, 因此需要下发"空列表"的接口可直接放
	// []agentView{} 或 {"agents": []}(typed-nil 切片则输出 null)。
	Data any `json:"data,omitempty"`
}

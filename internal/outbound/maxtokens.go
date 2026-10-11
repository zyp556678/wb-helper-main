package outbound

// 上游按「输入 token + 输出预算」判断是否超出模型窗口：请求不带 max_tokens 时，
// 上游会为输出默认预留 384000，标称 1048576 窗口的模型实际只剩约 664576 可用输入。
// 长会话会在远未到窗口上限时被 400 code=11133（model_param_invalid，报错文案不指明
// 具体参数）拒绝，表现为「聊着聊着就 400」。
//
// 因此仅在客户端未声明输出预算时，补上模型目录声明的输出上限，把被默认预留的额度
// 收回来。客户端显式传入的值一律不覆盖；目录未收录的模型保持原样，不猜测。
//
// 为什么放在 translateMaxCompletionTokens 之后：那一步已把别名归并/删除，
// 此时判「是否已声明」只需认 max_tokens 一个键，不会与别名字段打架。

// ensureCatalogMaxTokens 在客户端未声明输出预算时，按目录上限补 max_tokens。
//
// 返回补入的值（未补入返回 0）。判定：
//   - max_tokens 已存在且非 nil → 客户端已声明，不改（含显式传 0 的情形：
//     0 是「未设置」还是「不要输出」由上游解释，网关不替它决定）；
//   - limit <= 0（目录未收录该模型）→ 不改，不猜测。
func ensureCatalogMaxTokens(obj map[string]any, limit int) int {
	if v, ok := obj["max_tokens"]; ok && v != nil {
		return 0
	}
	if limit <= 0 {
		return 0
	}
	obj["max_tokens"] = int64(limit)
	return limit
}

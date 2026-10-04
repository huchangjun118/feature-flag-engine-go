package featureflag

import "hash/fnv"

// bucketOf 计算主体在指定开关与盐下的稳定分桶，返回值域 [0,100)。
//
// 稳定性保证：
//   - 纯函数：仅依赖 (flagKey, salt, subjectID)，不依赖进程状态、
//     随机源、时间、并发度或遍历顺序，进程重启后结果不变；
//   - 比例上调时，已落入 [0, oldP) 的主体仍落入 [0, newP)，不会被踢出；
//   - 比例下调时按分桶序确定性收缩，任何主体不会被“重新随机”。
func bucketOf(flagKey, salt, subjectID string) int {
	h := fnv.New64a()
	// 显式分隔符避免字段拼接产生歧义。
	h.Write([]byte(flagKey))
	h.Write([]byte{0})
	h.Write([]byte(salt))
	h.Write([]byte{0})
	h.Write([]byte(subjectID))
	// 取高 32 位再取模，弱化 FNV 低位分布对取模的影响。
	return int(uint32(h.Sum64()>>32) % 100)
}

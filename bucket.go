package flagengine

import "hash/fnv"

// BucketCount 是分桶总数，桶值范围固定为 [0, BucketCount-1]。
const BucketCount = 100

// bucketKey 生成稳定的分桶输入：salt|flagKey|subjectID。
//
// 刻意不包含以下量：
//   - 配置版本号 / 提交时间：回滚或热更新到等价定义后桶不变；
//   - 放量百分比 Percentage：比例上调/下调只改变阈值，不改变主体桶位，
//     因而放量是单调的——已进入的主体不会被无故踢出；
//   - 并发度、遍历顺序、随机源：哈希是纯函数。
func bucketKey(salt, flagKey, subjectID string) string {
	return salt + "|" + flagKey + "|" + subjectID
}

// hashBucket 对输入做确定性哈希并映射到 [0,99]。
// 使用 FNV-1a 64 位哈希；同输入在任何进程、任何时刻得到同输出。
func hashBucket(input string) int {
	h := fnv.New64a()
	_, _ = h.Write([]byte(input))
	return int(h.Sum64() % uint64(BucketCount))
}

// computeBucket 计算主体在某开关上的稳定桶值。
// Salt 缺省时使用 Flag.Key（见 config.go 的规范化），保证零配置也稳定。
func computeBucket(flag *Flag, subjectID string) int {
	salt := flag.Salt
	if salt == "" {
		salt = flag.Key
	}
	return hashBucket(bucketKey(salt, flag.Key, subjectID))
}

// inBucket 判断桶值是否落入放量比例：桶值 < percentage 即放量。
// 由于阈值是"前缀区间"，percentage 从 10 升到 30 时只有桶 [10,29] 新增，
// 桶 [0,9] 的主体保持在放量集合内；下调同理只移除高桶主体。
func inBucket(bucket, percentage int) bool {
	if percentage <= 0 {
		return false
	}
	if percentage >= BucketCount {
		return true
	}
	return bucket >= 0 && bucket < percentage
}

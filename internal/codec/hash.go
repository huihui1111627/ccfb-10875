package codec

import "hash/fnv"

// ShardOf 按 user_id 的 FNV-1a 32 位哈希取模，保证同一用户恒定路由到同一分片。
func ShardOf(userID string, shardCount int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(userID))
	return int(h.Sum32() % uint32(shardCount))
}

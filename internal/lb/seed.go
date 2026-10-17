package lb

import (
	"crypto/rand"
	"encoding/binary"
	"time"
)

// randSeed 生成一个随机种子。
// 每请求新建随机源在热路径上开销偏大，但负载均衡本身是毫秒级决策，
// 换来确定性与可测试性：权重分布可通过 PickWith 精确断言。
func randSeed() int64 {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return time.Now().UnixNano()
	}
	return int64(binary.LittleEndian.Uint64(buf[:]))
}

package service

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"time"
)

var seqCounter uint64

// genNo 生成业务单号，如 EX20261005T133001-ab12cd34。
// 时间戳 + 进程内自增序列 + 随机字节，单机内唯一；数据库唯一索引兜底。
func genNo(prefix string) string {
	n := atomic.AddUint64(&seqCounter, 1)
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s%s-%04x%s",
		prefix,
		time.Now().UTC().Format("20060102T150405"),
		n&0xffff,
		hex.EncodeToString(b[:2]),
	)
}

// NewIdempotencyKey 供前端/测试使用的幂等键。
func NewIdempotencyKey() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "idem-" + hex.EncodeToString(b[:])
}

// GenNo 导出版本，供种子/其他包使用。
func GenNo(prefix string) string { return genNo(prefix) }

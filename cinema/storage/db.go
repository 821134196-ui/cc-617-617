package storage

import (
	"fmt"
	"os"
	"path/filepath"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	_ "modernc.org/sqlite" // 注册纯 Go 的 "sqlite" 驱动
)

// Open 打开/创建 SQLite 数据库。
// 采用纯 Go 驱动 modernc.org/sqlite（驱动名 "sqlite"），无需 CGO。
// DSN 中对每个新连接固定：
//   - journal_mode=WAL：读写并发，写事务串行但不阻塞读；
//   - busy_timeout：写锁冲突时等待而不是立刻报 locked；
//   - foreign_keys=ON。
func Open(path string) (*gorm.DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(ON)", path)
	db, err := gorm.Open(sqlite.New(sqlite.Config{
		DriverName: "sqlite",
		DSN:        dsn,
	}), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
		// 让驱动把唯一约束错误翻译成 gorm.ErrDuplicatedKey
		TranslateError: true,
	})
	if err != nil {
		return nil, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	// WAL 模式允许多读单写；写冲突由 busy_timeout 等待 + 唯一索引兜底。
	// 限制连接数，避免高并发压测时产生过多文件锁竞争。
	sqlDB.SetMaxOpenConns(16)
	sqlDB.SetMaxIdleConns(8)
	return db, nil
}

package main

import (
	"fmt"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func openDB(path string) (*gorm.DB, error) {
	// _busy_timeout：写冲突时等待而非立即报 SQLITE_BUSY
	// _journal_mode=WAL：允许读写并发，写之间由库级锁串行
	dsn := fmt.Sprintf("%s?_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=on", path)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(
		&Movie{}, &Showtime{}, &Order{}, &Ticket{}, &Coupon{},
		&SeatLock{}, &Reservation{}, &Payment{},
	); err != nil {
		return nil, err
	}
	// 部分唯一索引：只约束活跃锁（held/sold），released 历史行不占位。
	stmts := []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_seat_active
		 ON seat_locks(showtime_id, seat_code) WHERE status IN ('held','sold')`,
	}
	for _, s := range stmts {
		if err := db.Exec(s).Error; err != nil {
			return nil, err
		}
	}
	if err := seed(db); err != nil {
		return nil, err
	}
	return db, nil
}

// seed 幂等灌入演示数据：影片/场次/会员订单/券。
func seed(db *gorm.DB) error {
	var n int64
	db.Model(&Movie{}).Count(&n)
	if n > 0 {
		return nil
	}
	now := time.Now()
	base := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	movies := []Movie{
		{ID: 1, Title: "星海彼岸", Genre: "科幻"},
		{ID: 2, Title: "午夜来信", Genre: "悬疑"},
	}
	shows := []Showtime{
		{ID: 1, MovieID: 1, Hall: "1号厅", StartAt: base.Add(14 * time.Hour), BasePrice: 4500, Rows: 6, Cols: 8},
		{ID: 2, MovieID: 1, Hall: "1号厅", StartAt: base.Add(19 * time.Hour), BasePrice: 5800, Rows: 6, Cols: 8},
		{ID: 3, MovieID: 2, Hall: "2号厅", StartAt: base.Add(16 * time.Hour + 30*time.Minute), BasePrice: 4000, Rows: 5, Cols: 8},
		{ID: 4, MovieID: 2, Hall: "2号厅", StartAt: base.Add(21 * time.Hour), BasePrice: 5200, Rows: 5, Cols: 8},
	}
	order := Order{ID: 1, OrderNo: "O1001", MemberID: 1, Status: OrderActive}

	// 同行订单：3 张原票，都在场次 1。
	percent90 := int64(90)
	fixed1000 := int64(1000)
	basis1 := DiscountBasis{CouponCode: "CPN-90OFF", Kind: "percent", Value: percent90,
		Description: "9折券（按票面90%收取）"}
	basis2 := DiscountBasis{CouponCode: "CPN-10OFF", Kind: "fixed", Value: fixed1000, MinSpend: 4000,
		Description: "满40减10券"}
	// 折扣函数必须与 discountFor 保持一致：9折 => 4500-450=4050；满40减10 => 4500-1000=3500。
	d1 := int64(450)
	d2 := int64(1000)
	coupon1ID := int64(1)
	coupon2ID := int64(2)
	t1 := Ticket{ID: 1, OrderID: 1, MemberID: 1, ShowtimeID: 1, SeatRow: 3, SeatCol: 4, SeatCode: "C4",
		FacePrice: 4500, DiscountAmount: d1, PaidAmount: 4500 - d1, CouponID: &coupon1ID,
		DiscountBasis: mustJSON(basis1), Status: TicketActive}
	t2 := Ticket{ID: 2, OrderID: 1, MemberID: 1, ShowtimeID: 1, SeatRow: 3, SeatCol: 5, SeatCode: "C5",
		FacePrice: 4500, DiscountAmount: d2, PaidAmount: 4500 - d2, CouponID: &coupon2ID,
		DiscountBasis: mustJSON(basis2), Status: TicketActive}
	t3 := Ticket{ID: 3, OrderID: 1, MemberID: 1, ShowtimeID: 1, SeatRow: 3, SeatCol: 6, SeatCode: "C6",
		FacePrice: 4500, DiscountAmount: 0, PaidAmount: 4500, Status: TicketActive}

	coupons := []Coupon{
		{ID: 1, Code: "CPN-90OFF", MemberID: 1, Name: "9折券", Kind: "percent", Value: percent90,
			Status: CouponUsed, CurrentTicketID: &t1.ID, ExpireAt: now.AddDate(0, 1, 0)},
		{ID: 2, Code: "CPN-10OFF", MemberID: 1, Name: "满40减10券", Kind: "fixed", Value: fixed1000,
			MinSpend: 4000, Status: CouponUsed, CurrentTicketID: &t2.ID, ExpireAt: now.AddDate(0, 1, 0)},
		{ID: 3, Code: "CPN-NEW8", MemberID: 1, Name: "新客8折券", Kind: "percent", Value: 80,
			Status: CouponAvailable, ExpireAt: now.AddDate(0, 1, 0)},
		{ID: 4, Code: "CPN-EXPIRED", MemberID: 1, Name: "过期券", Kind: "fixed", Value: 2000,
			Status: CouponExpired, ExpireAt: now.Add(-24 * time.Hour)},
	}

	locks := []SeatLock{
		{ID: 1, ShowtimeID: 1, SeatCode: "C4", Status: LockSold, TicketID: &t1.ID},
		{ID: 2, ShowtimeID: 1, SeatCode: "C5", Status: LockSold, TicketID: &t2.ID},
		{ID: 3, ShowtimeID: 1, SeatCode: "C6", Status: LockSold, TicketID: &t3.ID},
	}

	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&movies).Error; err != nil {
			return err
		}
		if err := tx.Create(&shows).Error; err != nil {
			return err
		}
		if err := tx.Create(&order).Error; err != nil {
			return err
		}
		if err := tx.Create(&coupons).Error; err != nil {
			return err
		}
		if err := tx.Create([]*Ticket{&t1, &t2, &t3}).Error; err != nil {
			return err
		}
		if err := tx.Create(&locks).Error; err != nil {
			return err
		}
		// 别人的场次 2 已占座位，便于演示抢座冲突
		other := SeatLock{ShowtimeID: 2, SeatCode: "A1", Status: LockSold}
		return tx.Create(&other).Error
	})
}

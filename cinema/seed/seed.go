package seed

import (
	"time"

	"gorm.io/gorm"

	"cinema/models"
	"cinema/service"
)

// Reset 清空并重建演示数据。返回主会员 ID。
func Reset(db *gorm.DB, now time.Time) (memberID uint, err error) {
	if err := models.AutoMigrate(db); err != nil {
		return 0, err
	}
	tables := []string{
		"receipts", "payments", "exchanges", "seat_occupancies",
		"reservations", "tickets", "orders", "coupons", "seats",
		"sessions", "movies", "members",
	}
	for _, t := range tables {
		if err := db.Exec("DELETE FROM " + t).Error; err != nil {
			return 0, err
		}
	}
	// 重置自增序列，保证演示 ID 稳定（会员从 1 开始）。
	db.Exec("DELETE FROM sqlite_sequence")

	m := models.Member{Name: "张三", Phone: "13800000001"}
	if err := db.Create(&m).Error; err != nil {
		return 0, err
	}
	other := models.Member{Name: "李四", Phone: "13800000002"}
	db.Create(&other)

	movies := []models.Movie{
		{Title: "星际穿越"},
		{Title: "沙丘 3"},
		{Title: "你的名字"},
	}
	for i := range movies {
		db.Create(&movies[i])
	}

	// 三场：同影片不同时间（跨场次改签），票价不同用于演示补/退。
	day := time.Date(2026, 10, 6, 0, 0, 0, 0, now.Location())
	sessions := []models.Session{
		{MovieID: movies[0].ID, Hall: "1号激光厅", StartAt: day.Add(14 * time.Hour), BasePrice: 6000},
		{MovieID: movies[0].ID, Hall: "1号激光厅", StartAt: day.Add(19 * time.Hour), BasePrice: 8000},
		{MovieID: movies[1].ID, Hall: "2号IMAX厅", StartAt: day.Add(20*time.Hour + 30*time.Minute), BasePrice: 10000},
		{MovieID: movies[2].ID, Hall: "3号杜比厅", StartAt: day.Add(16 * time.Hour), BasePrice: 5000},
	}
	for i := range sessions {
		db.Create(&sessions[i])
		// 5 排 x 8 座
		for r := 0; r < 5; r++ {
			for n := 1; n <= 8; n++ {
				db.Create(&models.Seat{SessionID: sessions[i].ID, Row: string(rune('A' + r)), Number: n})
			}
		}
	}

	// 券：8折券(未用)、30元立减券(未用)、9折券(已被原订单使用)、5折券(已过期)
	coupons := []models.Coupon{
		{MemberID: m.ID, Name: "会员8折券", Type: "discount", Rate: 80, ExpireAt: now.Add(7 * 24 * time.Hour)},
		{MemberID: m.ID, Name: "立减30元券", Type: "amount", Amount: 3000, ExpireAt: now.Add(7 * 24 * time.Hour)},
		{MemberID: m.ID, Name: "新人9折券", Type: "discount", Rate: 90, ExpireAt: now.Add(7 * 24 * time.Hour), RedeemedAt: &now},
		{MemberID: m.ID, Name: "过期5折券", Type: "discount", Rate: 50, ExpireAt: now.Add(-24 * time.Hour)},
	}
	for i := range coupons {
		db.Create(&coupons[i])
	}

	// 原订单：张三买两张同场次票（同行单），使用“新人9折券”抵扣——
	// 券抵扣按每张票记录。6000 的 9 折 = 5400，优惠 600/张，实付 5400/张。
	oldSess := sessions[0]
	usedCoupon := coupons[2]
	order := models.Order{
		OrderNo:   service.GenNo("ORD"),
		MemberID:  m.ID,
		Status:    models.OrderStatusPaid,
		PayAmount: 10800,
	}
	if err := db.Create(&order).Error; err != nil {
		return 0, err
	}
	seatsToBuy := []struct {
		row string
		num int
	}{{"A", 3}, {"A", 4}}
	for _, sb := range seatsToBuy {
		var seat models.Seat
		db.Where("session_id = ? AND `row` = ? AND number = ?", oldSess.ID, sb.row, sb.num).First(&seat)
		disc := int64(600)
		t := models.Ticket{
			TicketNo:     service.GenNo("TKT"),
			OrderID:      order.ID,
			MemberID:     m.ID,
			SessionID:    oldSess.ID,
			SeatID:       seat.ID,
			Status:       models.TicketStatusIssued,
			BasePrice:    oldSess.BasePrice,
			CouponID:     &usedCoupon.ID,
			CouponName:   usedCoupon.Name,
			CouponType:   usedCoupon.Type,
			CouponRate:   usedCoupon.Rate,
			CouponAmount: usedCoupon.Amount,
			Discount:     disc,
			PaidAmount:   oldSess.BasePrice - disc,
		}
		if err := db.Create(&t).Error; err != nil {
			return 0, err
		}
		db.Create(&models.SeatOccupancy{SessionID: oldSess.ID, SeatID: seat.ID, HolderType: service.HolderTicket, HolderID: t.ID})
	}

	return m.ID, nil
}

package service

import (
	"errors"

	"gorm.io/gorm"

	"cinema/models"
)

// MemberTickets 返回会员的有效票（含场次/座位/同行订单信息）。
func (s *ExchangeService) MemberTickets(memberID uint) ([]map[string]interface{}, error) {
	var tickets []models.Ticket
	if err := s.db.Where("member_id = ? AND status = ?", memberID, models.TicketStatusIssued).
		Order("id").Find(&tickets).Error; err != nil {
		return nil, err
	}
	out := make([]map[string]interface{}, 0, len(tickets))
	for i := range tickets {
		t := &tickets[i]
		view, err := s.ticketPriceView(t)
		if err != nil {
			return nil, err
		}
		// 同行票数统计（同一购票订单下有效票数量）
		var companions int64
		s.db.Model(&models.Ticket{}).
			Where("order_id = ? AND status = ? AND id <> ?", t.OrderID, models.TicketStatusIssued, t.ID).
			Count(&companions)
		out = append(out, map[string]interface{}{
			"id":         t.ID,
			"ticket_no":  t.TicketNo,
			"order_id":   t.OrderID,
			"companions": companions,
			"price":      view,
		})
	}
	return out, nil
}

// MemberCoupons 返回会员全部券及状态。
func (s *ExchangeService) MemberCoupons(memberID uint) ([]CouponView, error) {
	var cs []models.Coupon
	if err := s.db.Where("member_id = ?", memberID).Order("id").Find(&cs).Error; err != nil {
		return nil, err
	}
	now := s.now()
	out := make([]CouponView, 0, len(cs))
	for i := range cs {
		v := toCouponView(&cs[i], now)
		out = append(out, *v)
	}
	return out, nil
}

// Sessions 列出全部场次（影片/影厅/时间/票价）。
func (s *ExchangeService) Sessions() ([]SessionView, error) {
	var ss []models.Session
	if err := s.db.Preload("Movie").Order("start_at").Find(&ss).Error; err != nil {
		return nil, err
	}
	out := make([]SessionView, 0, len(ss))
	for _, sess := range ss {
		name := ""
		if sess.Movie != nil {
			name = sess.Movie.Title
		}
		out = append(out, SessionView{ID: sess.ID, Movie: name, Hall: sess.Hall, StartAt: sess.StartAt, BasePrice: sess.BasePrice})
	}
	return out, nil
}

// ExchangeDetail 返回改签单完整四栏视图（原票 / 新票 / 券 / 补退）。
func (s *ExchangeService) ExchangeDetail(exchangeNo string, memberID uint) (map[string]interface{}, error) {
	var e models.Exchange
	q := s.db.Where("exchange_no = ?", exchangeNo)
	if memberID != 0 {
		q = q.Where("member_id = ?", memberID)
	}
	if err := q.First(&e).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var oldT models.Ticket
	if err := s.db.First(&oldT, e.TicketID).Error; err != nil {
		return nil, err
	}
	oldView, err := s.ticketPriceView(&oldT)
	if err != nil {
		return nil, err
	}
	var newSess models.Session
	if err := s.db.Preload("Movie").First(&newSess, e.NewSessionID).Error; err != nil {
		return nil, err
	}
	var newSeat models.Seat
	if err := s.db.First(&newSeat, e.NewSeatID).Error; err != nil {
		return nil, err
	}
	nv := NewTicketView{
		Session:   SessionView{ID: newSess.ID, Hall: newSess.Hall, StartAt: newSess.StartAt, BasePrice: newSess.BasePrice},
		Seat:      SeatView{ID: newSeat.ID, Row: newSeat.Row, Number: newSeat.Number},
		BasePrice: e.NewBasePrice,
		Discount:  e.NewDiscount,
		Paid:      e.NewPaid,
	}
	if newSess.Movie != nil {
		nv.Session.Movie = newSess.Movie.Title
	}
	if e.NewCouponID != nil {
		var c models.Coupon
		if err := s.db.First(&c, *e.NewCouponID).Error; err == nil {
			nv.Coupon = toCouponView(&c, s.now())
		} else if e.NewCouponSnapshot != "" {
			nv.Coupon = &CouponView{Name: e.NewCouponSnapshot, Status: "redeemed"}
		}
	} else if oldT.CouponID != nil && e.NewDiscount > 0 {
		nv.Coupon = &CouponView{Name: oldT.CouponName, Type: oldT.CouponType, Rate: oldT.CouponRate, Amount: oldT.CouponAmount, Status: "carried"}
	}

	var r models.Reservation
	s.db.First(&r, e.ReservationID)
	res := map[string]interface{}{
		"status":    r.Status,
		"expire_at": r.ExpireAt,
	}

	// 同订单其余票（验证“同行单只改一张”）
	var mates []models.Ticket
	s.db.Where("order_id = ? AND status = ? AND id <> ?", oldT.OrderID, models.TicketStatusIssued, oldT.ID).Find(&mates)
	mateViews := make([]map[string]interface{}, 0, len(mates))
	for i := range mates {
		var st models.Seat
		s.db.First(&st, mates[i].SeatID)
		mateViews = append(mateViews, map[string]interface{}{
			"ticket_no":  mates[i].TicketNo,
			"session_id": mates[i].SessionID,
			"seat":       st.Row + "-" + itoa(st.Number),
			"status":     mates[i].Status,
		})
	}

	return map[string]interface{}{
		"exchange_no":     e.ExchangeNo,
		"status":          e.Status,
		"fail_reason":     e.FailReason,
		"reservation":     res,
		"old_ticket":      oldView,
		"new_ticket":      nv,
		"diff":            diffView(e.DiffAmount),
		"companions_kept": mateViews,
		"new_ticket_id":   e.NewTicketID,
		"created_at":      e.CreatedAt,
		"paid_at":         e.PaidAt,
	}, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

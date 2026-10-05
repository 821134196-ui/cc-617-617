package service

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"gorm.io/gorm"

	"cinema/models"
)

var (
	ErrNotFound           = errors.New("not found")
	ErrSeatTaken          = errors.New("seat already sold or held")
	ErrSameSeat           = errors.New("new seat is the same as the old seat")
	ErrTicketNotActive    = errors.New("ticket is not valid for exchange")
	ErrExchangeState      = errors.New("exchange status does not allow this action")
	ErrReservationExpired = errors.New("reservation already expired")
	ErrAmountMismatch     = errors.New("receipt amount does not match payment")
	ErrMemberMismatch     = errors.New("resource does not belong to member")
)

const (
	HolderTicket      = "ticket"
	HolderReservation = "reservation"
)

type ExchangeService struct {
	db  *gorm.DB
	ttl time.Duration
	now func() time.Time
	// wmu 串行化所有写事务。SQLite(WAL) 只允许单写者，靠驱动 busy_timeout
	// 在极端并发下仍可能立即返回 SQLITE_BUSY；进程内互斥保证竞争方
	// 快速得到确定的业务冲突（409 seat_taken），而不是 500 锁错误。
	// 座位归属的最终裁决权仍在数据库唯一索引，不依赖这把锁。
	wmu sync.Mutex
}

func NewExchangeService(db *gorm.DB, ttl time.Duration) *ExchangeService {
	if ttl == 0 {
		ttl = 60 * time.Second
	}
	return &ExchangeService{db: db, ttl: ttl, now: time.Now}
}

// SetClock 注入时钟（测试用）。
func (s *ExchangeService) SetClock(f func() time.Time) { s.now = f }

// writeTx 在进程内写互斥下执行一个数据库事务。
func (s *ExchangeService) writeTx(fn func(tx *gorm.DB) error) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.db.Transaction(fn)
}

// ---------------- 查询 / 报价 ----------------

type SeatView struct {
	ID       uint   `json:"id"`
	Row      string `json:"row"`
	Number   int    `json:"number"`
	Disabled bool   `json:"disabled"`
	Status   string `json:"status"` // available / held / sold / mine
	Holder   string `json:"holder,omitempty"`
}

// SeatMap 返回场次座位图。sold=有效票；held=未过期预留；mineTicket 时该票对应座位标 mine。
func (s *ExchangeService) SeatMap(sessionID, memberID uint, mineTicketID uint) ([]SeatView, error) {
	var seats []models.Seat
	if err := s.db.Where("session_id = ?", sessionID).Order("`row`, number").Find(&seats).Error; err != nil {
		return nil, err
	}
	var occ []models.SeatOccupancy
	if err := s.db.Where("session_id = ?", sessionID).Find(&occ).Error; err != nil {
		return nil, err
	}
	occMap := map[uint]models.SeatOccupancy{}
	for _, o := range occ {
		occMap[o.SeatID] = o
	}
	now := s.now()
	out := make([]SeatView, 0, len(seats))
	for _, st := range seats {
		v := SeatView{ID: st.ID, Row: st.Row, Number: st.Number, Disabled: st.Disabled, Status: "available"}
		if o, ok := occMap[st.ID]; ok {
			switch o.HolderType {
			case HolderTicket:
				v.Status = "sold"
				if o.HolderID == mineTicketID {
					v.Status = "mine"
				}
			case HolderReservation:
				var r models.Reservation
				s.db.First(&r, o.HolderID)
				if r.Status == models.ReservationHeld && r.ExpireAt.After(now) {
					v.Status = "held"
				}
			}
		}
		out = append(out, v)
	}
	return out, nil
}

type QuoteView struct {
	ExchangeNo string          `json:"exchange_no,omitempty"`
	Status     string          `json:"status,omitempty"`
	ExpireAt   *time.Time      `json:"expire_at,omitempty"`
	OldTicket  TicketPriceView `json:"old_ticket"`
	NewTicket  NewTicketView   `json:"new_ticket"`
	Diff       DiffView        `json:"diff"`
}

type TicketPriceView struct {
	TicketNo  string      `json:"ticket_no"`
	Session   SessionView `json:"session"`
	Seat      SeatView    `json:"seat"`
	BasePrice int64       `json:"base_price"`
	Coupon    *CouponView `json:"coupon"`
	Discount  int64       `json:"discount"`
	Paid      int64       `json:"paid"`
}

type NewTicketView struct {
	Session   SessionView `json:"session"`
	Seat      SeatView    `json:"seat"`
	BasePrice int64       `json:"base_price"`
	Coupon    *CouponView `json:"coupon"`
	Discount  int64       `json:"discount"`
	Paid      int64       `json:"paid"`
}

type DiffView struct {
	Amount int64  `json:"amount"` // >0 补交；<0 退还
	Action string `json:"action"` // pay / refund / none
	Reason string `json:"reason"`
}

type SessionView struct {
	ID        uint      `json:"id"`
	Movie     string    `json:"movie"`
	Hall      string    `json:"hall"`
	StartAt   time.Time `json:"start_at"`
	BasePrice int64     `json:"base_price"`
}

type CouponView struct {
	ID       uint      `json:"id"`
	Name     string    `json:"name"`
	Type     string    `json:"type"`
	Rate     int       `json:"rate,omitempty"`
	Amount   int64     `json:"amount,omitempty"`
	ExpireAt time.Time `json:"expire_at"`
	Status   string    `json:"status"` // available / redeemed / expired
}

func toCouponView(c *models.Coupon, now time.Time) *CouponView {
	if c == nil {
		return nil
	}
	st := "available"
	if c.RedeemedAt != nil {
		st = "redeemed"
	} else if !c.ExpireAt.After(now) {
		st = "expired"
	}
	return &CouponView{ID: c.ID, Name: c.Name, Type: c.Type, Rate: c.Rate, Amount: c.Amount, ExpireAt: c.ExpireAt, Status: st}
}

func (s *ExchangeService) loadSessionView(id uint) (SessionView, error) {
	var sess models.Session
	if err := s.db.Preload("Movie").First(&sess, id).Error; err != nil {
		return SessionView{}, ErrNotFound
	}
	name := ""
	if sess.Movie != nil {
		name = sess.Movie.Title
	}
	return SessionView{ID: sess.ID, Movie: name, Hall: sess.Hall, StartAt: sess.StartAt, BasePrice: sess.BasePrice}, nil
}

func (s *ExchangeService) ticketPriceView(t *models.Ticket) (TicketPriceView, error) {
	sv, err := s.loadSessionView(t.SessionID)
	if err != nil {
		return TicketPriceView{}, err
	}
	var seat models.Seat
	if err := s.db.First(&seat, t.SeatID).Error; err != nil {
		return TicketPriceView{}, err
	}
	v := TicketPriceView{
		TicketNo:  t.TicketNo,
		Session:   sv,
		Seat:      SeatView{ID: seat.ID, Row: seat.Row, Number: seat.Number, Status: "mine"},
		BasePrice: t.BasePrice,
		Discount:  t.Discount,
		Paid:      t.PaidAmount,
	}
	if t.CouponID != nil {
		v.Coupon = &CouponView{Name: t.CouponName, Type: t.CouponType, Rate: t.CouponRate, Amount: t.CouponAmount}
	}
	return v, nil
}

// Quote 纯报价（不落库）。couponID 传 nil 表示沿用原券优惠口径。
func (s *ExchangeService) Quote(memberID, ticketID, newSessionID, newSeatID uint, couponID *uint) (QuoteView, error) {
	now := s.now()
	t, err := s.loadActiveTicket(memberID, ticketID)
	if err != nil {
		return QuoteView{}, err
	}
	sess, seat, err := s.checkTargetSeat(newSessionID, newSeatID, t)
	if err != nil {
		return QuoteView{}, err
	}
	var chosen *models.Coupon
	if couponID != nil {
		chosen, err = s.loadUsableCoupon(memberID, *couponID, now)
		if err != nil {
			return QuoteView{}, err
		}
	}
	q, err := QuoteExchange(t, sess.BasePrice, chosen, true, now)
	if err != nil {
		return QuoteView{}, err
	}
	return s.buildQuoteView(t, sess, seat, q), nil
}

func (s *ExchangeService) loadActiveTicket(memberID, ticketID uint) (*models.Ticket, error) {
	var t models.Ticket
	if err := s.db.First(&t, ticketID).Error; err != nil {
		return nil, ErrNotFound
	}
	if t.MemberID != memberID {
		return nil, ErrMemberMismatch
	}
	if t.Status != models.TicketStatusIssued {
		return nil, ErrTicketNotActive
	}
	return &t, nil
}

func (s *ExchangeService) checkTargetSeat(sessionID, seatID uint, old *models.Ticket) (models.Session, models.Seat, error) {
	var sess models.Session
	if err := s.db.First(&sess, sessionID).Error; err != nil {
		return sess, models.Seat{}, ErrNotFound
	}
	var seat models.Seat
	if err := s.db.Where("id = ? AND session_id = ?", seatID, sessionID).First(&seat).Error; err != nil {
		return sess, seat, ErrNotFound
	}
	if seat.Disabled {
		return sess, seat, ErrSeatTaken
	}
	if old.SessionID == sessionID && old.SeatID == seatID {
		return sess, seat, ErrSameSeat
	}
	var occ int64
	s.db.Model(&models.SeatOccupancy{}).Where("session_id = ? AND seat_id = ?", sessionID, seatID).Count(&occ)
	if occ > 0 {
		return sess, seat, ErrSeatTaken
	}
	return sess, seat, nil
}

func (s *ExchangeService) loadUsableCoupon(memberID, couponID uint, now time.Time) (*models.Coupon, error) {
	var c models.Coupon
	if err := s.db.First(&c, couponID).Error; err != nil {
		return nil, ErrNotFound
	}
	if c.MemberID != memberID {
		return nil, ErrMemberMismatch
	}
	if _, err := CalcDiscount(100, &c, now); err != nil {
		return nil, err
	}
	return &c, nil
}

func diffView(d int64) DiffView {
	switch {
	case d > 0:
		return DiffView{Amount: d, Action: "pay", Reason: "新票应付高于原票实付，需补交差额"}
	case d < 0:
		return DiffView{Amount: d, Action: "refund", Reason: "新票应付低于原票实付，退还差额"}
	default:
		return DiffView{Amount: 0, Action: "none", Reason: "新旧票应付相同，无需补退"}
	}
}

func (s *ExchangeService) buildQuoteView(t *models.Ticket, sess models.Session, seat models.Seat, q Quote) QuoteView {
	oldView, _ := s.ticketPriceView(t)
	nv := NewTicketView{
		Session:   SessionView{ID: sess.ID, Hall: sess.Hall, StartAt: sess.StartAt, BasePrice: sess.BasePrice},
		Seat:      SeatView{ID: seat.ID, Row: seat.Row, Number: seat.Number, Status: "available"},
		BasePrice: q.NewBasePrice,
		Discount:  q.NewDiscount,
		Paid:      q.NewPaid,
		Coupon:    toCouponView(q.NewCoupon, s.now()),
	}
	if mv, err := s.loadSessionView(sess.ID); err == nil {
		nv.Session = mv
	}
	if q.NewCoupon == nil && t.CouponID != nil && q.NewDiscount > 0 {
		// 沿用原券：展示原券快照
		nv.Coupon = &CouponView{Name: t.CouponName, Type: t.CouponType, Rate: t.CouponRate, Amount: t.CouponAmount, Status: "carried"}
	}
	return QuoteView{OldTicket: oldView, NewTicket: nv, Diff: diffView(q.DiffAmount)}
}

// ---------------- 创建改签（同时限时预留新座位） ----------------

type CreateInput struct {
	MemberID       uint
	TicketID       uint
	NewSessionID   uint
	NewSeatID      uint
	CouponID       *uint
	IdempotencyKey string
}

// Create 在一个事务内完成：校验 → 占用新座位 → 写预留 → 写改签单 →（用新券时）占用券。
func (s *ExchangeService) Create(in CreateInput) (*models.Exchange, error) {
	now := s.now()
	var ex *models.Exchange

	err := s.writeTx(func(tx *gorm.DB) error {
		// 幂等：相同 IdempotencyKey 直接返回原单（客户端重试/双击）。
		if in.IdempotencyKey != "" {
			var old models.Exchange
			if err := tx.Where("idempotency_key = ?", in.IdempotencyKey).First(&old).Error; err == nil {
				ex = &old
				return nil
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}

		t, err := s.loadActiveTicketLocked(tx, in.MemberID, in.TicketID)
		if err != nil {
			return err
		}

		sess, seat, err := s.checkTargetSeatTx(tx, in.NewSessionID, in.NewSeatID, t)
		if err != nil {
			return err
		}

		var chosen *models.Coupon
		if in.CouponID != nil {
			if t.CouponID != nil && *in.CouponID == *t.CouponID {
				return ErrCouponRedeemed
			}
			c, err := s.loadUsableCouponLocked(tx, in.MemberID, *in.CouponID, now)
			if err != nil {
				return err
			}
			chosen = c
		}

		q, err := QuoteExchange(t, sess.BasePrice, chosen, true, now)
		if err != nil {
			return err
		}

		expireAt := now.Add(s.ttl)
		res := models.Reservation{
			ReserveNo: genNo("RSV"),
			MemberID:  in.MemberID,
			SessionID: sess.ID,
			SeatID:    seat.ID,
			TicketID:  &t.ID,
			Status:    models.ReservationHeld,
			ExpireAt:  expireAt,
		}
		if err := tx.Create(&res).Error; err != nil {
			return err
		}

		// 1) 占用新座位——唯一索引兜底并发抢座，冲突即 ErrSeatTaken。
		// holder_id 必须指向刚创建的预留记录。
		occ := models.SeatOccupancy{SessionID: sess.ID, SeatID: seat.ID,
			HolderType: HolderReservation, HolderID: res.ID}
		if err := tx.Create(&occ).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return ErrSeatTaken
			}
			return err
		}

		captured := false
		var newCouponID *uint
		couponSnap := ""
		if chosen != nil {
			// 立即占用券，防止同一券被另一笔改签并发使用；失败/过期时在回滚路径释放。
			ts := now
			cr := tx.Model(&models.Coupon{}).
				Where("id = ? AND redeemed_at IS NULL", chosen.ID).
				Update("redeemed_at", ts)
			if cr.Error != nil {
				return cr.Error
			}
			if cr.RowsAffected == 0 {
				return ErrCouponRedeemed
			}
			captured = true
			id := chosen.ID
			newCouponID = &id
			couponSnap = chosen.Name
		}

		newEx := models.Exchange{
			ExchangeNo:        genNo("EX"),
			MemberID:          in.MemberID,
			TicketID:          t.ID,
			OldSessionID:      t.SessionID,
			OldSeatID:         t.SeatID,
			NewSessionID:      sess.ID,
			NewSeatID:         seat.ID,
			ReservationID:     res.ID,
			OldBasePrice:      q.OldBasePrice,
			OldDiscount:       q.OldDiscount,
			OldPaid:           q.OldPaid,
			NewBasePrice:      q.NewBasePrice,
			NewDiscount:       q.NewDiscount,
			NewPaid:           q.NewPaid,
			DiffAmount:        q.DiffAmount,
			OldCouponID:       t.CouponID,
			NewCouponID:       newCouponID,
			NewCouponSnapshot: couponSnap,
			Status:            models.ExCreated,
			ActiveTicketKey:   &t.ID,
			CouponCaptured:    captured,
		}
		if in.IdempotencyKey != "" {
			k := in.IdempotencyKey
			newEx.IdempotencyKey = &k
		}
		if err := tx.Create(&newEx).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				// 同票进行中的改签冲突，或幂等键冲突。
				return ErrExchangeState
			}
			return err
		}
		// 回填关联
		if err := tx.Model(&res).Where("id = ?", res.ID).Update("exchange_id", newEx.ID).Error; err != nil {
			return err
		}
		ex = &newEx
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ex, nil
}

func (s *ExchangeService) loadActiveTicketLocked(tx *gorm.DB, memberID, ticketID uint) (*models.Ticket, error) {
	var t models.Ticket
	// SQLite 无 SELECT FOR UPDATE；用条件 UPDATE 触发行级序列化，再读回。
	if err := tx.Exec("UPDATE tickets SET updated_at = updated_at WHERE id = ? AND status = ?", ticketID, models.TicketStatusIssued).Error; err != nil {
		return nil, err
	}
	if err := tx.First(&t, ticketID).Error; err != nil {
		return nil, ErrNotFound
	}
	if t.MemberID != memberID {
		return nil, ErrMemberMismatch
	}
	if t.Status != models.TicketStatusIssued {
		return nil, ErrTicketNotActive
	}
	return &t, nil
}

func (s *ExchangeService) checkTargetSeatTx(tx *gorm.DB, sessionID, seatID uint, old *models.Ticket) (models.Session, models.Seat, error) {
	var sess models.Session
	if err := tx.First(&sess, sessionID).Error; err != nil {
		return sess, models.Seat{}, ErrNotFound
	}
	var seat models.Seat
	if err := tx.Where("id = ? AND session_id = ?", seatID, sessionID).First(&seat).Error; err != nil {
		return sess, seat, ErrNotFound
	}
	if seat.Disabled {
		return sess, seat, ErrSeatTaken
	}
	if old.SessionID == sessionID && old.SeatID == seatID {
		return sess, seat, ErrSameSeat
	}
	var occ int64
	tx.Model(&models.SeatOccupancy{}).Where("session_id = ? AND seat_id = ?", sessionID, seatID).Count(&occ)
	if occ > 0 {
		return sess, seat, ErrSeatTaken
	}
	return sess, seat, nil
}

func (s *ExchangeService) loadUsableCouponLocked(tx *gorm.DB, memberID, couponID uint, now time.Time) (*models.Coupon, error) {
	var c models.Coupon
	if err := tx.First(&c, couponID).Error; err != nil {
		return nil, ErrNotFound
	}
	if c.MemberID != memberID {
		return nil, ErrMemberMismatch
	}
	if _, err := CalcDiscount(100, &c, now); err != nil {
		return nil, err
	}
	return &c, nil
}

// ---------------- 发起支付（可异步：只把改签单置 paying 并生成待支付单） ----------------

// InitiatePayment 创建/返回待支付的收款单，并把改签单 CAS 到 paying。
// 若金额 <=0（退差/平价），不应调用：退差在改签成功后自动退；平价走 FinalizeZero。
func (s *ExchangeService) InitiatePayment(exchangeNo string, memberID uint) (*models.Payment, *models.Exchange, error) {
	var pay *models.Payment
	var ex *models.Exchange
	err := s.writeTx(func(tx *gorm.DB) error {
		e, err := s.getExchangeForMember(tx, exchangeNo, memberID)
		if err != nil {
			return err
		}
		if e.DiffAmount <= 0 {
			return fmt.Errorf("%w: non-positive diff", ErrExchangeState)
		}
		// 已有支付单（重试）直接返回。
		var existing models.Payment
		if err := tx.Where("exchange_id = ? AND type = ?", e.ID, "charge").First(&existing).Error; err == nil {
			pay = &existing
			ex = e
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if e.Status != models.ExCreated && e.Status != models.ExPaying {
			return ErrExchangeState
		}
		if err := s.ensureReservationLive(tx, e); err != nil {
			return err
		}
		if e.Status == models.ExCreated {
			if err := casExchange(tx, e.ID, models.ExCreated, models.ExPaying, nil); err != nil {
				return err
			}
			e.Status = models.ExPaying
		}
		p := models.Payment{
			PaymentNo:  genNo("PAY"),
			ExchangeID: e.ID,
			Amount:     e.DiffAmount,
			Type:       "charge",
			Status:     models.PayPending,
			Channel:    "mock",
		}
		if err := tx.Create(&p).Error; err != nil {
			return err
		}
		pay = &p
		ex = e
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return pay, ex, nil
}

func (s *ExchangeService) getExchangeForMember(tx *gorm.DB, no string, memberID uint) (*models.Exchange, error) {
	var e models.Exchange
	if err := tx.Where("exchange_no = ?", no).First(&e).Error; err != nil {
		return nil, ErrNotFound
	}
	if e.MemberID != memberID {
		return nil, ErrMemberMismatch
	}
	return &e, nil
}

func (s *ExchangeService) ensureReservationLive(tx *gorm.DB, e *models.Exchange) error {
	var r models.Reservation
	if err := tx.First(&r, e.ReservationID).Error; err != nil {
		return err
	}
	if r.Status != models.ReservationHeld {
		return ErrReservationExpired
	}
	if !r.ExpireAt.After(s.now()) {
		// 顺手执行过期
		if err := s.expireOne(tx, e); err != nil {
			return err
		}
		return ErrReservationExpired
	}
	return nil
}

// FinalizeZero 平价改签：无需支付，直接完成。
func (s *ExchangeService) FinalizeZero(exchangeNo string, memberID uint) (*models.Exchange, error) {
	var ex *models.Exchange
	err := s.writeTx(func(tx *gorm.DB) error {
		e, err := s.getExchangeForMember(tx, exchangeNo, memberID)
		if err != nil {
			return err
		}
		if e.DiffAmount != 0 {
			return fmt.Errorf("%w: diff is not zero", ErrExchangeState)
		}
		if err := s.ensureReservationLive(tx, e); err != nil {
			return err
		}
		return s.finalizeSuccess(tx, e)
	})
	if err != nil {
		return nil, err
	}
	return ex, nil
}

// FinalizeRefund 退差改签：无需用户付款，确认后直接完成，系统在终态事务内自动退差。
func (s *ExchangeService) FinalizeRefund(exchangeNo string, memberID uint) error {
	return s.writeTx(func(tx *gorm.DB) error {
		e, err := s.getExchangeForMember(tx, exchangeNo, memberID)
		if err != nil {
			return err
		}
		if e.DiffAmount >= 0 {
			return fmt.Errorf("%w: diff is not negative", ErrExchangeState)
		}
		if err := s.ensureReservationLive(tx, e); err != nil {
			return err
		}
		return s.finalizeSuccess(tx, e)
	})
}

// PaymentInfo 查询支付单对应的改签单号与金额（模拟网关回执时使用）。
func (s *ExchangeService) PaymentInfo(paymentNo string) (string, int64, error) {
	var p models.Payment
	if err := s.db.Where("payment_no = ?", paymentNo).First(&p).Error; err != nil {
		return "", 0, ErrNotFound
	}
	var e models.Exchange
	if err := s.db.First(&e, p.ExchangeID).Error; err != nil {
		return "", 0, ErrNotFound
	}
	return e.ExchangeNo, p.Amount, nil
}

// ---------------- 回执（幂等核心） ----------------

type ReceiptInput struct {
	ReceiptNo  string
	PaymentNo  string
	ExchangeNo string
	Kind       string // pay_success / pay_failed / refund
	Status     string // success / failed
	Amount     int64
	Note       string
}

type ReceiptResult struct {
	Receipt    *models.Receipt `json:"receipt"`
	Duplicate  bool            `json:"duplicate"`
	Effect     string          `json:"effect"` // exchange_succeeded / exchange_failed / refund_recorded / ignored_stale / duplicate
	ExchangeNO string          `json:"exchange_no,omitempty"`
}

// ApplyReceipt 处理支付/退款回调。
// 同一 receipt_no 永远只生效一次；迟到、重复回执不会触发第二次改签。
func (s *ExchangeService) ApplyReceipt(in ReceiptInput) (*ReceiptResult, error) {
	// 先在短事务里登记回执，靠 receipt_no 唯一索引判重。
	var (
		rec   *models.Receipt
		isDup bool
	)
	err := s.writeTx(func(tx *gorm.DB) error {
		var existing models.Receipt
		if err := tx.Where("receipt_no = ?", in.ReceiptNo).First(&existing).Error; err == nil {
			rec = &existing
			isDup = true
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		r := models.Receipt{
			ReceiptNo:  in.ReceiptNo,
			PaymentNo:  in.PaymentNo,
			ExchangeNo: in.ExchangeNo,
			Kind:       in.Kind,
			Status:     in.Status,
			Amount:     in.Amount,
			Note:       in.Note,
			Applied:    false,
		}
		if err := tx.Create(&r).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				if e2 := tx.Where("receipt_no = ?", in.ReceiptNo).First(&existing).Error; e2 == nil {
					rec = &existing
					isDup = true
					return nil
				}
			}
			return err
		}
		rec = &r
		return nil
	})
	if err != nil {
		return nil, err
	}
	if isDup {
		return &ReceiptResult{Receipt: rec, Duplicate: true, Effect: "duplicate", ExchangeNO: rec.ExchangeNo}, nil
	}

	// 新回执：在独立事务中作用于业务。
	var effect string
	err = s.writeTx(func(tx *gorm.DB) error {
		var e models.Exchange
		if err := tx.Where("exchange_no = ?", in.ExchangeNo).First(&e).Error; err != nil {
			return ErrNotFound
		}

		// 退款回执只登记退款单，绝不触碰改签状态（退款不是改签的触发器）。
		if in.Kind == "refund" {
			eff, err := s.applyRefundReceipt(tx, &e, in)
			if err != nil {
				return err
			}
			effect = eff
			return markApplied(tx, rec.ID)
		}

		switch {
		case in.Status == "success":
			// 成功收款
			p, err := s.validateChargeReceipt(tx, &e, in)
			if err != nil {
				return err
			}
			// 终态改签单收到回执：不允许第二次改签。
			if e.Status == models.ExSucceeded {
				effect = "ignored_stale"
				return tx.Model(&models.Receipt{}).Where("id = ?", rec.ID).
					Updates(map[string]interface{}{"applied": false, "note": "stale: exchange already succeeded"}).Error
			}
			if e.Status == models.ExFailed || e.Status == models.ExExpired || e.Status == models.ExCanceled {
				// 迟到的成功收款（预留已过期或支付失败已回滚）：钱收下但必须自动退款，绝不改签。
				if err := tx.Model(&p).Update("status", models.PaySuccess).Error; err != nil {
					return err
				}
				if err := s.automaticRefund(tx, &e, p, rec.ID, "late success after terminal state"); err != nil {
					return err
				}
				effect = "auto_refund_late_success"
				return nil
			}
			// created / paying：校验预留仍有效，然后完成改签。
			if err := s.ensureReservationLive(tx, &e); err != nil {
				if errors.Is(err, ErrReservationExpired) {
					// 与超时竞争失败：标记收款成功后自动退款，原票不动。
					if err := tx.Model(&p).Update("status", models.PaySuccess).Error; err != nil {
						return err
					}
					if err := s.automaticRefund(tx, &e, p, rec.ID, "success arrived after reservation expired"); err != nil {
						return err
					}
					effect = "auto_refund_after_expiry"
					return nil
				}
				return err
			}
			if err := tx.Model(&p).Where("status = ?", models.PayPending).
				Update("status", models.PaySuccess).Error; err != nil {
				return err
			}
			if err := s.finalizeSuccess(tx, &e); err != nil {
				return err
			}
			effect = "exchange_succeeded"
			return markApplied(tx, rec.ID)

		case in.Status == "failed":
			p, err := s.validateChargeReceipt(tx, &e, in)
			if err != nil {
				return err
			}
			if e.Status == models.ExSucceeded {
				effect = "ignored_stale"
				return nil
			}
			if e.Status == models.ExFailed {
				effect = "ignored_stale"
				return markApplied(tx, rec.ID)
			}
			if err := tx.Model(&p).Where("status = ?", models.PayPending).
				Update("status", models.PayFailed).Error; err != nil {
				return err
			}
			if err := s.failExchange(tx, &e, "payment failed: "+in.Note); err != nil {
				return err
			}
			effect = "exchange_failed"
			return markApplied(tx, rec.ID)

		default:
			return fmt.Errorf("unknown receipt status %q", in.Status)
		}
	})
	if err != nil {
		return nil, err
	}
	return &ReceiptResult{Receipt: rec, Duplicate: false, Effect: effect, ExchangeNO: in.ExchangeNo}, nil
}

func markApplied(tx *gorm.DB, id uint) error {
	return tx.Model(&models.Receipt{}).Where("id = ?", id).Update("applied", true).Error
}

func (s *ExchangeService) validateChargeReceipt(tx *gorm.DB, e *models.Exchange, in ReceiptInput) (*models.Payment, error) {
	var p models.Payment
	if err := tx.Where("payment_no = ? AND exchange_id = ?", in.PaymentNo, e.ID).First(&p).Error; err != nil {
		return nil, fmt.Errorf("%w: payment %s", ErrNotFound, in.PaymentNo)
	}
	if p.Type != "charge" {
		return nil, fmt.Errorf("payment %s is not a charge", in.PaymentNo)
	}
	if in.Amount != p.Amount {
		return nil, ErrAmountMismatch
	}
	return &p, nil
}

// automaticRefund 改签无法完成但钱已收到：生成退款单（成功）并登记退款回执。
func (s *ExchangeService) automaticRefund(tx *gorm.DB, e *models.Exchange, charge *models.Payment, receiptID uint, reason string) error {
	refund := models.Payment{
		PaymentNo:  genNo("RFD"),
		ExchangeID: e.ID,
		Amount:     charge.Amount,
		Type:       "refund",
		Status:     models.PaySuccess,
		Channel:    "mock",
	}
	if err := tx.Create(&refund).Error; err != nil {
		return err
	}
	rc := models.Receipt{
		ReceiptNo:  genNo("RCP"),
		PaymentNo:  refund.PaymentNo,
		ExchangeNo: e.ExchangeNo,
		Kind:       "refund",
		Status:     "success",
		Amount:     refund.Amount,
		Applied:    true,
		Note:       "auto refund: " + reason,
	}
	if err := tx.Create(&rc).Error; err != nil {
		return err
	}
	// 若改签单仍在非终态，置为失败并释放新座位（原票从未被改动）。
	if e.Status == models.ExCreated || e.Status == models.ExPaying {
		if err := s.failExchange(tx, e, reason); err != nil {
			return err
		}
	}
	return tx.Model(&models.Receipt{}).Where("id = ?", receiptID).Updates(map[string]interface{}{
		"applied": true,
		"note":    reason,
	}).Error
}

// applyRefundReceipt 登记外部退款回执（幂等），不改变改签状态。
func (s *ExchangeService) applyRefundReceipt(tx *gorm.DB, e *models.Exchange, in ReceiptInput) (string, error) {
	var p models.Payment
	err := tx.Where("payment_no = ? AND exchange_id = ? AND type = ?", in.PaymentNo, e.ID, "refund").First(&p).Error
	if err != nil {
		return "", fmt.Errorf("%w: refund payment %s", ErrNotFound, in.PaymentNo)
	}
	if in.Amount != p.Amount {
		return "", ErrAmountMismatch
	}
	if p.Status == models.PaySuccess {
		return "ignored_stale", nil
	}
	if err := tx.Model(&p).Update("status", models.PaySuccess).Error; err != nil {
		return "", err
	}
	return "refund_recorded", nil
}

// ---------------- 终态动作 ----------------

// finalizeSuccess 在当前事务内完成改签（调用方必须已确认预留有效、状态允许）。
// CAS 保证只有一个执行者能成功——重复回调/并发确认最多一次。
func (s *ExchangeService) finalizeSuccess(tx *gorm.DB, e *models.Exchange) error {
	from := e.Status
	if from != models.ExCreated && from != models.ExPaying {
		return ErrExchangeState
	}
	now := s.now()

	// 状态 CAS：created/paying → succeeded，并释放“同票唯一在途改签”守卫。
	res := tx.Model(&models.Exchange{}).
		Where("id = ? AND status IN ?", e.ID, []string{models.ExCreated, models.ExPaying}).
		Updates(map[string]interface{}{"status": models.ExSucceeded, "active_ticket_key": nil, "paid_at": now})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		// 已被并发处理（成功/失败/过期）——绝不能重复放行。
		return ErrExchangeState
	}

	var old models.Ticket
	if err := tx.First(&old, e.TicketID).Error; err != nil {
		return err
	}
	if old.Status != models.TicketStatusIssued {
		// 原票已不有效，防御性回滚（正常不会发生）。
		return ErrTicketNotActive
	}

	// 1) 新订单（与原购票订单解耦；同行单的其余票仍挂在原订单）。
	order := models.Order{
		OrderNo:       genNo("ORD"),
		MemberID:      e.MemberID,
		ParentOrderID: &old.OrderID,
		Status:        models.OrderStatusPaid,
		PayAmount:     e.NewPaid,
	}
	if err := tx.Create(&order).Error; err != nil {
		return err
	}

	// 2) 新票，完整写入优惠依据（沿用原券快照 或 新券快照）。
	nt := models.Ticket{
		TicketNo:   genNo("TKT"),
		OrderID:    order.ID,
		MemberID:   e.MemberID,
		SessionID:  e.NewSessionID,
		SeatID:     e.NewSeatID,
		Status:     models.TicketStatusIssued,
		BasePrice:  e.NewBasePrice,
		Discount:   e.NewDiscount,
		PaidAmount: e.NewPaid,
	}
	reissFrom := old.ID
	nt.ReissuedFrom = &reissFrom
	if e.NewCouponID != nil {
		cid := *e.NewCouponID
		nt.CouponID = &cid
		var c models.Coupon
		if err := tx.First(&c, cid).Error; err == nil {
			nt.CouponName, nt.CouponType, nt.CouponRate, nt.CouponAmount = c.Name, c.Type, c.Rate, c.Amount
		} else {
			nt.CouponName = e.NewCouponSnapshot
		}
	} else if old.CouponID != nil && e.NewDiscount > 0 {
		// 沿用原券：优惠依据从原票原样复制。
		cid := *old.CouponID
		nt.CouponID = &cid
		nt.CouponName, nt.CouponType, nt.CouponRate, nt.CouponAmount = old.CouponName, old.CouponType, old.CouponRate, old.CouponAmount
	}
	if err := tx.Create(&nt).Error; err != nil {
		return err
	}

	// 3) 原票归档（数据保留，状态 exchanged，永不重复放行）。
	if err := tx.Model(&models.Ticket{}).Where("id = ? AND status = ?", old.ID, models.TicketStatusIssued).
		Updates(map[string]interface{}{"status": models.TicketStatusExchanged}).Error; err != nil {
		return err
	}

	// 4) 预留→已支付，占用权从 reservation 转为新票（同一物理行，改 holder）。
	if err := tx.Model(&models.Reservation{}).
		Where("id = ? AND status = ?", e.ReservationID, models.ReservationHeld).
		Update("status", models.ReservationPaid).Error; err != nil {
		return err
	}
	occRes := tx.Model(&models.SeatOccupancy{}).
		Where("session_id = ? AND seat_id = ? AND holder_type = ? AND holder_id = ?",
			e.NewSessionID, e.NewSeatID, HolderReservation, e.ReservationID).
		Updates(map[string]interface{}{"holder_type": HolderTicket, "holder_id": nt.ID})
	if occRes.Error != nil {
		return occRes.Error
	}
	if occRes.RowsAffected == 0 {
		// 预留占用行不见了（被过期流程删掉）——不应发生（前面已确保 live），防御性中止。
		return ErrReservationExpired
	}

	// 5) 释放原座位（支付成功后才释放）。
	if err := tx.Where("session_id = ? AND seat_id = ? AND holder_type = ? AND holder_id = ?",
		e.OldSessionID, e.OldSeatID, HolderTicket, old.ID).
		Delete(&models.SeatOccupancy{}).Error; err != nil {
		return err
	}

	// 6) 写回新票 ID。
	if err := tx.Model(&models.Exchange{}).Where("id = ?", e.ID).Update("new_ticket_id", nt.ID).Error; err != nil {
		return err
	}

	// 7) 退差：生成自动退款单（成功）。退款回执幂等，迟到/重复不影响改签。
	if e.DiffAmount < 0 {
		refund := models.Payment{
			PaymentNo:  genNo("RFD"),
			ExchangeID: e.ID,
			Amount:     -e.DiffAmount,
			Type:       "refund",
			Status:     models.PaySuccess,
			Channel:    "mock",
		}
		if err := tx.Create(&refund).Error; err != nil {
			return err
		}
		rc := models.Receipt{
			ReceiptNo:  genNo("RCP"),
			PaymentNo:  refund.PaymentNo,
			ExchangeNo: e.ExchangeNo,
			Kind:       "refund",
			Status:     "success",
			Amount:     refund.Amount,
			Applied:    true,
			Note:       "auto refund of price difference",
		}
		if err := tx.Create(&rc).Error; err != nil {
			return err
		}
	}

	e.Status = models.ExSucceeded
	return nil
}

// failExchange 支付失败/取消/过期：保留原订单原座位，释放新座位，归还被占用的券。
func (s *ExchangeService) failExchange(tx *gorm.DB, e *models.Exchange, reason string) error {
	res := tx.Model(&models.Exchange{}).
		Where("id = ? AND status IN ?", e.ID, []string{models.ExCreated, models.ExPaying}).
		Updates(map[string]interface{}{
			"status":            models.ExFailed,
			"active_ticket_key": nil,
			"fail_reason":       reason,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return nil // 已终态，什么都不做
	}
	if err := tx.Model(&models.Reservation{}).
		Where("id = ? AND status = ?", e.ReservationID, models.ReservationHeld).
		Update("status", models.ReservationCanceled).Error; err != nil {
		return err
	}
	if err := tx.Where("session_id = ? AND seat_id = ? AND holder_type = ? AND holder_id = ?",
		e.NewSessionID, e.NewSeatID, HolderReservation, e.ReservationID).
		Delete(&models.SeatOccupancy{}).Error; err != nil {
		return err
	}
	if e.CouponCaptured && e.NewCouponID != nil {
		if err := tx.Model(&models.Coupon{}).Where("id = ?", *e.NewCouponID).
			Update("redeemed_at", nil).Error; err != nil {
			return err
		}
	}
	e.Status = models.ExFailed
	return nil
}

// expireOne 将单个进行中的改签单置过期（调用前已确认预留超时）。
func (s *ExchangeService) expireOne(tx *gorm.DB, e *models.Exchange) error {
	if e.Status != models.ExCreated && e.Status != models.ExPaying {
		return nil
	}
	res := tx.Model(&models.Exchange{}).
		Where("id = ? AND status IN ?", e.ID, []string{models.ExCreated, models.ExPaying}).
		Updates(map[string]interface{}{"status": models.ExExpired, "active_ticket_key": nil, "fail_reason": "reservation expired"})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return nil
	}
	if err := tx.Model(&models.Reservation{}).
		Where("id = ? AND status = ?", e.ReservationID, models.ReservationHeld).
		Update("status", models.ReservationExpired).Error; err != nil {
		return err
	}
	if err := tx.Where("session_id = ? AND seat_id = ? AND holder_type = ? AND holder_id = ?",
		e.NewSessionID, e.NewSeatID, HolderReservation, e.ReservationID).
		Delete(&models.SeatOccupancy{}).Error; err != nil {
		return err
	}
	if e.CouponCaptured && e.NewCouponID != nil {
		if err := tx.Model(&models.Coupon{}).Where("id = ?", *e.NewCouponID).
			Update("redeemed_at", nil).Error; err != nil {
			return err
		}
	}
	e.Status = models.ExExpired
	return nil
}

// ExpireOnce 扫描超时预留并释放（供定时器和测试调用）。返回处理条数。
func (s *ExchangeService) ExpireOnce() (int, error) {
	n := 0
	var ids []uint
	if err := s.db.Model(&models.Reservation{}).
		Where("status = ? AND expire_at <= ?", models.ReservationHeld, s.now()).
		Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	for _, id := range ids {
		err := s.writeTx(func(tx *gorm.DB) error {
			var r models.Reservation
			if err := tx.First(&r, id).Error; err != nil {
				return err
			}
			if r.Status != models.ReservationHeld || r.ExpireAt.After(s.now()) {
				return nil
			}
			if r.ExchangeID == nil {
				tx.Model(&r).Update("status", models.ReservationExpired)
				tx.Where("session_id = ? AND seat_id = ? AND holder_type = ? AND holder_id = ?",
					r.SessionID, r.SeatID, HolderReservation, r.ID).Delete(&models.SeatOccupancy{})
				n++
				return nil
			}
			var e models.Exchange
			if err := tx.First(&e, *r.ExchangeID).Error; err != nil {
				return err
			}
			if err := s.expireOne(tx, &e); err != nil {
				return err
			}
			n++
			return nil
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func casExchange(tx *gorm.DB, id uint, from, to string, extra map[string]interface{}) error {
	upd := map[string]interface{}{"status": to}
	for k, v := range extra {
		upd[k] = v
	}
	res := tx.Model(&models.Exchange{}).Where("id = ? AND status = ?", id, from).Updates(upd)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrExchangeState
	}
	return nil
}

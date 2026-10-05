package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

var (
	ErrSeatTaken      = errors.New("座位已被占用或预留")
	ErrSeatIllegal    = errors.New("座位不存在")
	ErrTicketIllegal  = errors.New("原票不可改签")
	ErrShowNotFound   = errors.New("目标场次不存在")
	ErrSameSeat       = errors.New("新座位与原票一致，无需改签")
	ErrInFlight       = errors.New("该票已有进行中的改签，请先完成或等待超时")
	ErrReceiptReplayed = errors.New("回执已处理，请勿重复提交")
	ErrReceiptUnknown  = errors.New("回执与当前待结算方向不符")
)

type Service struct {
	db    *gorm.DB
	ttl   time.Duration
	mu    sync.Mutex // 结算串行化（SQLite 写亦为单连接，双保险）
	now   func() time.Time
}

func NewService(db *gorm.DB, ttl time.Duration) *Service {
	return &Service{db: db, ttl: ttl, now: time.Now}
}

func seatCode(row, col int) string {
	return fmt.Sprintf("%c%d", 'A'+row-1, col)
}

// Preview 只计价、不占位。
func (s *Service) Preview(memberID, ticketID, newShowID int64) (*Reservation, *Ticket, *Showtime, error) {
	old, show, err := s.loadContext(memberID, ticketID, newShowID)
	if err != nil {
		return nil, nil, nil, err
	}
	r := &Reservation{}
	r.OldFacePrice, r.OldDiscountAmount, r.OldPaidAmount = old.FacePrice, old.DiscountAmount, old.PaidAmount
	r.NewFacePrice, r.NewDiscountAmount, r.NewPaidAmount, r.Delta, _ = quote(old, show)
	if b := parseBasis(old.DiscountBasis); b != nil {
		r.DiscountBasis = mustJSON(b)
	}
	r.CouponID = old.CouponID
	return r, old, show, nil
}

func (s *Service) loadContext(memberID, ticketID, newShowID int64) (*Ticket, *Showtime, error) {
	var old Ticket
	if err := s.db.First(&old, ticketID).Error; err != nil {
		return nil, nil, ErrTicketIllegal
	}
	if old.MemberID != memberID || old.Status != TicketActive {
		return nil, nil, ErrTicketIllegal
	}
	var show Showtime
	if err := s.db.First(&show, newShowID).Error; err != nil {
		return nil, nil, ErrShowNotFound
	}
	if show.ID == old.ShowtimeID {
		return nil, nil, errors.New("目标场次与原场次相同")
	}
	return &old, &show, nil
}

// Reserve 限时预占新座位并生成改签单。幂等：相同 idempotencyKey 返回原单。
func (s *Service) Reserve(memberID, ticketID, newShowID int64, row, col int, idemKey string) (*Reservation, error) {
	if strings.TrimSpace(idemKey) == "" {
		idemKey = fmt.Sprintf("reserve-%d-%d-%d-%d-%d", ticketID, newShowID, row, col, s.now().UnixNano())
	}
	old, show, err := s.loadContext(memberID, ticketID, newShowID)
	if err != nil {
		return nil, err
	}
	if row < 1 || row > show.Rows || col < 1 || col > show.Cols {
		return nil, ErrSeatIllegal
	}
	code := seatCode(row, col)
	if show.ID == old.ShowtimeID && code == old.SeatCode {
		return nil, ErrSameSeat
	}

	var r Reservation
	err = s.db.Transaction(func(tx *gorm.DB) error {
		// 幂等键复用
		if e := tx.Where("idempotency_key = ?", idemKey).First(&r).Error; e == nil {
			return nil
		} else if !errors.Is(e, gorm.ErrRecordNotFound) {
			return e
		}
		// 同一原票同时只允许一个进行中的预留
		var cnt int64
		if e := tx.Model(&Reservation{}).
			Where("old_ticket_id = ? AND status = ? AND expires_at > ?", ticketID, ResvReserved, s.now()).
			Count(&cnt).Error; e != nil {
			return e
		}
		if cnt > 0 {
			return ErrInFlight
		}
		// 原子占位：部分唯一索引保证同一时刻只有一个 held/sold
		lock := SeatLock{ShowtimeID: show.ID, SeatCode: code, Status: LockHeld}
		if e := tx.Create(&lock).Error; e != nil {
			if isUniqueViolation(e) {
				return ErrSeatTaken
			}
			return e
		}
		newFace, newDisc, newPaid, delta, basis := quote(old, show)
		r = Reservation{
			ReservationNo:    fmt.Sprintf("R%d", s.now().UnixNano()/1000),
			IdempotencyKey:   idemKey,
			MemberID:         memberID,
			OrderID:          old.OrderID,
			OldTicketID:      old.ID,
			NewShowtimeID:    show.ID,
			NewSeatRow:       row,
			NewSeatCol:       col,
			NewSeatCode:      code,
			SeatLockID:       lock.ID,
			OldFacePrice:     old.FacePrice,
			OldDiscountAmount: old.DiscountAmount,
			OldPaidAmount:    old.PaidAmount,
			NewFacePrice:     newFace,
			NewDiscountAmount: newDisc,
			NewPaidAmount:    newPaid,
			CouponID:         old.CouponID,
			DiscountBasis:    basisJSON(basis),
			Delta:            delta,
			Status:           ResvReserved,
			ExpiresAt:        s.now().Add(s.ttl),
		}
		if e := tx.Create(&r).Error; e != nil {
			return e
		}
		// 按 delta 方向建唯一一条待结算支付单（delta=0 记 0 元补款）
		direction := PayDirectionPay
		amount := delta
		if delta < 0 {
			direction = PayDirectionRefund
			amount = -delta
		}
		pay := Payment{ReservationID: r.ID, Direction: direction, Amount: amount, Status: PayPending}
		return tx.Create(&pay).Error
	})
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func basisJSON(b *DiscountBasis) string {
	if b == nil {
		return ""
	}
	return mustJSON(b)
}

// ReceiptResult 结算结果。Replayed=true 表示重复回执未触发任何状态变化。
type ReceiptResult struct {
	Reservation *Reservation
	Action      string // completed | failed | expired_late_refunded | expired_noted | replayed
	Replayed    bool
}

// Settle 处理支付/退款回执。成功才完成改签；失败释放新座保留原票；
// 超时后迟到的成功补款自动退款、绝不二次放行；重复回执幂等。
func (s *Service) Settle(reservationNo string, success bool, receiptNo string) (*ReceiptResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	res := &ReceiptResult{}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var r Reservation
		if e := tx.Where("reservation_no = ?", reservationNo).First(&r).Error; e != nil {
			return fmt.Errorf("改签单不存在")
		}
		direction := PayDirectionPay
		if r.Delta < 0 {
			direction = PayDirectionRefund
		}
		var pay Payment
		if e := tx.Where("reservation_id = ? AND direction = ?", r.ID, direction).First(&pay).Error; e != nil {
			return e
		}

		// 终态单：任何回执都不再触发改签
		if r.Status == ResvCompleted || r.Status == ResvFailed {
			res.Replayed = true
			res.Action = "replayed"
			res.Reservation = &r
			// 已成功的支付单收到“另一张”回执号 → 明确拒绝，便于排查
			if pay.Status == PaySucceeded && receiptNo != "" && pay.ReceiptNo != nil && *pay.ReceiptNo != receiptNo {
				return ErrReceiptReplayed
			}
			return nil
		}

		targetStatus := PaySucceeded
		if !success {
			targetStatus = PayFailed
		}

		if r.Status == ResvExpired {
			// 预留已超时：座位已释放、原票保留。迟到回执只能记账。
			mark := map[string]any{"status": targetStatus, "note": "预留超时后到达的回执"}
			if receiptNo != "" {
				mark["receipt_no"] = receiptNo
			}
			if e := tx.Model(&Payment{}).
				Where("id = ? AND status = ?", pay.ID, PayPending).Updates(mark).Error; e != nil {
				return e
			}
			if success {
				// 钱已收到/退款已打出，补一条反向自动退款（本地模拟即时成功），绝不完成改签
				autoDirection := PayDirectionRefund
				note := "预留超时后补款迟到，自动原路退回"
				if direction == PayDirectionRefund {
					autoDirection = PayDirectionPay
					note = "预留超时后退款回执迟到，异常待人工处理"
				}
				auto := Payment{ReservationID: r.ID, Direction: autoDirection, Amount: pay.Amount,
					Status: PaySucceeded, Note: note}
				if receiptNo != "" {
					rc := "AUTO-" + receiptNo
					auto.ReceiptNo = &rc
				}
				if e := tx.Create(&auto).Error; e != nil && !isUniqueViolation(e) {
					return e
				}
				tx.Model(&Reservation{}).Where("id = ?", r.ID).Update("note", note)
				res.Action = "expired_late_refunded"
			} else {
				res.Action = "expired_noted"
			}
			res.Reservation = &r
			return nil
		}

		// r.Status == reserved
		// 支付单条件更新：只有 pending 能迁移，挡住并发/重复回执
		upd := map[string]any{"status": targetStatus}
		if receiptNo != "" {
			upd["receipt_no"] = receiptNo
		}
		out := tx.Model(&Payment{}).Where("id = ? AND status = ?", pay.ID, PayPending).Updates(upd)
		if out.Error != nil {
			return out.Error
		}
		if out.RowsAffected == 0 {
			// 并发下另一回执已先行处理：当作重复，不再动作
			res.Replayed = true
			res.Action = "replayed"
			res.Reservation = &r
			return nil
		}

		if !success {
			// 补款/退款失败：释放新座位，原订单与原座位保持不变
			if e := tx.Model(&SeatLock{}).
				Where("id = ? AND status = ?", r.SeatLockID, LockHeld).
				Update("status", LockReleased).Error; e != nil {
				return e
			}
			if e := tx.Model(&Reservation{}).
				Where("id = ? AND status = ?", r.ID, ResvReserved).
				Update("status", ResvFailed).Error; e != nil {
				return e
			}
			r.Status = ResvFailed
			res.Action = "failed"
			res.Reservation = &r
			return nil
		}

		// 成功：出票新票 -> 旧票 exchanged -> 新座 sold -> 旧座 released，单事务完成
		basis := r.DiscountBasis
		newTicket := Ticket{
			OrderID:        r.OrderID,
			MemberID:       r.MemberID,
			ShowtimeID:     r.NewShowtimeID,
			SeatRow:        r.NewSeatRow,
			SeatCol:        r.NewSeatCol,
			SeatCode:       r.NewSeatCode,
			FacePrice:      r.NewFacePrice,
			DiscountAmount: r.NewDiscountAmount,
			PaidAmount:     r.NewPaidAmount,
			CouponID:       r.CouponID,
			DiscountBasis:  basis,
			Status:         TicketActive,
			ReservationID:  &r.ID,
		}
		if e := tx.Create(&newTicket).Error; e != nil {
			return e
		}
		// 券抵扣按票迁移：券保持 used，优惠依据跟随新票，不发生第二次核销
		if r.CouponID != nil {
			if e := tx.Model(&Coupon{}).Where("id = ?", *r.CouponID).
				Update("current_ticket_id", newTicket.ID).Error; e != nil {
				return e
			}
		}
		if e := tx.Model(&Ticket{}).Where("id = ? AND status = ?", r.OldTicketID, TicketActive).
			Updates(map[string]any{"status": TicketExchanged, "exchanged_to_id": newTicket.ID}).Error; e != nil {
			return e
		}
		if e := tx.Model(&SeatLock{}).Where("id = ? AND status = ?", r.SeatLockID, LockHeld).
			Updates(map[string]any{"status": LockSold, "ticket_id": newTicket.ID}).Error; e != nil {
			return e
		}
		// 释放原座位
		if e := tx.Model(&SeatLock{}).
			Where("showtime_id = (SELECT showtime_id FROM tickets WHERE id = ?) AND seat_code = (SELECT seat_code FROM tickets WHERE id = ?) AND status = ?",
				r.OldTicketID, r.OldTicketID, LockSold).
			Update("status", LockReleased).Error; e != nil {
			return e
		}
		if e := tx.Model(&Reservation{}).
			Where("id = ? AND status = ?", r.ID, ResvReserved).
			Updates(map[string]any{"status": ResvCompleted, "new_ticket_id": newTicket.ID}).Error; e != nil {
			return e
		}
		r.Status = ResvCompleted
		r.NewTicketID = &newTicket.ID
		res.Action = "completed"
		res.Reservation = &r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// ExpireDue 超时回收：reserved -> expired 并释放仍为 held 的新座位。
func (s *Service) ExpireDue() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	expired := 0
	var due []Reservation
	if err := s.db.Where("status = ? AND expires_at < ?", ResvReserved, s.now()).Find(&due).Error; err != nil {
		return 0, err
	}
	for _, r := range due {
		err := s.db.Transaction(func(tx *gorm.DB) error {
			out := tx.Model(&Reservation{}).
				Where("id = ? AND status = ?", r.ID, ResvReserved).Update("status", ResvExpired)
			if out.Error != nil {
				return out.Error
			}
			if out.RowsAffected == 0 {
				return nil // 已被结算或其他回收处理
			}
			return tx.Model(&SeatLock{}).
				Where("id = ? AND status = ?", r.SeatLockID, LockHeld).
				Update("status", LockReleased).Error
		})
		if err != nil {
			return expired, err
		}
		expired++
	}
	return expired, nil
}

func isUniqueViolation(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "UNIQUE constraint") ||
		strings.Contains(err.Error(), "Duplicate entry"))
}

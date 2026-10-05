package main

// 所有金额一律使用 int64 「分」存储，避免浮点误差。

import "time"

type Movie struct {
	ID    int64  `gorm:"primaryKey" json:"id"`
	Title string `gorm:"not null" json:"title"`
	Genre string `json:"genre"`
}

type Showtime struct {
	ID        int64     `gorm:"primaryKey" json:"id"`
	MovieID   int64     `gorm:"not null;index" json:"movie_id"`
	Movie     *Movie    `gorm:"foreignKey:MovieID" json:"movie,omitempty"`
	Hall      string    `gorm:"not null" json:"hall"`
	StartAt   time.Time `gorm:"not null" json:"start_at"`
	BasePrice int64     `gorm:"not null" json:"base_price"` // 全场统一票价（分）
	Rows      int       `gorm:"not null" json:"rows"`
	Cols      int       `gorm:"not null" json:"cols"`
}

const (
	OrderActive = "active"
)

type Order struct {
	ID        int64     `gorm:"primaryKey" json:"id"`
	OrderNo   string    `gorm:"size:32;uniqueIndex" json:"order_no"`
	MemberID  int64     `gorm:"not null;index" json:"member_id"`
	Status    string    `gorm:"not null;default:active" json:"status"` // active：同行票始终保留
	CreatedAt time.Time `json:"created_at"`
}

const (
	TicketActive     = "active"     // 有效票
	TicketExchanged  = "exchanged"  // 已改签（旧票终态）
	TicketRefunded   = "refunded"   // 已退票
)

// DiscountBasis 是优惠依据快照，随票保存，改签时按同一依据在新票价上重算抵扣。
type DiscountBasis struct {
	CouponCode  string `json:"coupon_code"`
	Kind        string `json:"kind"` // percent：折扣率（Value=90 表示 9 折）；fixed：立减（分）
	Value       int64  `json:"value"`
	MinSpend    int64  `json:"min_spend"` // fixed 券可用的最低票面价
	Description string `json:"description"`
}

type Ticket struct {
	ID             int64   `gorm:"primaryKey" json:"id"`
	OrderID        int64   `gorm:"not null;index" json:"order_id"`
	MemberID       int64   `gorm:"not null;index" json:"member_id"`
	ShowtimeID     int64   `gorm:"not null;index" json:"showtime_id"`
	Showtime       *Showtime `gorm:"foreignKey:ShowtimeID" json:"showtime,omitempty"`
	SeatRow        int     `gorm:"not null" json:"seat_row"`
	SeatCol        int     `gorm:"not null" json:"seat_col"`
	SeatCode       string  `gorm:"size:8;not null" json:"seat_code"`
	FacePrice      int64   `gorm:"not null" json:"face_price"`       // 票面价
	DiscountAmount int64   `gorm:"not null;default:0" json:"discount_amount"` // 该票券抵扣
	PaidAmount     int64   `gorm:"not null" json:"paid_amount"`     // 实付 = 票面 - 抵扣
	CouponID       *int64  `gorm:"index" json:"coupon_id,omitempty"`
	DiscountBasis  string  `gorm:"type:text" json:"discount_basis"` // 优惠依据 JSON 快照
	Status         string  `gorm:"not null;default:active;index" json:"status"`
	ExchangedToID  *int64  `json:"exchanged_to_id,omitempty"` // 改签后新票
	ReservationID  *int64  `gorm:"index" json:"reservation_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

const (
	CouponAvailable = "available"
	CouponUsed      = "used"
	CouponExpired   = "expired"
)

type Coupon struct {
	ID             int64      `gorm:"primaryKey" json:"id"`
	Code           string     `gorm:"size:32;uniqueIndex" json:"code"`
	MemberID       int64      `gorm:"not null;index" json:"member_id"`
	Name           string     `json:"name"`
	Kind           string     `gorm:"not null" json:"kind"` // percent / fixed
	Value          int64      `gorm:"not null" json:"value"`
	MinSpend       int64      `gorm:"not null;default:0" json:"min_spend"`
	Status         string     `gorm:"not null;default:available;index" json:"status"`
	CurrentTicketID *int64    `json:"current_ticket_id,omitempty"` // 已使用券当前挂在哪张活票上（改签时迁移）
	ExpireAt       time.Time  `json:"expire_at"`
	CreatedAt      time.Time  `json:"created_at"`
}

const (
	LockHeld     = "held"     // 改签新座位限时预留
	LockSold     = "sold"     // 已出票
	LockReleased = "released" // 已释放
)

// SeatLock 座位占用。部分唯一索引保证同一时刻同场次同座位至多一个 held/sold 锁。
type SeatLock struct {
	ID            int64     `gorm:"primaryKey" json:"id"`
	ShowtimeID    int64     `gorm:"not null;index:idx_seat_lookup,unique" json:"showtime_id"`
	SeatCode      string    `gorm:"size:8;not null;index:idx_seat_lookup,unique" json:"seat_code"`
	Status        string    `gorm:"not null;default:held;index:idx_seat_lookup,unique" json:"status"`
	TicketID      *int64    `json:"ticket_id,omitempty"`
	ReservationID *int64    `json:"reservation_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

const (
	ResvReserved  = "reserved"  // 已预留座位，等待支付/退款回执
	ResvCompleted = "completed" // 改签完成（终态）
	ResvFailed    = "failed"    // 补款失败，新座位释放、原票保留（终态）
	ResvExpired   = "expired"   // 预留超时（终态；晚到的成功补款会自动退款）
)

type Reservation struct {
	ID              int64     `gorm:"primaryKey" json:"id"`
	ReservationNo   string    `gorm:"size:32;uniqueIndex" json:"reservation_no"`
	IdempotencyKey  string    `gorm:"size:64;uniqueIndex" json:"idempotency_key"`
	MemberID        int64     `gorm:"not null;index" json:"member_id"`
	OrderID         int64     `gorm:"not null" json:"order_id"`
	OldTicketID     int64     `gorm:"not null" json:"old_ticket_id"`
	NewShowtimeID   int64     `gorm:"not null" json:"new_showtime_id"`
	NewSeatRow      int       `json:"new_seat_row"`
	NewSeatCol      int       `json:"new_seat_col"`
	NewSeatCode     string    `gorm:"size:8;not null" json:"new_seat_code"`
	SeatLockID      int64     `json:"not null" json:"seat_lock_id"`

	OldFacePrice      int64 `gorm:"not null" json:"old_face_price"`
	OldDiscountAmount int64 `gorm:"not null" json:"old_discount_amount"`
	OldPaidAmount     int64 `gorm:"not null" json:"old_paid_amount"`
	NewFacePrice      int64 `gorm:"not null" json:"new_face_price"`
	NewDiscountAmount int64 `gorm:"not null" json:"new_discount_amount"`
	NewPaidAmount     int64 `gorm:"not null" json:"new_paid_amount"`
	CouponID          *int64 `json:"coupon_id,omitempty"`
	DiscountBasis     string `gorm:"type:text" json:"discount_basis"`
	// Delta > 0 会员需补交；< 0 需退回；= 0 平
	Delta int64 `gorm:"not null" json:"delta"`

	Status    string    `gorm:"not null;default:reserved;index" json:"status"`
	ExpiresAt time.Time `gorm:"not null" json:"expires_at"`
	NewTicketID *int64  `json:"new_ticket_id,omitempty"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

const (
	PayDirectionPay    = "pay"    // 补款
	PayDirectionRefund = "refund" // 退款
	PayPending         = "pending"
	PaySucceeded       = "succeeded"
	PayFailed          = "failed"
)

// Payment 一笔预留对应每个方向至多一条支付单（部分唯一索引）。
// 状态只能 pending -> succeeded/failed 一次，重复回执通过条件 UPDATE 被挡下。
type Payment struct {
	ID            int64     `gorm:"primaryKey" json:"id"`
	ReservationID int64     `gorm:"not null;uniqueIndex:idx_pay_resv_dir,priority:1" json:"reservation_id"`
	Direction     string    `gorm:"size:8;not null;uniqueIndex:idx_pay_resv_dir,priority:2" json:"direction"`
	Amount        int64     `gorm:"not null" json:"amount"`
	Status        string    `gorm:"not null;default:pending" json:"status"`
	ReceiptNo     *string   `gorm:"uniqueIndex" json:"receipt_no,omitempty"`
	Note          string    `json:"note"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

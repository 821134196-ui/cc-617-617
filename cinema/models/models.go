package models

import (
	"time"

	"gorm.io/gorm"
)

// 金额单位统一为“分”，避免浮点误差。

// Member 会员
type Member struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	Name      string `gorm:"not null" json:"name"`
	Phone     string `gorm:"uniqueIndex;not null" json:"phone"`
	CreatedAt time.Time
}

// Movie 影片
type Movie struct {
	ID    uint   `gorm:"primaryKey" json:"id"`
	Title string `gorm:"not null" json:"title"`
}

// Session 场次
type Session struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	MovieID   uint      `gorm:"not null;index" json:"movie_id"`
	Movie     *Movie    `gorm:"foreignKey:MovieID" json:"movie,omitempty"`
	Hall      string    `gorm:"not null" json:"hall"`
	StartAt   time.Time `gorm:"not null;index" json:"start_at"`
	BasePrice int64     `gorm:"not null" json:"base_price"` // 基础票价(分)
	CreatedAt time.Time
}

// Seat 物理座位（每个场次独立排座，简化为场次内座位）
type Seat struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	SessionID uint   `gorm:"not null;uniqueIndex:uk_session_seat" json:"session_id"`
	Row       string `gorm:"not null;uniqueIndex:uk_session_seat" json:"row"`
	Number    int    `gorm:"not null;uniqueIndex:uk_session_seat" json:"number"`
	// 座位固定状态只表示“是否物理存在/禁用”，不表示售卖状态；
	// 售卖状态由 Ticket / Reservation 推导。
	Disabled bool `json:"disabled"`
}

// Coupon 会员券
type Coupon struct {
	ID         uint       `gorm:"primaryKey" json:"id"`
	MemberID   uint       `gorm:"not null;index" json:"member_id"`
	Name       string     `gorm:"not null" json:"name"`
	Type       string     `gorm:"not null" json:"type"` // discount: 折扣券(rate); amount: 立减券(amount)
	Rate       int        `json:"rate"`                 // 折扣，如 80 = 8折，用于 discount
	Amount     int64      `json:"amount"`               // 立减金额(分)，用于 amount
	ExpireAt   time.Time  `gorm:"not null" json:"expire_at"`
	RedeemedAt *time.Time `json:"redeemed_at"` // 核销时间；非空 = 已使用
	CreatedAt  time.Time
}

// 订单与票状态
const (
	OrderStatusPaid = "paid" // 已支付、正常观影
	OrderStatusDone = "done" // 已完成（改签后原票归档）
	OrderStatusVoid = "void" // 支付失败/取消（预占位作废）
)

const (
	TicketStatusIssued    = "issued"    // 有效票
	TicketStatusExchanged = "exchanged" // 已改签，原票归档
	TicketStatusVoid      = "void"      // 作废
)

// Order 订单（一次购票或改签支付）
type Order struct {
	ID       uint   `gorm:"primaryKey" json:"id"`
	OrderNo  string `gorm:"uniqueIndex;size:40;not null" json:"order_no"`
	MemberID uint   `gorm:"not null;index" json:"member_id"`
	// ParentOrderID: 同行单。一个会员一次买多张票属于同一购票订单；
	// 改签单张票时新开 Exchange，原订单保留，其余票不动。
	ParentOrderID *uint  `gorm:"index" json:"parent_order_id,omitempty"`
	Status        string `gorm:"size:16;not null;index" json:"status"`
	PayAmount     int64  `gorm:"not null" json:"pay_amount"` // 该订单实付(分)
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Ticket 票。券抵扣按每张票记录，原始优惠依据永久保留在本行。
type Ticket struct {
	ID           uint   `gorm:"primaryKey" json:"id"`
	TicketNo     string `gorm:"uniqueIndex;size:40;not null" json:"ticket_no"`
	OrderID      uint   `gorm:"not null;index" json:"order_id"`
	MemberID     uint   `gorm:"not null;index" json:"member_id"`
	SessionID    uint   `gorm:"not null;index" json:"session_id"`
	SeatID       uint   `gorm:"not null;index" json:"seat_id"`
	Status       string `gorm:"size:16;not null;index" json:"status"`
	BasePrice    int64  `gorm:"not null" json:"base_price"` // 购票时场次基础票价快照
	CouponID     *uint  `gorm:"index" json:"coupon_id,omitempty"`
	CouponName   string `json:"coupon_name"` // 券快照
	CouponType   string `json:"coupon_type"`
	CouponRate   int    `json:"coupon_rate"`
	CouponAmount int64  `json:"coupon_amount"`
	Discount     int64  `gorm:"not null" json:"discount"`    // 本张票券抵扣金额(分)
	PaidAmount   int64  `gorm:"not null" json:"paid_amount"` // 本张票实付 = base_price - discount
	// ReissuedFrom: 改签新票指向原票；原票 status=exchanged 但数据完整保留。
	ReissuedFrom *uint `gorm:"index" json:"reissued_from,omitempty"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// 预留状态
const (
	ReservationHeld     = "held"
	ReservationPaid     = "paid"
	ReservationExpired  = "expired"
	ReservationCanceled = "canceled" // 改签失败/放弃
)

// Reservation 选座后的限时预留（改签新座位用；普通购票同表也可复用）
type Reservation struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	ReserveNo string `gorm:"uniqueIndex;size:40;not null" json:"reserve_no"`
	MemberID  uint   `gorm:"not null;index" json:"member_id"`
	SessionID uint   `gorm:"not null;index" json:"session_id"`
	SeatID    uint   `gorm:"not null;index" json:"seat_id"`
	// TicketID: 被改签的原票；普通购票为空。
	TicketID *uint `gorm:"index" json:"ticket_id,omitempty"`
	// ExchangeID: 关联改签申请；购票为空。
	ExchangeID *uint     `gorm:"index" json:"exchange_id,omitempty"`
	Status     string    `gorm:"size:16;not null;index" json:"status"`
	ExpireAt   time.Time `gorm:"not null;index" json:"expire_at"`
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// SeatOccupancy 座位占用权威表：一行 = 该座位在一场次中被“有效票”或“有效预留”占用。
// 靠 (session_id, seat_id) 唯一索引解决并发抢座——两个事务不可能同时插入成功。
type SeatOccupancy struct {
	ID        uint `gorm:"primaryKey" json:"id"`
	SessionID uint `gorm:"not null;uniqueIndex:uk_occupy" json:"session_id"`
	SeatID    uint `gorm:"not null;uniqueIndex:uk_occupy" json:"seat_id"`
	// holder_type: ticket / reservation
	HolderType string `gorm:"size:16;not null" json:"holder_type"`
	HolderID   uint   `gorm:"not null" json:"holder_id"`
	CreatedAt  time.Time
}

// 改签状态机
const (
	ExCreated   = "created"   // 已报价、新座位预留中，待支付
	ExPaying    = "paying"    // 已发起支付
	ExSucceeded = "succeeded" // 支付成功，改签完成
	ExFailed    = "failed"    // 支付失败，原票保留
	ExExpired   = "expired"   // 预留超时
	ExCanceled  = "canceled"
)

// Exchange 改签申请（一次改签一张票；改多张请逐张发起，原单其余票保留）
type Exchange struct {
	ID            uint   `gorm:"primaryKey" json:"id"`
	ExchangeNo    string `gorm:"uniqueIndex;size:40;not null" json:"exchange_no"`
	MemberID      uint   `gorm:"not null;index" json:"member_id"`
	TicketID      uint   `gorm:"not null;index" json:"ticket_id"` // 原票
	OldSessionID  uint   `gorm:"not null" json:"old_session_id"`
	OldSeatID     uint   `gorm:"not null" json:"old_seat_id"`
	NewSessionID  uint   `gorm:"not null" json:"new_session_id"`
	NewSeatID     uint   `gorm:"not null" json:"new_seat_id"`
	ReservationID uint   `gorm:"not null" json:"reservation_id"`
	// 计价快照（确认改签时锁定）
	OldBasePrice      int64  `gorm:"not null" json:"old_base_price"`
	OldDiscount       int64  `gorm:"not null" json:"old_discount"`
	OldPaid           int64  `gorm:"not null" json:"old_paid"`
	NewBasePrice      int64  `gorm:"not null" json:"new_base_price"`
	NewDiscount       int64  `gorm:"not null" json:"new_discount"`
	NewPaid           int64  `gorm:"not null" json:"new_paid"`
	DiffAmount        int64  `gorm:"not null" json:"diff_amount"` // >0 补交, <0 退还, =0 平
	OldCouponID       *uint  `json:"old_coupon_id,omitempty"`
	NewCouponID       *uint  `gorm:"index" json:"new_coupon_id,omitempty"`
	NewCouponSnapshot string `json:"new_coupon_snapshot"`
	// CouponCaptured: 创建改签时是否由本单占用(预核销)了新券；失败/过期需归还。
	CouponCaptured bool   `json:"coupon_captured"`
	Status         string `gorm:"size:16;not null;index" json:"status"`
	// ActiveTicketKey: 非终态时等于原票 ID，终态置 NULL。
	// 唯一索引保证“同一张票同时只能有一笔进行中的改签”（SQLite 唯一索引允许多个 NULL）。
	ActiveTicketKey *uint `gorm:"uniqueIndex" json:"-"`
	// IdempotencyKey: 前端/客户端发起改签时携带，防止重复创建（指针：多个 NULL 不冲突）。
	IdempotencyKey *string    `gorm:"uniqueIndex;size:64" json:"idempotency_key,omitempty"`
	NewTicketID    *uint      `json:"new_ticket_id,omitempty"`
	PaidAt         *time.Time `json:"paid_at,omitempty"`
	FailReason     string     `json:"fail_reason"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// 支付单
const (
	PayPending = "pending"
	PaySuccess = "success"
	PayFailed  = "failed"
)

// Payment 支付单。一笔改签对应一张支付单；支付单只允许成功一次。
type Payment struct {
	ID         uint   `gorm:"primaryKey" json:"id"`
	PaymentNo  string `gorm:"uniqueIndex;size:40;not null" json:"payment_no"`
	ExchangeID uint   `gorm:"not null;index" json:"exchange_id"` // 一笔改签最多两笔：收款 + 自动退款
	Amount     int64  `gorm:"not null" json:"amount"`            // 应补金额；为负时走退款单
	Type       string `gorm:"size:16;not null" json:"type"`      // charge / refund
	Status     string `gorm:"size:16;not null;index" json:"status"`
	Channel    string `gorm:"size:32" json:"channel"`
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Receipt 支付/退款回执。receipt_no 唯一，重复回执直接忽略。
type Receipt struct {
	ID          uint   `gorm:"primaryKey" json:"id"`
	ReceiptNo   string `gorm:"uniqueIndex;size:64;not null" json:"receipt_no"`
	PaymentNo   string `gorm:"size:40;not null;index" json:"payment_no"`
	ExchangeNo  string `gorm:"size:40;not null;index" json:"exchange_no"`
	Kind        string `gorm:"size:16;not null" json:"kind"` // pay_success / pay_failed / refund
	Status      string `gorm:"size:16;not null" json:"status"`
	Amount      int64  `gorm:"not null" json:"amount"`
	DuplicateOf *uint  `json:"duplicate_of,omitempty"`  // 重复回执指向首条
	Applied     bool   `gorm:"not null" json:"applied"` // 是否已作用于订单
	Note        string `json:"note"`
	CreatedAt   time.Time
}

func AutoMigrate(db *gorm.DB) error {
	return db.AutoMigrate(
		&Member{}, &Movie{}, &Session{}, &Seat{}, &Coupon{},
		&Order{}, &Ticket{}, &Reservation{}, &SeatOccupancy{},
		&Exchange{}, &Payment{}, &Receipt{},
	)
}

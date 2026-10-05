package service

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"cinema/models"
	"cinema/storage"
)

// ---------- 测试夹具 ----------

type testFixture struct {
	db  *gorm.DB
	svc *ExchangeService
	now time.Time
}

func newFixture(t *testing.T) *testFixture {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := models.AutoMigrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	f := &testFixture{db: db, now: base}
	svc := NewExchangeService(db, 2*time.Second)
	svc.SetClock(func() time.Time { return f.now })
	f.svc = svc
	return f
}

// advance 推进虚拟时钟。
func (f *testFixture) advance(d time.Duration) { f.now = f.now.Add(d) }

func (f *testFixture) mkMember(name string) uint {
	m := models.Member{Name: name, Phone: "p" + name + timeSkip()}
	if err := f.db.Create(&m).Error; err != nil {
		panic(err)
	}
	return m.ID
}

var memberSeq struct {
	sync.Mutex
	n int
}

func timeSkip() string {
	memberSeq.Lock()
	memberSeq.n++
	n := memberSeq.n
	memberSeq.Unlock()
	return itoa(n)
}

// mkSession 建场次并返回 (sessionID, 座位ID矩阵[row][num])。
func (f *testFixture) mkSession(movie string, price int64) (uint, [][]uint) {
	mov := models.Movie{Title: movie}
	f.db.Create(&mov)
	s := models.Session{MovieID: mov.ID, Hall: "H1", StartAt: f.now.Add(24 * time.Hour), BasePrice: price}
	f.db.Create(&s)
	seats := make([][]uint, 2)
	for r := 0; r < 2; r++ {
		seats[r] = make([]uint, 4)
		for n := 1; n <= 4; n++ {
			st := models.Seat{SessionID: s.ID, Row: string(rune('A' + r)), Number: n}
			f.db.Create(&st)
			seats[r][n-1] = st.ID
		}
	}
	return s.ID, seats
}

// mkCoupon 建券并返回 ID。redeemed/expired 可控。
func (f *testFixture) mkCoupon(memberID uint, name, typ string, rate int, amount int64, redeemed, expired bool) uint {
	c := models.Coupon{MemberID: memberID, Name: name, Type: typ, Rate: rate, Amount: amount,
		ExpireAt: f.now.Add(7 * 24 * time.Hour)}
	if redeemed {
		ts := f.now.Add(-time.Hour)
		c.RedeemedAt = &ts
	}
	if expired {
		c.ExpireAt = f.now.Add(-time.Hour)
	}
	f.db.Create(&c)
	return c.ID
}

// mkTicket 以给定价格/券建一张已售出的有效票（含订单+占用），返回票ID。
func (f *testFixture) mkTicket(memberID, sessionID, seatID uint, basePrice int64, couponID *uint) uint {
	o := models.Order{OrderNo: genNo("ORD"), MemberID: memberID, Status: models.OrderStatusPaid, PayAmount: basePrice}
	f.db.Create(&o)
	t := models.Ticket{
		TicketNo: genNo("TKT"), OrderID: o.ID, MemberID: memberID,
		SessionID: sessionID, SeatID: seatID, Status: models.TicketStatusIssued,
		BasePrice: basePrice, PaidAmount: basePrice,
	}
	if couponID != nil {
		var c models.Coupon
		f.db.First(&c, *couponID)
		d, _ := CalcDiscount(basePrice, &c, f.now)
		// 已核销券也允许作为“原票已用券”写入（历史事实）
		if c.RedeemedAt != nil || !c.ExpireAt.After(f.now) {
			d = discountOf(basePrice, c.Type, c.Rate, c.Amount)
		}
		t.CouponID = &c.ID
		t.CouponName, t.CouponType, t.CouponRate, t.CouponAmount = c.Name, c.Type, c.Rate, c.Amount
		t.Discount, t.PaidAmount = d, basePrice-d
		o.PayAmount = t.PaidAmount
	}
	f.db.Create(&t)
	f.db.Model(&o).Update("pay_amount", o.PayAmount)
	f.db.Create(&models.SeatOccupancy{SessionID: sessionID, SeatID: seatID, HolderType: HolderTicket, HolderID: t.ID})
	return t.ID
}

// paySuccess 走完整“发起支付→成功回执”。
func (f *testFixture) paySuccess(ex *models.Exchange) (*ReceiptResult, *models.Payment) {
	pay, _, err := f.svc.InitiatePayment(ex.ExchangeNo, ex.MemberID)
	if err != nil {
		panic(err)
	}
	res, err := f.svc.ApplyReceipt(ReceiptInput{
		ReceiptNo: genNo("RCP"), PaymentNo: pay.PaymentNo, ExchangeNo: ex.ExchangeNo,
		Kind: "pay_success", Status: "success", Amount: pay.Amount,
	})
	if err != nil {
		panic(err)
	}
	return res, pay
}

func (f *testFixture) getTicket(id uint) models.Ticket {
	var t models.Ticket
	f.db.First(&t, id)
	return t
}

func (f *testFixture) occupantCount(sessionID, seatID uint) int64 {
	var n int64
	f.db.Model(&models.SeatOccupancy{}).Where("session_id = ? AND seat_id = ?", sessionID, seatID).Count(&n)
	return n
}

func (f *testFixture) occupant(sessionID, seatID uint) (string, uint) {
	var o models.SeatOccupancy
	if err := f.db.Where("session_id = ? AND seat_id = ?", sessionID, seatID).First(&o).Error; err != nil {
		return "", 0
	}
	return o.HolderType, o.HolderID
}

// ---------- 计价单测 ----------

func TestCalcDiscount(t *testing.T) {
	f := newFixture(t)
	mid := f.mkMember("m1")
	load := func(id uint) models.Coupon {
		var c models.Coupon
		if err := f.db.First(&c, id).Error; err != nil {
			t.Fatalf("load coupon %d: %v", id, err)
		}
		return c
	}

	c := load(f.mkCoupon(mid, "8折", "discount", 80, 0, false, false))
	if d, err := CalcDiscount(6000, &c, f.now); err != nil || d != 1200 {
		t.Fatalf("8折 on 6000: d=%d err=%v, want 1200", d, err)
	}

	c = load(f.mkCoupon(mid, "used", "discount", 90, 0, true, false))
	if _, err := CalcDiscount(6000, &c, f.now); err != ErrCouponRedeemed {
		t.Fatalf("redeemed coupon: want ErrCouponRedeemed, got %v", err)
	}

	c = load(f.mkCoupon(mid, "exp", "discount", 50, 0, false, true))
	if _, err := CalcDiscount(6000, &c, f.now); err != ErrCouponExpired {
		t.Fatalf("expired coupon: want ErrCouponExpired, got %v", err)
	}

	// 立减券：面额 > 票价时抵扣不超过票价
	c = load(f.mkCoupon(mid, "减3000", "amount", 0, 3000, false, false))
	if d, _ := CalcDiscount(2000, &c, f.now); d != 2000 {
		t.Fatalf("amount coupon cap: d=%d, want 2000", d)
	}
}

func TestQuoteDifferences(t *testing.T) {
	f := newFixture(t)
	mid := f.mkMember("m")
	oldS, oldSeats := f.mkSession("旧片", 6000)

	// 原票 6000 用9折 → 实付 5400
	c90 := f.mkCoupon(mid, "9折", "discount", 90, 0, true, false)
	tid := f.mkTicket(mid, oldS, oldSeats[0][0], 6000, &c90)
	old := f.getTicket(tid)

	// 新场次 8000，沿用原券 9 折 → 7200，补 1800
	hi, _ := f.mkSession("贵片", 8000)
	var hiSeat models.Seat
	f.db.First(&hiSeat, "session_id = ?", hi)
	q, err := QuoteExchange(&old, 8000, nil, true, f.now)
	if err != nil || q.DiffAmount != 1800 || q.NewPaid != 7200 {
		t.Fatalf("pay case: %+v err=%v", q, err)
	}

	// 新场次 5000，沿用 9 折 → 4500，退 900
	q, _ = QuoteExchange(&old, 5000, nil, true, f.now)
	if q.DiffAmount != -900 || q.NewPaid != 4500 {
		t.Fatalf("refund case: %+v", q)
	}

	// 平价：新价也 6000 沿用 → 5400，差 0
	q, _ = QuoteExchange(&old, 6000, nil, true, f.now)
	if q.DiffAmount != 0 {
		t.Fatalf("zero case: %+v", q)
	}

	// 新券：8000 的场次用8折券 → 6400，对比原实付5400，补1000
	c80id := f.mkCoupon(mid, "8折", "discount", 80, 0, false, false)
	var c80 models.Coupon
	f.db.First(&c80, c80id)
	q, err = QuoteExchange(&old, 8000, &c80, true, f.now)
	if err != nil || q.NewDiscount != 1600 || q.DiffAmount != 1000 {
		t.Fatalf("new coupon case: %+v err=%v", q, err)
	}
}

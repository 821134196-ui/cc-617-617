package service

import (
	"sync"
	"testing"
	"time"

	"cinema/models"
)

// 并发抢座：N 个会员同时改签/抢同一个新座位，只能 1 个成功。
func TestConcurrentSeatGrab(t *testing.T) {
	f := newFixture(t)

	sid, seats := f.mkSession("热映片", 6000)
	target := seats[0][0]

	const N = 20
	members := make([]uint, N)
	tickets := make([]uint, N)
	// 每人在同一场次的其他座位都有一张票，试图改到 target
	for i := 0; i < N; i++ {
		members[i] = f.mkMember("grab")
		// 各自的“原票”放在不同场次，避免与目标场次冲突
		osid, oseats := f.mkSession("旧场", 5000)
		tickets[i] = f.mkTicket(members[i], osid, oseats[i%2][i%4], 5000, nil)
	}

	var (
		wg       sync.WaitGroup
		successN int64
		mu       sync.Mutex
		okNos    []string
	)
	start := make(chan struct{})
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			ex, err := f.svc.Create(CreateInput{
				MemberID: members[i], TicketID: tickets[i],
				NewSessionID: sid, NewSeatID: target,
				IdempotencyKey: genNo("IDEM"),
			})
			if err == nil {
				mu.Lock()
				okNos = append(okNos, ex.ExchangeNo)
				mu.Unlock()
				atomicAdd(&successN)
			}
		}()
	}
	close(start)
	wg.Wait()

	if successN != 1 {
		t.Fatalf("concurrent grab: %d successes, want exactly 1 (nos=%v)", successN, okNos)
	}
	if n := f.occupantCount(sid, target); n != 1 {
		t.Fatalf("occupancy rows for target seat: %d, want 1", n)
	}
}

// 同一会员双击/客户端重试：相同幂等键只产生一笔改签单。
func TestIdempotentCreate(t *testing.T) {
	f := newFixture(t)
	mid := f.mkMember("dbl")
	osid, oseats := f.mkSession("旧", 6000)
	tid := f.mkTicket(mid, osid, oseats[0][0], 6000, nil)
	nsid, nseats := f.mkSession("新", 7000)

	key := genNo("FIXED")
	ex1, err := f.svc.Create(CreateInput{mid, tid, nsid, nseats[0][0], nil, key})
	if err != nil {
		t.Fatal(err)
	}
	ex2, err := f.svc.Create(CreateInput{mid, tid, nsid, nseats[0][1], nil, key})
	if err != nil {
		t.Fatal(err)
	}
	if ex1.ID != ex2.ID || ex1.NewSeatID != ex2.NewSeatID {
		t.Fatalf("idempotent retry created a different exchange: %d vs %d", ex1.ID, ex2.ID)
	}
	// 原票第二张座位不应被占用
	if n := f.occupantCount(nsid, nseats[0][1]); n != 0 {
		t.Fatalf("retry must not hold a second seat, occ=%d", n)
	}
	// 同票第二笔进行中的改签（不同键）必须被拒绝
	if _, err := f.svc.Create(CreateInput{mid, tid, nsid, nseats[0][1], nil, genNo("OTHER")}); err == nil {
		t.Fatal("second concurrent exchange for same ticket must fail")
	}
}

// 同行订单只改一张：支付成功后新票生效，原票归档，同单其余票原场次不动。
func TestSingleTicketExchangeKeepsCompanions(t *testing.T) {
	f := newFixture(t)
	mid := f.mkMember("family")
	osid, oseats := f.mkSession("家庭旧场", 6000)
	nsid, nseats := f.mkSession("家庭新场", 8000)

	// 同一订单两张票（手动保证 order 相同）
	o := models.Order{OrderNo: genNo("ORD"), MemberID: mid, Status: models.OrderStatusPaid}
	f.db.Create(&o)
	mk := func(seatID uint) uint {
		tk := models.Ticket{TicketNo: genNo("TKT"), OrderID: o.ID, MemberID: mid,
			SessionID: osid, SeatID: seatID, Status: models.TicketStatusIssued,
			BasePrice: 6000, PaidAmount: 6000}
		f.db.Create(&tk)
		f.db.Create(&models.SeatOccupancy{SessionID: osid, SeatID: seatID, HolderType: HolderTicket, HolderID: tk.ID})
		return tk.ID
	}
	t1 := mk(oseats[0][0])
	t2 := mk(oseats[0][1])

	ex, err := f.svc.Create(CreateInput{mid, t1, nsid, nseats[1][1], nil, genNo("K")})
	if err != nil {
		t.Fatal(err)
	}
	res, _ := f.paySuccess(ex)
	if res.Effect != "exchange_succeeded" {
		t.Fatalf("effect=%s", res.Effect)
	}

	// 原票 t1 归档
	if got := f.getTicket(t1).Status; got != models.TicketStatusExchanged {
		t.Fatalf("old ticket status=%s, want exchanged", got)
	}
	// 同行票 t2 仍然 issued、原场次原座位
	t2got := f.getTicket(t2)
	if t2got.Status != models.TicketStatusIssued || t2got.SessionID != osid || t2got.SeatID != oseats[0][1] {
		t.Fatalf("companion ticket altered: %+v", t2got)
	}
	if ht, _ := f.occupant(osid, oseats[0][1]); ht != HolderTicket {
		t.Fatalf("companion seat must remain sold")
	}
	// 原票座位已释放
	if n := f.occupantCount(osid, oseats[0][0]); n != 0 {
		t.Fatalf("old seat should be released, occ=%d", n)
	}
	// 新座位由新票持有
	ht, hid := f.occupant(nsid, nseats[1][1])
	if ht != HolderTicket || hid == 0 {
		t.Fatalf("new seat holder=%s/%d", ht, hid)
	}
	// 新票指向原票，金额正确
	var nt models.Ticket
	f.db.First(&nt, hid)
	if nt.ReissuedFrom == nil || *nt.ReissuedFrom != t1 {
		t.Fatalf("new ticket missing reissued_from: %+v", nt)
	}
	if nt.PaidAmount != 8000 {
		t.Fatalf("new ticket paid=%d want 8000", nt.PaidAmount)
	}
}

// 补款失败：原订单/原座位保留，新座位释放，改签单 failed，券归还。
func TestPaymentFailureRecovery(t *testing.T) {
	f := newFixture(t)
	mid := f.mkMember("fail")
	osid, oseats := f.mkSession("旧", 6000)
	tid := f.mkTicket(mid, osid, oseats[0][0], 6000, nil)
	nsid, nseats := f.mkSession("新", 9000)
	cpID := f.mkCoupon(mid, "8折", "discount", 80, 0, false, false)

	ex, err := f.svc.Create(CreateInput{mid, tid, nsid, nseats[0][0], &cpID, genNo("K")})
	if err != nil {
		t.Fatal(err)
	}
	pay, _, err := f.svc.InitiatePayment(ex.ExchangeNo, mid)
	if err != nil {
		t.Fatal(err)
	}
	r1, err := f.svc.ApplyReceipt(ReceiptInput{
		ReceiptNo: genNo("RCP"), PaymentNo: pay.PaymentNo, ExchangeNo: ex.ExchangeNo,
		Kind: "pay_failed", Status: "failed", Amount: pay.Amount, Note: "insufficient balance",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r1.Effect != "exchange_failed" {
		t.Fatalf("effect=%s", r1.Effect)
	}

	// 原票仍有效、原座位仍持有
	if got := f.getTicket(tid).Status; got != models.TicketStatusIssued {
		t.Fatalf("old ticket status=%s after failure", got)
	}
	if ht, _ := f.occupant(osid, oseats[0][0]); ht != HolderTicket {
		t.Fatalf("old seat must remain occupied after payment failure")
	}
	// 新座位释放
	if n := f.occupantCount(nsid, nseats[0][0]); n != 0 {
		t.Fatalf("new seat must be released after failure, occ=%d", n)
	}
	// 改签单终态
	var ex2 models.Exchange
	f.db.First(&ex2, ex.ID)
	if ex2.Status != models.ExFailed {
		t.Fatalf("exchange status=%s", ex2.Status)
	}
	// 券归还（可再次使用）
	var c models.Coupon
	f.db.First(&c, cpID)
	if c.RedeemedAt != nil {
		t.Fatalf("coupon should be released after failure")
	}
	// 支付单 failed
	var p models.Payment
	f.db.First(&p, pay.ID)
	if p.Status != models.PayFailed {
		t.Fatalf("payment status=%s", p.Status)
	}

	// 失败后可以重新改签成功
	ex3, err := f.svc.Create(CreateInput{mid, tid, nsid, nseats[0][0], &cpID, genNo("K2")})
	if err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
	res, _ := f.paySuccess(ex3)
	if res.Effect != "exchange_succeeded" {
		t.Fatalf("retry effect=%s", res.Effect)
	}
	if got := f.getTicket(tid).Status; got != models.TicketStatusExchanged {
		t.Fatalf("old ticket should be exchanged after retry, got %s", got)
	}
}

// 重复回执：同 receipt_no 第二次到达返回 duplicate，不触发第二次改签。
func TestDuplicateReceipt(t *testing.T) {
	f := newFixture(t)
	mid := f.mkMember("dup")
	osid, oseats := f.mkSession("旧", 6000)
	tid := f.mkTicket(mid, osid, oseats[0][0], 6000, nil)
	nsid, nseats := f.mkSession("新", 7000)
	ex, err := f.svc.Create(CreateInput{mid, tid, nsid, nseats[0][0], nil, genNo("K")})
	if err != nil {
		t.Fatal(err)
	}
	pay, _, err := f.svc.InitiatePayment(ex.ExchangeNo, mid)
	if err != nil {
		t.Fatal(err)
	}
	in := ReceiptInput{ReceiptNo: "FIXED-RCP-1", PaymentNo: pay.PaymentNo, ExchangeNo: ex.ExchangeNo,
		Kind: "pay_success", Status: "success", Amount: pay.Amount}
	r1, err := f.svc.ApplyReceipt(in)
	if err != nil || r1.Effect != "exchange_succeeded" {
		t.Fatalf("first: %+v err=%v", r1, err)
	}
	r2, err := f.svc.ApplyReceipt(in)
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Duplicate || r2.Effect != "duplicate" {
		t.Fatalf("second receipt: %+v, want duplicate", r2)
	}
	// 再来几次仍然 duplicate，新票只有一张
	for i := 0; i < 3; i++ {
		if r, _ := f.svc.ApplyReceipt(in); !r.Duplicate {
			t.Fatalf("repeat %d not duplicate", i)
		}
	}
	var nT int64
	f.db.Model(&models.Ticket{}).Where("reissued_from = ?", tid).Count(&nT)
	if nT != 1 {
		t.Fatalf("new tickets issued: %d, want 1", nT)
	}
	var nEx int64
	f.db.Model(&models.Exchange{}).Where("id = ? AND status = ?", ex.ID, models.ExSucceeded).Count(&nEx)
	if nEx != 1 {
		t.Fatal("exchange not exactly succeeded")
	}
}

// 迟到回执：改签已失败后，渠道“成功”回执晚到 → 自动退款，绝不改签，原票保留。
func TestLateSuccessAfterFailure(t *testing.T) {
	f := newFixture(t)
	mid := f.mkMember("late")
	osid, oseats := f.mkSession("旧", 6000)
	tid := f.mkTicket(mid, osid, oseats[0][0], 6000, nil)
	nsid, nseats := f.mkSession("新", 9000)
	ex, err := f.svc.Create(CreateInput{mid, tid, nsid, nseats[0][0], nil, genNo("K")})
	if err != nil {
		t.Fatal(err)
	}
	pay, _, err := f.svc.InitiatePayment(ex.ExchangeNo, mid)
	if err != nil {
		t.Fatal(err)
	}
	// 失败
	if _, err := f.svc.ApplyReceipt(ReceiptInput{
		ReceiptNo: genNo("RCP"), PaymentNo: pay.PaymentNo, ExchangeNo: ex.ExchangeNo,
		Kind: "pay_failed", Status: "failed", Amount: pay.Amount,
	}); err != nil {
		t.Fatal(err)
	}
	// 迟到的成功（新回执号）
	late, err := f.svc.ApplyReceipt(ReceiptInput{
		ReceiptNo: genNo("RCP"), PaymentNo: pay.PaymentNo, ExchangeNo: ex.ExchangeNo,
		Kind: "pay_success", Status: "success", Amount: pay.Amount, Note: "late",
	})
	if err != nil {
		t.Fatal(err)
	}
	if late.Effect != "auto_refund_late_success" {
		t.Fatalf("late effect=%s", late.Effect)
	}
	// 原票仍有效
	if got := f.getTicket(tid).Status; got != models.TicketStatusIssued {
		t.Fatalf("old ticket status=%s, must remain issued", got)
	}
	// 新座位未被持有
	if n := f.occupantCount(nsid, nseats[0][0]); n != 0 {
		t.Fatalf("new seat occ=%d, want 0", n)
	}
	// 存在等额成功退款单
	var rf models.Payment
	if err := f.db.Where("exchange_id = ? AND type = ? AND status = ?", ex.ID, "refund", models.PaySuccess).First(&rf).Error; err != nil {
		t.Fatalf("auto refund payment missing: %v", err)
	}
	if rf.Amount != pay.Amount {
		t.Fatalf("refund amount=%d want %d", rf.Amount, pay.Amount)
	}
	// 改签单仍 failed
	var ex2 models.Exchange
	f.db.First(&ex2, ex.ID)
	if ex2.Status != models.ExFailed {
		t.Fatalf("exchange status=%s", ex2.Status)
	}
	// 若该退款回执再重放，属于另一条 refund 回执重复 → duplicate；
	// 对同一 late 回执号重放 → duplicate
	if r, _ := f.svc.ApplyReceipt(ReceiptInput{
		ReceiptNo: late.Receipt.ReceiptNo, PaymentNo: pay.PaymentNo, ExchangeNo: ex.ExchangeNo,
		Kind: "pay_success", Status: "success", Amount: pay.Amount,
	}); !r.Duplicate {
		t.Fatal("late receipt replay must be duplicate")
	}
}

// 预留超时：到点后新座位释放、改签 expired、原票保留；之后到达的成功回执自动退款。
func TestReservationExpiry(t *testing.T) {
	f := newFixture(t)
	mid := f.mkMember("exp")
	osid, oseats := f.mkSession("旧", 6000)
	tid := f.mkTicket(mid, osid, oseats[0][0], 6000, nil)
	nsid, nseats := f.mkSession("新", 9000)
	cpID := f.mkCoupon(mid, "8折", "discount", 80, 0, false, false)
	ex, err := f.svc.Create(CreateInput{mid, tid, nsid, nseats[0][0], &cpID, genNo("K")})
	if err != nil {
		t.Fatal(err)
	}
	pay, _, err := f.svc.InitiatePayment(ex.ExchangeNo, mid)
	if err != nil {
		t.Fatal(err)
	}

	// 超时前别人不能抢
	if _, err := f.svc.Create(CreateInput{mid, tid, nsid, nseats[0][0], nil, genNo("X")}); err == nil {
		t.Fatal("seat held: same-ticket second exchange unexpectedly allowed")
	}

	f.advance(3 * time.Second) // TTL=2s
	n, err := f.svc.ExpireOnce()
	if err != nil || n != 1 {
		t.Fatalf("expire count=%d err=%v", n, err)
	}
	if c := f.occupantCount(nsid, nseats[0][0]); c != 0 {
		t.Fatalf("after expiry occupancy=%d", c)
	}
	var ex2 models.Exchange
	f.db.First(&ex2, ex.ID)
	if ex2.Status != models.ExExpired {
		t.Fatalf("status=%s", ex2.Status)
	}
	if got := f.getTicket(tid).Status; got != models.TicketStatusIssued {
		t.Fatalf("old ticket changed on expiry: %s", got)
	}
	var c models.Coupon
	f.db.First(&c, cpID)
	if c.RedeemedAt != nil {
		t.Fatal("coupon should be released on expiry")
	}
	// 座位释放后别人（这里同一会员新改签）可重新占用
	ex3, err := f.svc.Create(CreateInput{mid, tid, nsid, nseats[0][0], &cpID, genNo("K2")})
	if err != nil {
		t.Fatalf("rebook after expiry: %v", err)
	}
	// 旧支付单迟到的成功回执：必须自动退款而非完成旧单
	late, err := f.svc.ApplyReceipt(ReceiptInput{
		ReceiptNo: genNo("RCP"), PaymentNo: pay.PaymentNo, ExchangeNo: ex.ExchangeNo,
		Kind: "pay_success", Status: "success", Amount: pay.Amount,
	})
	if err != nil {
		t.Fatal(err)
	}
	if late.Effect != "auto_refund_late_success" {
		t.Fatalf("late-after-expiry effect=%s", late.Effect)
	}
	// 新的一笔改签仍可正常支付成功
	res, _ := f.paySuccess(ex3)
	if res.Effect != "exchange_succeeded" {
		t.Fatalf("new exchange effect=%s", res.Effect)
	}
}

// 退差改签成功后自动生成等额退款单；重复调用完成动作不得二次执行。
func TestRefundDifferenceAutoRefund(t *testing.T) {
	f := newFixture(t)
	mid := f.mkMember("rf")
	osid, oseats := f.mkSession("旧", 6000)
	c90 := f.mkCoupon(mid, "9折", "discount", 90, 0, true, false)
	tid := f.mkTicket(mid, osid, oseats[0][0], 6000, &c90) // 实付 5400
	nsid, nseats := f.mkSession("新", 5000)                 // 沿用9折 = 4500，退 900

	ex, err := f.svc.Create(CreateInput{mid, tid, nsid, nseats[0][0], nil, genNo("K")})
	if err != nil {
		t.Fatal(err)
	}
	if ex.DiffAmount != -900 {
		t.Fatalf("diff=%d", ex.DiffAmount)
	}
	if err := f.svc.FinalizeRefund(ex.ExchangeNo, mid); err != nil {
		t.Fatal(err)
	}
	// 再调一次必须被状态机拒绝（不可二次改签）
	if err := f.svc.FinalizeRefund(ex.ExchangeNo, mid); err == nil {
		t.Fatal("double finalize must fail")
	}
	var rf models.Payment
	if err := f.db.Where("exchange_id = ? AND type = ?", ex.ID, "refund").First(&rf).Error; err != nil {
		t.Fatal(err)
	}
	if rf.Amount != 900 || rf.Status != models.PaySuccess {
		t.Fatalf("refund=%+v", rf)
	}
	var nt models.Ticket
	if err := f.db.Where("reissued_from = ?", tid).First(&nt).Error; err != nil {
		t.Fatal(err)
	}
	if nt.PaidAmount != 4500 || nt.Discount != 500 {
		t.Fatalf("new ticket %+v", nt)
	}
	if nt.CouponName == "" {
		t.Fatal("new ticket must preserve original coupon basis")
	}
}

func atomicAdd(p *int64) { *p++ }

// 平价改签：新旧实付相同，无需支付即可完成；重复完成被拒绝。
func TestZeroDifference(t *testing.T) {
	f := newFixture(t)
	mid := f.mkMember("zero")
	osid, oseats := f.mkSession("旧", 6000)
	// 原票无券实付 6000
	tid := f.mkTicket(mid, osid, oseats[0][0], 6000, nil)
	// 新场次也是 6000，无券
	nsid, nseats := f.mkSession("新", 6000)
	ex, err := f.svc.Create(CreateInput{mid, tid, nsid, nseats[0][0], nil, genNo("K")})
	if err != nil {
		t.Fatal(err)
	}
	if ex.DiffAmount != 0 {
		t.Fatalf("diff=%d want 0", ex.DiffAmount)
	}
	if _, err := f.svc.FinalizeZero(ex.ExchangeNo, mid); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.FinalizeZero(ex.ExchangeNo, mid); err == nil {
		t.Fatal("double finalize must fail")
	}
	var e models.Exchange
	f.db.First(&e, ex.ID)
	if e.Status != models.ExSucceeded {
		t.Fatalf("status=%s", e.Status)
	}
	if n := f.occupantCount(osid, oseats[0][0]); n != 0 {
		t.Fatal("old seat not released")
	}
	if ht, _ := f.occupant(nsid, nseats[0][0]); ht != HolderTicket {
		t.Fatal("new seat not held by ticket")
	}
	// 不应生成任何收/退款支付单
	var nPay int64
	f.db.Model(&models.Payment{}).Where("exchange_id = ?", ex.ID).Count(&nPay)
	if nPay != 0 {
		t.Fatalf("zero-diff should create no payments, got %d", nPay)
	}
}

// 并发重放同一成功回执：只有一条被应用，改签恰好完成一次。
func TestConcurrentSameReceipt(t *testing.T) {
	f := newFixture(t)
	mid := f.mkMember("cd")
	osid, oseats := f.mkSession("旧", 6000)
	tid := f.mkTicket(mid, osid, oseats[0][0], 6000, nil)
	nsid, nseats := f.mkSession("新", 7000)
	ex, err := f.svc.Create(CreateInput{mid, tid, nsid, nseats[0][0], nil, genNo("K")})
	if err != nil {
		t.Fatal(err)
	}
	pay, _, err := f.svc.InitiatePayment(ex.ExchangeNo, mid)
	if err != nil {
		t.Fatal(err)
	}
	in := ReceiptInput{ReceiptNo: "RCP-CONC-1", PaymentNo: pay.PaymentNo, ExchangeNo: ex.ExchangeNo,
		Kind: "pay_success", Status: "success", Amount: pay.Amount}

	const N = 16
	var wg sync.WaitGroup
	succ, dup := int64(0), int64(0)
	start := make(chan struct{})
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			<-start
			r, err := f.svc.ApplyReceipt(in)
			if err != nil {
				t.Errorf("apply: %v", err)
				return
			}
			if r.Duplicate {
				atomicAdd(&dup)
			} else if r.Effect == "exchange_succeeded" {
				atomicAdd(&succ)
			}
		}()
	}
	close(start)
	wg.Wait()
	if succ != 1 || dup != N-1 {
		t.Fatalf("succ=%d dup=%d (want 1/%d)", succ, dup, N-1)
	}
	var n int64
	f.db.Model(&models.Ticket{}).Where("reissued_from = ?", tid).Count(&n)
	if n != 1 {
		t.Fatalf("new tickets=%d want 1", n)
	}
}

// 券并发：两张票同时尝试使用同一张新券，只能一笔占用成功。
func TestConcurrentCouponUse(t *testing.T) {
	f := newFixture(t)
	shared := f.mkCoupon(0, "shared80", "discount", 80, 0, false, false)
	// 把券归属到第一个会员，但允许两个不同会员测试占用竞争：改为同一会员的两张票
	mid := f.mkMember("cp")
	f.db.Model(&models.Coupon{}).Where("id = ?", shared).Update("member_id", mid)

	osid, oseats := f.mkSession("旧", 6000)
	nsid, nseats := f.mkSession("新", 8000)
	t1 := f.mkTicket(mid, osid, oseats[0][0], 6000, nil)
	t2 := f.mkTicket(mid, osid, oseats[0][1], 6000, nil)

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, errs[0] = f.svc.Create(CreateInput{mid, t1, nsid, nseats[0][0], &shared, genNo("K1")})
	}()
	go func() {
		defer wg.Done()
		<-start
		_, errs[1] = f.svc.Create(CreateInput{mid, t2, nsid, nseats[0][1], &shared, genNo("K2")})
	}()
	close(start)
	wg.Wait()

	ok, denied := 0, 0
	for _, e := range errs {
		if e == nil {
			ok++
		} else if e == ErrCouponRedeemed || e == ErrExchangeState {
			denied++
		} else {
			t.Fatalf("unexpected err %v", e)
		}
	}
	if ok != 1 || denied != 1 {
		t.Fatalf("coupon grab ok=%d denied=%d errs=%v", ok, denied, errs)
	}
}

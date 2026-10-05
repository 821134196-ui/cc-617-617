package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func testDB(t *testing.T) (*gorm.DB, *Service) {
	t.Helper()
	db, err := openDB(t.TempDir() + "/cinema.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	return db, NewService(db, 2*time.Second)
}

// canRedeem 是新购票/新增抵扣时的券校验：仅 available 且未过期可核销。
// 改签中原票券的迁移属于同一权益的延续，不属于再次抵扣。
func canRedeem(c Coupon, now time.Time) bool {
	return c.Status == CouponAvailable && c.Kind != "" && c.ExpireAt.After(now)
}

func TestPricingBasis(t *testing.T) {
	// 9 折：4500 -> 抵扣 450
	if d := discountFor(&DiscountBasis{Kind: "percent", Value: 90}, 4500); d != 450 {
		t.Fatalf("percent discount = %d", d)
	}
	// 9 折迁移到 5800 场次：抵扣 580
	if d := discountFor(&DiscountBasis{Kind: "percent", Value: 90}, 5800); d != 580 {
		t.Fatalf("percent discount new show = %d", d)
	}
	// 立减券达到门槛
	if d := discountFor(&DiscountBasis{Kind: "fixed", Value: 1000, MinSpend: 4000}, 4000); d != 1000 {
		t.Fatalf("fixed discount = %d", d)
	}
	// 立减券未达门槛不能抵扣
	if d := discountFor(&DiscountBasis{Kind: "fixed", Value: 1000, MinSpend: 4000}, 3999); d != 0 {
		t.Fatalf("fixed discount below min spend = %d", d)
	}
	// 抵扣不超过票面
	if d := discountFor(&DiscountBasis{Kind: "fixed", Value: 1000, MinSpend: 0}, 500); d != 500 {
		t.Fatalf("fixed discount cap = %d", d)
	}
}

func TestCouponRedemptionRules(t *testing.T) {
	db, _ := testDB(t)
	now := time.Now()
	var cs []Coupon
	db.Find(&cs)
	avail := map[string]bool{}
	for _, c := range cs {
		avail[c.Code] = canRedeem(c, now)
	}
	if avail["CPN-90OFF"] {
		t.Fatal("已使用券不能再次抵扣")
	}
	if avail["CPN-EXPIRED"] {
		t.Fatal("过期券不能抵扣")
	}
	if !avail["CPN-NEW8"] {
		t.Fatal("可用券应可抵扣")
	}
}

// 并发抢座：同场次同座位只有一个预留成功。
func TestConcurrentSeatGrab(t *testing.T) {
	db, svc := testDB(t)
	// 种子票 1、2 分别发起对场次 2 / B5 的抢占
	var wg sync.WaitGroup
	results := make([]error, 2)
	ticketIDs := []int64{1, 2}
	loser := int64(-1)
	start := make(chan struct{})
	for i, ticketID := range ticketIDs {
		wg.Add(1)
		go func(idx int, tid int64) {
			defer wg.Done()
			<-start
			_, results[idx] = svc.Reserve(1, tid, 2, 2, 5,
				fmt.Sprintf("idem-grab-%d-%d", tid, time.Now().UnixNano()))
		}(i, ticketID)
	}
	close(start)
	wg.Wait()

	ok, fail := 0, 0
	for i, e := range results {
		switch e {
		case nil:
			ok++
		case ErrSeatTaken, ErrInFlight:
			fail++
			loser = ticketIDs[i]
		default:
			t.Fatalf("unexpected error: %v", e)
		}
	}
	if ok != 1 || fail != 1 || loser < 0 {
		t.Fatalf("并发抢座结果异常: 成功=%d 失败=%d（应为 1/1）", ok, fail)
	}

	// 库里只允许存在一个 held 锁
	var held int64
	db.Model(&SeatLock{}).
		Where("showtime_id = ? AND seat_code = ? AND status = ?", 2, "B5", LockHeld).
		Count(&held)
	if held != 1 {
		t.Fatalf("held 锁数量 = %d，应为 1", held)
	}

	// 失败者改选 B6 应可成功
	if _, err := svc.Reserve(1, loser, 2, 2, 6, "idem-grab-alt"); err != nil {
		t.Fatalf("备选座位应可预留: %v", err)
	}
}

// 同行订单只改一张：其余票保留原场次原座位。
func TestSingleTicketExchangeKeepsOthers(t *testing.T) {
	db, svc := testDB(t)
	// 票1（9折券）改到场次3（4000 元场）：新抵扣 400，新实付 3600，
	// 原实付 4050 => 应退 450
	r, err := svc.Reserve(1, 1, 3, 1, 1, "idem-single")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if r.NewDiscountAmount != 400 || r.NewPaidAmount != 3600 || r.Delta != -450 {
		t.Fatalf("计价错误: disc=%d paid=%d delta=%d", r.NewDiscountAmount, r.NewPaidAmount, r.Delta)
	}

	res, err := svc.Settle(r.ReservationNo, true, "RFD-1")
	if err != nil || res.Action != "completed" {
		t.Fatalf("结算: %+v err=%v", res, err)
	}

	// 票 1 exchanged，票 2、3 仍 active 且留在场次 1
	var tickets []Ticket
	db.Order("id").Find(&tickets)
	if tickets[0].Status != TicketExchanged || tickets[0].ShowtimeID != 1 {
		t.Fatal("原票1应标记 exchanged 并留痕")
	}
	if tickets[1].Status != TicketActive || tickets[1].ShowtimeID != 1 || tickets[1].SeatCode != "C5" {
		t.Fatal("票2应保留在原场次 C5")
	}
	if tickets[2].Status != TicketActive || tickets[2].ShowtimeID != 1 || tickets[2].SeatCode != "C6" {
		t.Fatal("票3应保留在原场次 C6")
	}
	var newTicket Ticket
	if err := db.First(&newTicket, *res.Reservation.NewTicketID).Error; err != nil {
		t.Fatalf("new ticket: %v", err)
	}
	if newTicket.ShowtimeID != 3 || newTicket.SeatCode != "A1" || newTicket.DiscountAmount != 400 ||
		newTicket.PaidAmount != 3600 || newTicket.CouponID == nil || *newTicket.CouponID != 1 {
		t.Fatalf("新票数据错误: %+v", newTicket)
	}
	// 优惠依据快照必须保留
	b := parseBasis(newTicket.DiscountBasis)
	if b == nil || b.Kind != "percent" || b.Value != 90 || b.CouponCode != "CPN-90OFF" {
		t.Fatalf("新票优惠依据丢失: %+v", b)
	}

	// 券仍为 used，但已迁移到新票（没有发生第二次核销）
	var cpn Coupon
	db.First(&cpn, 1)
	if cpn.Status != CouponUsed || cpn.CurrentTicketID == nil || *cpn.CurrentTicketID != newTicket.ID {
		t.Fatalf("券迁移错误: %+v", cpn)
	}

	// 原座 C4 released；C5、C6 仍 sold
	countSold := func(code string) int64 {
		var n int64
		db.Model(&SeatLock{}).Where("showtime_id = ? AND seat_code = ? AND status = ?", 1, code, LockSold).Count(&n)
		return n
	}
	if countSold("C4") != 0 {
		t.Fatal("原座位 C4 应已释放")
	}
	if countSold("C5") != 1 || countSold("C6") != 1 {
		t.Fatal("同行其余座位 C5/C6 不应被动")
	}
	var newSeatSold int64
	db.Model(&SeatLock{}).Where("showtime_id = ? AND seat_code = ? AND status = ?", 3, "A1", LockSold).Count(&newSeatSold)
	if newSeatSold != 1 {
		t.Fatal("新座位应为 sold")
	}

	// 订单仍然 active
	var order Order
	db.First(&order, 1)
	if order.Status != OrderActive {
		t.Fatal("同行订单应保持 active")
	}
}

// 补款失败：新座位释放、原票原座保留、可再次改签。
func TestPayFailureRecovery(t *testing.T) {
	db, svc := testDB(t)
	// 票1 -> 场次2（5800，9折后 5220），B5；delta = 5220-4050 = 1170 补款
	r, err := svc.Reserve(1, 1, 2, 2, 5, "idem-fail")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if r.Delta != 1170 {
		t.Fatalf("delta = %d, 期望 1170", r.Delta)
	}
	res, err := svc.Settle(r.ReservationNo, false, "")
	if err != nil || res.Action != "failed" {
		t.Fatalf("结算: action=%v err=%v", res, err)
	}

	// 原票仍 active、原座仍 sold
	var old Ticket
	db.First(&old, 1)
	if old.Status != TicketActive {
		t.Fatal("补款失败后原票必须保留")
	}
	var oldSeat int64
	db.Model(&SeatLock{}).Where("showtime_id = ? AND seat_code = ? AND status = ?", 1, "C4", LockSold).Count(&oldSeat)
	if oldSeat != 1 {
		t.Fatal("补款失败后原座位必须保留")
	}
	// 新座位已释放，可被重新预留
	var newSeat int64
	db.Model(&SeatLock{}).Where("showtime_id = ? AND seat_code = ? AND status IN ?", 2, "B5",
		[]string{LockHeld, LockSold}).Count(&newSeat)
	if newSeat != 0 {
		t.Fatal("补款失败后新座位必须释放")
	}
	// 未产生任何新票
	var nTickets int64
	db.Model(&Ticket{}).Count(&nTickets)
	if nTickets != 3 {
		t.Fatalf("补款失败不应产生新票，当前票数=%d", nTickets)
	}
	// 支付单 failed
	var pay Payment
	db.Where("reservation_id = ? AND direction = ?", r.ID, PayDirectionPay).First(&pay)
	if pay.Status != PayFailed {
		t.Fatalf("支付单状态=%s，应为 failed", pay.Status)
	}

	// 重新发起改签同一新座位，应成功
	r2, err := svc.Reserve(1, 1, 2, 2, 5, "idem-retry")
	if err != nil {
		t.Fatalf("失败后应可重新改签: %v", err)
	}
	if res2, err := svc.Settle(r2.ReservationNo, true, "PAY-OK-2"); err != nil || res2.Action != "completed" {
		t.Fatalf("重试结算: %+v err=%v", res2, err)
	}
}

// 重复/并发回执：只能完成一次改签。
func TestDuplicateReceiptIdempotent(t *testing.T) {
	db, svc := testDB(t)
	r, err := svc.Reserve(1, 1, 2, 2, 5, "idem-dup")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}

	// 并发 10 个成功回执
	var wg sync.WaitGroup
	actions := make([]string, 10)
	start := make(chan struct{})
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := svc.Settle(r.ReservationNo, true, "PAY-DUP-1")
			if err != nil {
				actions[i] = "err:" + err.Error()
				return
			}
			actions[i] = res.Action
		}(i)
	}
	close(start)
	wg.Wait()

	completed, replayed := 0, 0
	for _, a := range actions {
		switch a {
		case "completed":
			completed++
		case "replayed":
			replayed++
		default:
			t.Fatalf("意外结果: %s", a)
		}
	}
	if completed != 1 || replayed != 9 {
		t.Fatalf("completed=%d replayed=%d，应为 1/9", completed, replayed)
	}

	// 原票只改签一次：新票只有一张，exchanged_to 指向它
	var newCount int64
	db.Model(&Ticket{}).Where("reservation_id = ? AND status = ?", r.ID, TicketActive).Count(&newCount)
	if newCount != 1 {
		t.Fatalf("新票数量=%d，应为 1", newCount)
	}
	var old Ticket
	db.First(&old, 1)
	if old.Status != TicketExchanged || old.ExchangedToID == nil {
		t.Fatal("原票应只有一条改签去向")
	}

	// 同号回执再到达：幂等成功；异号回执：明确拒绝（防伪造二次入账）
	res2, err := svc.Settle(r.ReservationNo, true, "PAY-DUP-1")
	if err != nil || !res2.Replayed {
		t.Fatalf("同号重复回执应幂等: %+v %v", res2, err)
	}
	if _, err := svc.Settle(r.ReservationNo, true, "PAY-DUP-DIFFERENT"); err == nil {
		t.Fatal("异号回执应被拒绝")
	}
}

// 预留超时后释放；迟到的成功补款不能触发改签，自动退款。
func TestExpireThenLateReceipt(t *testing.T) {
	db, _ := testDB(t)
	svc := NewService(db, 250*time.Millisecond)
	r, err := svc.Reserve(1, 1, 2, 2, 5, "idem-late")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	time.Sleep(350 * time.Millisecond)
	n, err := svc.ExpireDue()
	if err != nil || n != 1 {
		t.Fatalf("过期回收 n=%d err=%v", n, err)
	}

	var held int64
	db.Model(&SeatLock{}).Where("id = ? AND status = ?", r.SeatLockID, LockHeld).Count(&held)
	if held != 0 {
		t.Fatal("超时后新座位必须释放")
	}

	// 迟到的成功补款回执
	res, err := svc.Settle(r.ReservationNo, true, "PAY-LATE")
	if err != nil {
		t.Fatalf("late settle: %v", err)
	}
	if res.Action != "expired_late_refunded" {
		t.Fatalf("迟到补款动作=%s，应为自动退款", res.Action)
	}

	// 原票仍在、没有新票、改签单为 expired
	var old Ticket
	db.First(&old, 1)
	if old.Status != TicketActive {
		t.Fatal("超时后原票必须保留")
	}
	var nTickets int64
	db.Model(&Ticket{}).Count(&nTickets)
	if nTickets != 3 {
		t.Fatalf("迟到补款不得产生新票，票数=%d", nTickets)
	}
	var rr Reservation
	db.First(&rr, r.ID)
	if rr.Status != ResvExpired {
		t.Fatalf("改签单状态=%s", rr.Status)
	}
	// 自动退款单存在且成功
	var refund Payment
	if err := db.Where("reservation_id = ? AND direction = ?", r.ID, PayDirectionRefund).First(&refund).Error; err != nil {
		t.Fatalf("应生成自动退款单: %v", err)
	}
	if refund.Status != PaySucceeded || refund.Amount != 1170 {
		t.Fatalf("自动退款单错误: %+v", refund)
	}
	// 再次迟到/重复回执不再新增动作
	res2, err := svc.Settle(r.ReservationNo, true, "PAY-LATE")
	if err != nil || res2.Action != "expired_late_refunded" {
		t.Fatalf("重复迟到回执: %+v %v", res2, err)
	}
	var refundCount int64
	db.Model(&Payment{}).Where("reservation_id = ? AND direction = ?", r.ID, PayDirectionRefund).Count(&refundCount)
	if refundCount != 1 {
		t.Fatalf("自动退款只能有一笔，实际=%d", refundCount)
	}

	// 座位释放后可被正常改签
	svc2 := NewService(db, 30*time.Second)
	r3, err := svc2.Reserve(1, 1, 2, 2, 5, "idem-after-expire")
	if err != nil {
		t.Fatalf("超时释放后应可重新占位: %v", err)
	}
	if res3, err := svc2.Settle(r3.ReservationNo, true, "PAY-OK-3"); err != nil || res3.Action != "completed" {
		t.Fatalf("重新改签失败: %+v %v", res3, err)
	}
}

// HTTP 层：模拟支付回调重复 POST，只有第一次 completed。
func TestHTTPDuplicateReceipt(t *testing.T) {
	db, svc := testDB(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	registerRoutes(r, &Handler{svc: svc, db: db})

	r0, err := svc.Reserve(1, 1, 2, 2, 5, "idem-http")
	if err != nil {
		t.Fatal(err)
	}
	post := func(no string, success bool, receipt string) map[string]any {
		body, _ := json.Marshal(receiptReq{ReservationNo: no, Success: success, ReceiptNo: receipt})
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/api/exchange/receipt", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		if w.Code != 200 {
			t.Fatalf("http %d: %s", w.Code, w.Body.String())
		}
		return out
	}
	a1 := post(r0.ReservationNo, true, "HTTP-PAY-1")
	a2 := post(r0.ReservationNo, true, "HTTP-PAY-1")
	if a1["action"] != "completed" || a2["action"] != "replayed" || a2["replayed"] != true {
		t.Fatalf("重复回调: %v / %v", a1["action"], a2["action"])
	}
}

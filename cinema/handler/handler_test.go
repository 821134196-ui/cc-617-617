package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"cinema/models"
	"cinema/service"
	"cinema/storage"
)

func newAPIFixture(t *testing.T) (*gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := storage.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := models.AutoMigrate(db); err != nil {
		t.Fatal(err)
	}
	svc := service.NewExchangeService(db, time.Hour)
	h := New(svc)
	r := gin.New()
	h.Register(r)
	return r, db
}

func mustJSON(t *testing.T, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func post(t *testing.T, r http.Handler, path string, body any) (int, map[string]any) {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(mustJSON(t, body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// setupScenario 建 N 个会员，每人一张不同场次的已售票；
// 再建一个公共“目标场次”（含至少 1 个座位），返回目标场次与座位。
func setupScenario(t *testing.T, db *gorm.DB, n int) (memberIDs, ticketIDs []uint, targetSession, targetSeat uint) {
	t.Helper()
	mov := models.Movie{Title: "M"}
	db.Create(&mov)
	target := models.Session{MovieID: mov.ID, Hall: "T", StartAt: time.Now().Add(48 * time.Hour), BasePrice: 7000}
	db.Create(&target)
	for k := 1; k <= 2; k++ {
		db.Create(&models.Seat{SessionID: target.ID, Row: "A", Number: k})
	}
	var ts models.Seat
	db.First(&ts, "session_id = ? AND number = ?", target.ID, 1)
	targetSession, targetSeat = target.ID, ts.ID

	for i := 0; i < n; i++ {
		m := models.Member{Name: fmt.Sprintf("m%d", i), Phone: fmt.Sprintf("13900%06d", i)}
		db.Create(&m)
		old := models.Session{MovieID: mov.ID, Hall: fmt.Sprintf("H%d", i), StartAt: time.Now().Add(24 * time.Hour), BasePrice: 5000}
		db.Create(&old)
		st := models.Seat{SessionID: old.ID, Row: "A", Number: 1}
		db.Create(&st)
		o := models.Order{OrderNo: fmt.Sprintf("ORD%d", i), MemberID: m.ID, Status: models.OrderStatusPaid, PayAmount: 5000}
		db.Create(&o)
		tk := models.Ticket{
			TicketNo: fmt.Sprintf("TKT%d", i), OrderID: o.ID, MemberID: m.ID,
			SessionID: old.ID, SeatID: st.ID, Status: models.TicketStatusIssued,
			BasePrice: 5000, PaidAmount: 5000,
		}
		db.Create(&tk)
		db.Create(&models.SeatOccupancy{SessionID: old.ID, SeatID: st.ID, HolderType: service.HolderTicket, HolderID: tk.ID})
		memberIDs = append(memberIDs, m.ID)
		ticketIDs = append(ticketIDs, tk.ID)
	}
	return
}

func countIssued(db *gorm.DB, ticketID uint) int64 {
	var n int64
	db.Model(&models.Ticket{}).Where("id = ? AND status = ?", ticketID, models.TicketStatusIssued).Count(&n)
	return n
}
func countNewTickets(db *gorm.DB, oldID uint) int64 {
	var n int64
	db.Model(&models.Ticket{}).Where("reissued_from = ?", oldID).Count(&n)
	return n
}
func countOccupancy(db *gorm.DB, sessionID, seatID uint) int64 {
	var n int64
	db.Model(&models.SeatOccupancy{}).Where("session_id = ? AND seat_id = ?", sessionID, seatID).Count(&n)
	return n
}

// 20 个会员同时 POST /api/exchanges 抢同一座位，只有 1 个 200。
func TestAPIConcurrentGrab(t *testing.T) {
	r, db := newAPIFixture(t)
	mids, tids, sid, seat := setupScenario(t, db, 20)

	const N = 20
	var wg sync.WaitGroup
	statuses := make([]int, N)
	start := make(chan struct{})
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			code, _ := post(t, r, "/api/exchanges", map[string]any{
				"member_id":       mids[i],
				"ticket_id":       tids[i],
				"new_session_id":  sid,
				"new_seat_id":     seat,
				"idempotency_key": fmt.Sprintf("client-%d", i),
			})
			statuses[i] = code
		}()
	}
	close(start)
	wg.Wait()

	ok, conflict := 0, 0
	for _, c := range statuses {
		switch c {
		case 200:
			ok++
		case 409:
			conflict++
		default:
			t.Fatalf("unexpected http status %d (only 200/409 allowed)", c)
		}
	}
	if ok != 1 || conflict != N-1 {
		t.Fatalf("ok=%d conflict=%d (N=%d)", ok, conflict, N)
	}
	if n := countOccupancy(db, sid, seat); n != 1 {
		t.Fatalf("target occupancy=%d want 1", n)
	}
}

// 单张改签成功 + 重复回执不二次改签。
func TestAPISingleExchangeAndDuplicate(t *testing.T) {
	r, db := newAPIFixture(t)
	mids, tids, sid, seat := setupScenario(t, db, 1)
	mid, tid := mids[0], tids[0]

	code, ex := post(t, r, "/api/exchanges", map[string]any{
		"member_id": mid, "ticket_id": tid, "new_session_id": sid,
		"new_seat_id": seat, "idempotency_key": "K1",
	})
	if code != 200 {
		t.Fatalf("create: %d %v", code, ex)
	}
	exNo := ex["exchange_no"].(string)
	if len(ex["companions_kept"].([]any)) != 0 {
		t.Fatal("no companions expected")
	}

	code, pay := post(t, r, "/api/exchanges/"+exNo+"/pay", map[string]any{"member_id": mid})
	if code != 200 {
		t.Fatalf("pay init: %d %v", code, pay)
	}
	payNo := pay["payment_no"].(string)

	code, done := post(t, r, "/api/payments/"+payNo+"/mock-pay", map[string]any{
		"receipt_no": "RCP-FIXED-1", "fail": false,
	})
	if code != 200 {
		t.Fatalf("mock-pay: %d %v", code, done)
	}
	if res, _ := done["result"].(map[string]any); res["effect"] != "exchange_succeeded" {
		t.Fatalf("effect=%v", done["result"])
	}
	if n := countIssued(db, tid); n != 0 {
		t.Fatalf("old ticket still issued: %d", n)
	}
	if n := countNewTickets(db, tid); n != 1 {
		t.Fatalf("new tickets=%d want 1", n)
	}
	if n := countOccupancy(db, sid, seat); n != 1 {
		t.Fatalf("new seat occupancy=%d want 1", n)
	}

	// 相同回执号重复到达
	_, dup := post(t, r, "/api/payments/"+payNo+"/mock-pay", map[string]any{"receipt_no": "RCP-FIXED-1"})
	res := dup["result"].(map[string]any)
	if res["duplicate"] != true {
		t.Fatalf("duplicate not detected: %v", res)
	}
	if n := countNewTickets(db, tid); n != 1 {
		t.Fatalf("duplicate receipt issued another ticket: %d", n)
	}
}

// 支付失败恢复 + 迟到成功回执自动退款。
func TestAPIPaymentFailure(t *testing.T) {
	r, db := newAPIFixture(t)
	mids, tids, sid, seat := setupScenario(t, db, 1)
	mid, tid := mids[0], tids[0]

	_, ex := post(t, r, "/api/exchanges", map[string]any{
		"member_id": mid, "ticket_id": tid, "new_session_id": sid,
		"new_seat_id": seat, "idempotency_key": "KF",
	})
	exNo := ex["exchange_no"].(string)
	_, pay := post(t, r, "/api/exchanges/"+exNo+"/pay", map[string]any{"member_id": mid})
	payNo := pay["payment_no"].(string)

	code, fail := post(t, r, "/api/payments/"+payNo+"/mock-pay", map[string]any{"fail": true, "receipt_no": "RCP-F-1"})
	if code != 200 {
		t.Fatalf("fail pay: %d %v", code, fail)
	}
	if res, _ := fail["result"].(map[string]any); res["effect"] != "exchange_failed" {
		t.Fatalf("effect=%v", fail["result"])
	}
	if n := countIssued(db, tid); n != 1 {
		t.Fatalf("old ticket issued=%d want 1", n)
	}
	if n := countOccupancy(db, sid, seat); n != 0 {
		t.Fatalf("new seat occupancy=%d want 0", n)
	}

	// 迟到的支付成功
	code, late := post(t, r, "/api/receipts", map[string]any{
		"receipt_no": "RCP-LATE-1", "payment_no": payNo, "exchange_no": exNo,
		"kind": "pay_success", "status": "success",
		"amount": pay["amount"], "note": "late",
	})
	if code != 200 {
		t.Fatalf("late receipt: %d %v", code, late)
	}
	if late["effect"] != "auto_refund_late_success" {
		t.Fatalf("late effect=%v", late["effect"])
	}
	if n := countIssued(db, tid); n != 1 {
		t.Fatalf("old ticket changed after late success: %d", n)
	}
	var refunds int64
	db.Model(&models.Payment{}).Where("exchange_id IN (SELECT id FROM exchanges WHERE exchange_no = ?) AND type = ? AND status = ?",
		exNo, "refund", models.PaySuccess).Count(&refunds)
	if refunds != 1 {
		t.Fatalf("auto refunds=%d want 1", refunds)
	}
}

package handler

import (
	"errors"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"

	"cinema/service"
)

type Handler struct {
	svc *service.ExchangeService
}

func New(svc *service.ExchangeService) *Handler { return &Handler{svc: svc} }

func (h *Handler) Register(r *gin.Engine) {
	api := r.Group("/api")
	{
		api.GET("/bootstrap", h.bootstrap)
		api.GET("/sessions", h.listSessions)
		api.GET("/sessions/:id/seats", h.seatMap)
		api.GET("/members/:id/tickets", h.listTickets)
		api.GET("/members/:id/coupons", h.listCoupons)
		api.POST("/quote", h.quote)
		api.POST("/exchanges", h.createExchange)
		api.GET("/exchanges/:no", h.getExchange)
		api.POST("/exchanges/:no/pay", h.pay)
		// 本地模拟支付网关
		api.POST("/payments/:no/mock-pay", h.mockPay)
		// 原始回执重放（演示重复/迟到回执）
		api.POST("/receipts", h.rawReceipt)
	}
}

// 简单“当前会员”：演示系统从参数读取，不做鉴权。
func memberID(c *gin.Context) (uint, bool) {
	var raw string
	if v, ok := c.Get("member_id"); ok {
		raw = strconv.Itoa(int(v.(uint)))
	}
	if raw == "" {
		raw = c.Query("member_id")
	}
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "member_id required"})
		return 0, false
	}
	return uint(id), true
}

func (h *Handler) bootstrap(c *gin.Context) {
	mid, ok := memberID(c)
	if !ok {
		return
	}
	tickets, err := h.svc.MemberTickets(mid)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	coupons, err := h.svc.MemberCoupons(mid)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	sessions, err := h.svc.Sessions()
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"member_id": mid, "tickets": tickets, "coupons": coupons, "sessions": sessions})
}

func (h *Handler) listSessions(c *gin.Context) {
	ss, err := h.svc.Sessions()
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, ss)
}

func (h *Handler) seatMap(c *gin.Context) {
	mid, _ := memberID(c)
	sid, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(400, gin.H{"error": "bad session id"})
		return
	}
	var mine uint
	if t := c.Query("ticket_id"); t != "" {
		if id, err := strconv.ParseUint(t, 10, 64); err == nil {
			mine = uint(id)
		}
	}
	seats, err := h.svc.SeatMap(uint(sid), mid, mine)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"seats": seats, "server_time": time.Now()})
}

func (h *Handler) listTickets(c *gin.Context) {
	mid, ok := memberID(c)
	if !ok {
		return
	}
	ts, err := h.svc.MemberTickets(mid)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, ts)
}

func (h *Handler) listCoupons(c *gin.Context) {
	mid, ok := memberID(c)
	if !ok {
		return
	}
	cs, err := h.svc.MemberCoupons(mid)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, cs)
}

type quoteReq struct {
	MemberID     uint  `json:"member_id"`
	TicketID     uint  `json:"ticket_id"`
	NewSessionID uint  `json:"new_session_id"`
	NewSeatID    uint  `json:"new_seat_id"`
	CouponID     *uint `json:"coupon_id"`
}

func (h *Handler) quote(c *gin.Context) {
	var req quoteReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	q, err := h.svc.Quote(req.MemberID, req.TicketID, req.NewSessionID, req.NewSeatID, req.CouponID)
	if err != nil {
		writeBizError(c, err)
		return
	}
	c.JSON(200, q)
}

type createReq struct {
	MemberID       uint   `json:"member_id"`
	TicketID       uint   `json:"ticket_id"`
	NewSessionID   uint   `json:"new_session_id"`
	NewSeatID      uint   `json:"new_seat_id"`
	CouponID       *uint  `json:"coupon_id"`
	IdempotencyKey string `json:"idempotency_key"`
}

var createReqSeq uint64

func (h *Handler) createExchange(c *gin.Context) {
	var req createReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if req.IdempotencyKey == "" {
		// 后端兜底生成（前端也会生成，防双击）
		req.IdempotencyKey = service.NewIdempotencyKey() + "-" + strconv.FormatUint(atomic.AddUint64(&createReqSeq, 1), 10)
	}
	ex, err := h.svc.Create(service.CreateInput{
		MemberID:       req.MemberID,
		TicketID:       req.TicketID,
		NewSessionID:   req.NewSessionID,
		NewSeatID:      req.NewSeatID,
		CouponID:       req.CouponID,
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		writeBizError(c, err)
		return
	}
	detail, err := h.svc.ExchangeDetail(ex.ExchangeNo, req.MemberID)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	detail["idempotency_key"] = req.IdempotencyKey
	c.JSON(200, detail)
}

func (h *Handler) getExchange(c *gin.Context) {
	mid, _ := memberID(c)
	d, err := h.svc.ExchangeDetail(c.Param("no"), mid)
	if err != nil {
		writeBizError(c, err)
		return
	}
	c.JSON(200, d)
}

type payReq struct {
	MemberID uint `json:"member_id"`
}

func (h *Handler) pay(c *gin.Context) {
	var req payReq
	if err := c.ShouldBindJSON(&req); err != nil || req.MemberID == 0 {
		// 允许 query
		if v := c.Query("member_id"); v != "" {
			id, _ := strconv.ParseUint(v, 10, 64)
			req.MemberID = uint(id)
		}
	}
	if req.MemberID == 0 {
		c.JSON(400, gin.H{"error": "member_id required"})
		return
	}
	// 先取改签单判断差额方向（ExchangeDetail 中 diff 是结构体，不是 map）
	detail, err := h.svc.ExchangeDetail(c.Param("no"), req.MemberID)
	if err != nil {
		writeBizError(c, err)
		return
	}
	action := ""
	switch dv := detail["diff"].(type) {
	case service.DiffView:
		action = dv.Action
	case map[string]interface{}:
		action, _ = dv["action"].(string)
	}

	switch action {
	case "none":
		if _, err := h.svc.FinalizeZero(c.Param("no"), req.MemberID); err != nil {
			writeBizError(c, err)
			return
		}
		d2, _ := h.svc.ExchangeDetail(c.Param("no"), req.MemberID)
		c.JSON(200, gin.H{"kind": "zero", "exchange": d2})
		return
	case "refund":
		// 退差无需用户付款：直接走“成功”通道（finalizeSuccess 内自动生成退款单）。
		// 通过一个成功的 0 元语义不易表达，这里调用内部：创建 charge=0 不合法，
		// 因此提供专用入口 FinalizeRefund（见下）。
		if err := h.svc.FinalizeRefund(c.Param("no"), req.MemberID); err != nil {
			writeBizError(c, err)
			return
		}
		d2, _ := h.svc.ExchangeDetail(c.Param("no"), req.MemberID)
		c.JSON(200, gin.H{"kind": "refund", "exchange": d2})
		return
	}

	pay, _, err := h.svc.InitiatePayment(c.Param("no"), req.MemberID)
	if err != nil {
		writeBizError(c, err)
		return
	}
	c.JSON(200, gin.H{
		"kind":        "charge",
		"payment_no":  pay.PaymentNo,
		"amount":      pay.Amount,
		"exchange_no": c.Param("no"),
		"expire_hint": "新座位限时保留中，支付成功即完成改签",
	})
}

type mockPayReq struct {
	Fail      bool   `json:"fail"`
	ReceiptNo string `json:"receipt_no"` // 可指定，用于重放同一回执
	Note      string `json:"note"`
}

func (h *Handler) mockPay(c *gin.Context) {
	var req mockPayReq
	_ = c.ShouldBindJSON(&req)
	paymentNo := c.Param("no")
	// 找到支付单对应改签单
	no, amount, err := h.svc.PaymentInfo(paymentNo)
	if err != nil {
		writeBizError(c, err)
		return
	}
	status := "success"
	kind := "pay_success"
	if req.Fail {
		status, kind = "failed", "pay_failed"
	}
	receiptNo := req.ReceiptNo
	if receiptNo == "" {
		receiptNo = service.GenNo("RCP")
	}
	res, err := h.svc.ApplyReceipt(service.ReceiptInput{
		ReceiptNo:  receiptNo,
		PaymentNo:  paymentNo,
		ExchangeNo: no,
		Kind:       kind,
		Status:     status,
		Amount:     amount,
		Note:       req.Note,
	})
	if err != nil {
		writeBizError(c, err)
		return
	}
	detail, _ := h.svc.ExchangeDetail(no, 0)
	c.JSON(200, gin.H{"result": res, "exchange": detail, "receipt_no": receiptNo})
}

type rawReceiptReq struct {
	ReceiptNo  string `json:"receipt_no"`
	PaymentNo  string `json:"payment_no"`
	ExchangeNo string `json:"exchange_no"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	Amount     int64  `json:"amount"`
	Note       string `json:"note"`
}

func (h *Handler) rawReceipt(c *gin.Context) {
	var req rawReceiptReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if req.Kind == "" {
		req.Kind = "pay_success"
	}
	if req.Status == "" {
		req.Status = "success"
	}
	res, err := h.svc.ApplyReceipt(service.ReceiptInput{
		ReceiptNo:  req.ReceiptNo,
		PaymentNo:  req.PaymentNo,
		ExchangeNo: req.ExchangeNo,
		Kind:       req.Kind,
		Status:     req.Status,
		Amount:     req.Amount,
		Note:       req.Note,
	})
	if err != nil {
		writeBizError(c, err)
		return
	}
	c.JSON(200, res)
}

func writeBizError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrSeatTaken):
		c.JSON(409, gin.H{"error": err.Error(), "code": "seat_taken"})
	case errors.Is(err, service.ErrNotFound):
		c.JSON(404, gin.H{"error": err.Error(), "code": "not_found"})
	case errors.Is(err, service.ErrTicketNotActive):
		c.JSON(409, gin.H{"error": err.Error(), "code": "ticket_not_active"})
	case errors.Is(err, service.ErrExchangeState):
		c.JSON(409, gin.H{"error": err.Error(), "code": "bad_state"})
	case errors.Is(err, service.ErrReservationExpired):
		c.JSON(409, gin.H{"error": err.Error(), "code": "expired"})
	case errors.Is(err, service.ErrSameSeat):
		c.JSON(400, gin.H{"error": err.Error(), "code": "same_seat"})
	case errors.Is(err, service.ErrCouponRedeemed):
		c.JSON(409, gin.H{"error": err.Error(), "code": "coupon_redeemed"})
	case errors.Is(err, service.ErrCouponExpired):
		c.JSON(409, gin.H{"error": err.Error(), "code": "coupon_expired"})
	case errors.Is(err, service.ErrMemberMismatch):
		c.JSON(403, gin.H{"error": err.Error(), "code": "forbidden"})
	case errors.Is(err, service.ErrAmountMismatch):
		c.JSON(400, gin.H{"error": err.Error(), "code": "amount_mismatch"})
	default:
		c.JSON(500, gin.H{"error": err.Error()})
	}
}

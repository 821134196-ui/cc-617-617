package main

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type Handler struct {
	svc *Service
	db  *gorm.DB
}

func yuan(f int64) float64 { return float64(f) / 100 }

func statusErr(err error) (int, string) {
	switch {
	case errors.Is(err, ErrSeatTaken):
		return http.StatusConflict, err.Error()
	case errors.Is(err, ErrSeatIllegal), errors.Is(err, ErrSameSeat):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, ErrTicketIllegal), errors.Is(err, ErrShowNotFound):
		return http.StatusNotFound, err.Error()
	case errors.Is(err, ErrInFlight):
		return http.StatusConflict, err.Error()
	case errors.Is(err, ErrReceiptReplayed), errors.Is(err, ErrReceiptUnknown):
		return http.StatusConflict, err.Error()
	default:
		return http.StatusInternalServerError, err.Error()
	}
}

// GET /api/movies
func (h *Handler) movies(c *gin.Context) {
	var ms []Movie
	h.db.Find(&ms)
	c.JSON(200, ms)
}

// GET /api/showtimes
func (h *Handler) showtimes(c *gin.Context) {
	var ss []Showtime
	h.db.Preload("Movie").Order("start_at").Find(&ss)
	type out struct {
		Showtime
		Status interface{} `json:"-"`
	}
	list := make([]gin.H, 0, len(ss))
	for _, s := range ss {
		list = append(list, gin.H{
			"id": s.ID, "movie_id": s.MovieID, "movie": s.Movie,
			"hall": s.Hall, "start_at": s.StartAt, "base_price": s.BasePrice,
			"rows": s.Rows, "cols": s.Cols,
		})
	}
	c.JSON(200, list)
}

// GET /api/showtimes/:id/seats —— 选座图：sold/held/empty
func (h *Handler) seats(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	var show Showtime
	if err := h.db.First(&show, id).Error; err != nil {
		c.JSON(404, gin.H{"error": "场次不存在"})
		return
	}
	var locks []SeatLock
	h.db.Where("showtime_id = ? AND status IN ?", id, []string{LockHeld, LockSold}).Find(&locks)
	grid := map[string]string{}
	for _, l := range locks {
		grid[l.SeatCode] = l.Status
	}
	c.JSON(200, gin.H{"showtime": show, "seats": grid})
}

// GET /api/member/tickets —— 我的票（含同行订单其余票）
func (h *Handler) myTickets(c *gin.Context) {
	memberID := int64(1) // 本地演示固定会员
	var ts []Ticket
	h.db.Preload("Showtime.Movie").
		Where("member_id = ?", memberID).Order("id").Find(&ts)
	c.JSON(200, ts)
}

// GET /api/member/coupons
func (h *Handler) myCoupons(c *gin.Context) {
	memberID := int64(1)
	var cs []Coupon
	h.db.Where("member_id = ?", memberID).Order("id").Find(&cs)
	c.JSON(200, cs)
}

type previewReq struct {
	TicketID    int64 `json:"ticket_id" binding:"required"`
	NewShowID   int64 `json:"new_show_id" binding:"required"`
}

// POST /api/exchange/preview —— 只计价
func (h *Handler) preview(c *gin.Context) {
	var req previewReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	r, old, show, err := h.svc.Preview(1, req.TicketID, req.NewShowID)
	if err != nil {
		code, msg := statusErr(err)
		c.JSON(code, gin.H{"error": msg})
		return
	}
	c.JSON(200, gin.H{
		"old_ticket":      old,
		"new_showtime":    show,
		"old_face":        r.OldFacePrice,
		"old_discount":    r.OldDiscountAmount,
		"old_paid":        r.OldPaidAmount,
		"new_face":        r.NewFacePrice,
		"new_discount":    r.NewDiscountAmount,
		"new_paid":        r.NewPaidAmount,
		"delta":           r.Delta,
		"settle_direction": settleDir(r.Delta),
		"coupon_basis":    parseBasis(r.DiscountBasis),
	})
}

func settleDir(delta int64) string {
	if delta > 0 {
		return "pay"
	}
	if delta < 0 {
		return "refund"
	}
	return "none"
}

type reserveReq struct {
	TicketID       int64  `json:"ticket_id" binding:"required"`
	NewShowID      int64  `json:"new_show_id" binding:"required"`
	Row            int    `json:"row" binding:"required"`
	Col            int    `json:"col" binding:"required"`
	IdempotencyKey string `json:"idempotency_key"`
}

// POST /api/exchange/reserve
func (h *Handler) reserve(c *gin.Context) {
	var req reserveReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	r, err := h.svc.Reserve(1, req.TicketID, req.NewShowID, req.Row, req.Col, req.IdempotencyKey)
	if err != nil {
		code, msg := statusErr(err)
		c.JSON(code, gin.H{"error": msg})
		return
	}
	c.JSON(200, gin.H{"reservation": r, "ttl_seconds": int(h.svc.ttl.Seconds())})
}

type receiptReq struct {
	ReservationNo string `json:"reservation_no" binding:"required"`
	Success       bool   `json:"success"`
	ReceiptNo     string `json:"receipt_no"`
}

// POST /api/exchange/receipt —— 本地模拟支付/退款回执入口（可重复调用验证幂等）
func (h *Handler) receipt(c *gin.Context) {
	var req receiptReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	res, err := h.svc.Settle(req.ReservationNo, req.Success, req.ReceiptNo)
	if err != nil {
		code, msg := statusErr(err)
		c.JSON(code, gin.H{"error": msg})
		return
	}
	c.JSON(200, gin.H{
		"action":      res.Action,
		"replayed":    res.Replayed,
		"reservation": res.Reservation,
		"server_time": time.Now(),
	})
}

// GET /api/reservations/:no
func (h *Handler) reservation(c *gin.Context) {
	var r Reservation
	if err := h.db.Where("reservation_no = ?", c.Param("no")).First(&r).Error; err != nil {
		c.JSON(404, gin.H{"error": "改签单不存在"})
		return
	}
	c.JSON(200, r)
}

func registerRoutes(r *gin.Engine, h *Handler) {
	api := r.Group("/api")
	api.GET("/movies", h.movies)
	api.GET("/showtimes", h.showtimes)
	api.GET("/showtimes/:id/seats", h.seats)
	api.GET("/member/tickets", h.myTickets)
	api.GET("/member/coupons", h.myCoupons)
	api.POST("/exchange/preview", h.preview)
	api.POST("/exchange/reserve", h.reserve)
	api.POST("/exchange/receipt", h.receipt)
	api.GET("/reservations/:no", h.reservation)
}

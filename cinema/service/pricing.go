package service

import (
	"errors"
	"time"

	"cinema/models"
)

// 计价规则（金额单位：分）：
//  1. 券按“每张票”记录和抵扣；
//  2. 折扣券 discount：newPaid = base * rate / 100（向下取整），discount = base - newPaid；
//  3. 立减券 amount：discount = min(amount, base)，最低 0 元票；
//  4. 已核销(RedeemedAt != nil)或已过期的券不能再次抵扣；
//  5. 改签时原票的优惠依据直接读取原票快照，原券不重算、不重复释放；
//  6. 是否允许在改签时把原券优惠“平移”到新票（券仍未过期）由调用方决定，
//     平移时新票沿用原券的抵扣口径——保证原优惠依据不丢失。

var (
	ErrCouponRedeemed = errors.New("coupon already redeemed")
	ErrCouponExpired  = errors.New("coupon expired")
	ErrCouponType     = errors.New("unknown coupon type")
)

// CalcDiscount 根据券计算单张票抵扣额。
func CalcDiscount(basePrice int64, c *models.Coupon, now time.Time) (int64, error) {
	if c == nil {
		return 0, nil
	}
	if c.RedeemedAt != nil {
		return 0, ErrCouponRedeemed
	}
	if !c.ExpireAt.After(now) {
		return 0, ErrCouponExpired
	}
	return discountOf(basePrice, c.Type, c.Rate, c.Amount), nil
}

// discountOf 纯计算，不校验券状态。
func discountOf(basePrice int64, couponType string, rate int, amount int64) int64 {
	switch couponType {
	case "discount":
		if rate <= 0 || rate > 100 {
			return 0
		}
		paid := basePrice * int64(rate) / 100
		d := basePrice - paid
		if d < 0 {
			d = 0
		}
		if d > basePrice {
			d = basePrice
		}
		return d
	case "amount":
		if amount < 0 {
			return 0
		}
		if amount > basePrice {
			return basePrice
		}
		return amount
	default:
		return 0
	}
}

// Quote 改签报价。原票信息从票记录快照读取，新票基础价来自新场次。
type Quote struct {
	OldBasePrice int64
	OldDiscount  int64
	OldPaid      int64
	NewBasePrice int64
	NewDiscount  int64
	NewPaid      int64
	DiffAmount   int64 // >0 会员补交; <0 系统退回
	// 新票使用的券：要么是沿用的原券（未过期），要么是新选券，要么为 nil（无优惠/原价改签）
	NewCoupon *models.Coupon
}

// QuoteExchange 计算改签差额。
//   - oldTicket: 原票（含原优惠快照，永久依据）
//   - newBasePrice: 新场次票价
//   - newCoupon: 用户为新票选择的券；传 nil 时尝试沿用原券(carryOriginal=true)
//   - carryOriginal: 无新券时，原券未过期则按原券口径平移优惠
func QuoteExchange(oldTicket *models.Ticket, newBasePrice int64, newCoupon *models.Coupon, carryOriginal bool, now time.Time) (Quote, error) {
	q := Quote{
		OldBasePrice: oldTicket.BasePrice,
		OldDiscount:  oldTicket.Discount,
		OldPaid:      oldTicket.PaidAmount,
		NewBasePrice: newBasePrice,
	}

	if newCoupon != nil {
		d, err := CalcDiscount(newBasePrice, newCoupon, now)
		if err != nil {
			return Quote{}, err
		}
		q.NewDiscount = d
		q.NewCoupon = newCoupon
	} else if carryOriginal && oldTicket.CouponID != nil && oldTicket.CouponType != "" {
		// 平移原券：用原券快照参数计算新价；若原券已过期则不能再抵，按原价。
		// （原券在原票上已经核销，这里不再次核销，只继承“优惠依据”。）
		q.NewDiscount = discountOf(newBasePrice, oldTicket.CouponType, oldTicket.CouponRate, oldTicket.CouponAmount)
	}

	q.NewPaid = newBasePrice - q.NewDiscount
	if q.NewPaid < 0 {
		q.NewPaid = 0
	}
	q.DiffAmount = q.NewPaid - q.OldPaid
	return q, nil
}

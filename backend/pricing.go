package main

import (
	"encoding/json"
	"math"
)

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func parseBasis(s string) *DiscountBasis {
	if s == "" {
		return nil
	}
	var b DiscountBasis
	if err := json.Unmarshal([]byte(s), &b); err != nil {
		return nil
	}
	return &b
}

// discountFor 按优惠依据在给定票面价上重算抵扣额（单位：分）。
// percent：折扣率，Value=90 表示 9 折，抵扣 = 票面 * (100-Value)/100，四舍五入到分。
// fixed  ：立减券，票面达到 MinSpend 才可⽤，抵扣不超过票面本身。
func discountFor(b *DiscountBasis, face int64) int64 {
	if b == nil || face <= 0 {
		return 0
	}
	switch b.Kind {
	case "percent":
		if b.Value <= 0 || b.Value >= 100 {
			return 0
		}
		d := float64(face) * float64(100-b.Value) / 100
		d = math.Round(d)
		if d < 0 {
			d = 0
		}
		if d > float64(face) {
			d = float64(face)
		}
		return int64(d)
	case "fixed":
		if face < b.MinSpend {
			return 0
		}
		if b.Value > face {
			return face
		}
		return b.Value
	default:
		return 0
	}
}

// quote 基于原票与新场次，给出改签报价。优惠依据直接取自原票快照。
func quote(old *Ticket, newShow *Showtime) (newFace, newDiscount, newPaid, delta int64, basis *DiscountBasis) {
	basis = parseBasis(old.DiscountBasis)
	newFace = newShow.BasePrice
	newDiscount = discountFor(basis, newFace)
	newPaid = newFace - newDiscount
	delta = newPaid - old.PaidAmount
	return
}

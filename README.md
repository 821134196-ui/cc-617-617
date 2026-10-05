# 🎬 电影院会员券 · 跨场次改签差额系统

会员改换场次、或只替同行人改一张票时，先看懂新旧票价与券优惠，再确认补交/退回多少钱。

- **后端**：Go + Gin + GORM（SQLite，纯 Go 驱动 `glebarez/sqlite`，无需 CGO）
- **前端**：Vue 3（本地化 vendor，无需 npm 构建），选座图 + 原票/新票/券/补退四栏对照
- **支付**：本地模拟回执接口（成功 / 失败 / 重复 / 迟到都可在页面上点出来）
- **一条命令启动**：`./run.sh`，打开 http://localhost:8080

## 快速开始

```bash
./run.sh            # 启动（首次自动装 Go、拉依赖）
./run.sh --reset    # 重置演示数据
./run.sh test       # 运行全部验证测试（-race 竞态检测）
```

演示数据：会员 1 的同行订单 **O1001** 有 3 张票（《星海彼岸》14:00 场，座位 C4/C5/C6）：
票1 用 **9折券**（实付 ¥40.50）、票2 用 **满40减10券**（实付 ¥35.00）、票3 无券（¥45.00）。
另有一张可用的新客 8 折券和一张已过期券用于展示券状态。

## 改签流程

1. 左侧选择要改的票（可只选同行订单中的一张）；
2. 中间选择新场次、新座位；右侧实时展示对照：
   原票面/券抵扣/原实付 ‖ 新票面/按原依据重算的券抵扣/新实付 ‖ **应补或应退差额**；
3. 「限时预留新座位并锁单」→ 新座位变 `held`，进入 TTL 倒计时（默认 120 秒）；
4. 模拟渠道回执：
   - **补款/退款成功**：单事务内出新票 → 旧票 `exchanged` → 新座 `sold` → **原座释放**，改签完成；
   - **补款/退款失败**：新座位 `released`、改签单 `failed`，**原订单与原座位原样保留**，可重新改签；
   - **重复回执**：支付单只能 `pending → 终态` 一次，重复/并发回执返回 `replayed`，不触发第二次改签；
   - **预留超时后才到的成功回执**：改签单已 `expired`、座位已释放，系统**绝不二次放行**，
     对迟到的补款自动生成一笔原路退款（退款回执迟到则记异常备注）。

## 关键设计

| 关注点 | 实现 |
| --- | --- |
| 并发抢座 | `seat_locks` 上的**部分唯一索引** `(showtime_id, seat_code) WHERE status IN ('held','sold')`，INSERT 冲突即抢座失败 |
| 新座先预留 | held 锁 + `reservations.expires_at`，后台每 2s 扫描超时释放 |
| 支付成功才放原座 | 出票、旧票 exchanged、新座 sold、旧座 released 在**同一个 DB 事务**里 |
| 单张改签 | 改签只针对选中的一张票；同行其余票、座位、订单状态完全不动 |
| 券按票记录 | 每张票存 `coupon_id` 与 `discount_basis`（券种/折扣值/门槛/描述 JSON 快照）；改签迁移券挂载点，**不重新核销**，已使用/过期券无法再次抵扣 |
| 优惠依据保留 | 新票抵扣 = 用原票快照依据在**新票面价**上重算（9折到 58 元场抵扣 5.80 元；立减券按新价重新校验门槛） |
| 差额 | `delta = 新实付 − 原实付`，正补负退；每方向（pay/refund）在支付表上有**部分唯一约束**，只允许一笔 |
| 回执幂等 | 条件更新 `WHERE status='pending'`，`RowsAffected=0` 即重复；同号回执安全重试，异号回执明确拒绝 |
| 金额精度 | 全程 `int64` 分，杜绝浮点误差 |

## API

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/member/tickets` | 我的票（含同行票） |
| GET | `/api/member/coupons` | 我的券 |
| GET | `/api/showtimes` / `/api/showtimes/:id/seats` | 场次 / 座位占用图 |
| POST | `/api/exchange/preview` | `{ticket_id,new_show_id}` 只计价 |
| POST | `/api/exchange/reserve` | `{ticket_id,new_show_id,row,col,idempotency_key}` 限时占位锁单 |
| POST | `/api/exchange/receipt` | `{reservation_no,success,receipt_no}` 模拟支付/退款回执 |
| GET | `/api/reservations/:no` | 改签单状态（前端轮询超时） |

## 验证的场景（`backend/service_test.go`）

- `TestConcurrentSeatGrab`：两个会员并发抢同一座位，恰好 1 成 1 败，库里只有 1 把 held 锁；
- `TestSingleTicketExchangeKeepsOthers`：只改票1，票2/3 保留原场次座位，券依据随新票重算并迁移；
- `TestPayFailureRecovery`：补款失败后原票/原座都在、新座释放无新票、可重新改签成功；
- `TestDuplicateReceiptIdempotent`：10 个并发成功回执只有 1 个 `completed`，其余 `replayed`；
- `TestExpireThenLateReceipt`：超时释放后迟到补款自动退款、绝不出票，之后可正常重新改签；
- `TestPricingBasis` / `TestCouponRedemptionRules`：折扣重算、立减门槛、已用/过期券规则。

## 目录

```
backend/
  main.go         入口 + 超时回收 goroutine + embed 前端
  models.go       GORM 模型
  db.go           SQLite(WAL)、部分唯一索引、演示种子
  pricing.go      优惠依据与差额计算（分）
  service.go      预留 / 幂等结算 / 超时回收（核心事务）
  handlers.go     Gin API
  service_test.go 验证测试
  web/dist/       Vue 单页（vendor 本地 vue.global.prod.js）
run.sh            一条命令
```

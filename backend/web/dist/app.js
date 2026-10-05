const { createApp } = Vue;

createApp({
  data() {
    return {
      loading: true,
      tickets: [],
      showtimes: [],
      coupons: [],
      selectedTicketId: null,
      selectedShowId: null,
      seatMap: {},            // code -> held/sold
      seatLoading: false,
      selectedSeat: null,     // {row,col,code}
      preview: null,
      previewErr: "",
      reserving: false,
      reservation: null,      // 当前改签单
      ttlSeconds: 120,
      remain: 0,
      timer: null,
      pollTimer: null,
      result: null,           // {type, text}
      idemKey: "",
      busy: false,
    };
  },
  computed: {
    selectedTicket() {
      return this.tickets.find(t => t.id === this.selectedTicketId) || null;
    },
    selectedShow() {
      return this.showtimes.find(s => s.id === this.selectedShowId) || null;
    },
    couponOfTicket() {
      const t = this.selectedTicket;
      if (!t || !t.coupon_id) return null;
      return this.coupons.find(c => c.id === t.coupon_id) || null;
    },
    seatRows() {
      if (!this.selectedShow) return [];
      const rows = [];
      for (let r = 1; r <= this.selectedShow.rows; r++) {
        const cells = [];
        for (let c = 1; c <= this.selectedShow.cols; c++) {
          const code = this.seatCode(r, c);
          cells.push({ row: r, col: c, code, status: this.seatMap[code] || "available" });
        }
        rows.push(cells);
      }
      return rows;
    },
    gridStyle() {
      const n = this.selectedShow ? this.selectedShow.cols : 8;
      return { gridTemplateColumns: `22px repeat(${n}, 34px)` };
    },
    settleKind() {
      if (!this.preview) return "none";
      return this.preview.settle_direction; // pay / refund / none
    },
    canReserve() {
      return this.selectedTicket && this.selectedShow && this.selectedSeat &&
        this.preview && !this.reservation && !this.reserving;
    },
  },
  async mounted() {
    await this.refreshAll();
    this.loading = false;
  },
  beforeUnmount() {
    clearInterval(this.timer);
    clearInterval(this.pollTimer);
  },
  methods: {
    seatCode(r, c) { return String.fromCharCode(64 + r) + c; },
    fmt(cents) {
      return "¥" + (cents / 100).toFixed(2);
    },
    fmtTime(t) {
      const d = new Date(t);
      return `${d.getMonth() + 1}/${d.getDate()} ${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
    },
    badge(status) {
      return { active: "有效", exchanged: "已改签", refunded: "已退票" }[status] || status;
    },
    couponBadge(s) {
      return { available: "未使用", used: "已使用", expired: "已过期" }[s] || s;
    },
    async api(path, opts = {}) {
      const res = await fetch("/api" + path, {
        headers: { "Content-Type": "application/json" },
        ...opts,
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(data.error || ("请求失败 " + res.status));
      return data;
    },
    async refreshAll() {
      const [tickets, shows, coupons] = await Promise.all([
        this.api("/member/tickets"),
        this.api("/showtimes"),
        this.api("/member/coupons"),
      ]);
      this.tickets = tickets;
      this.showtimes = shows;
      this.coupons = coupons;
      if (this.selectedShowId) this.loadSeats(this.selectedShowId, true);
    },

    selectTicket(t) {
      if (t.status !== "active" || this.reservation) return;
      this.selectedTicketId = t.id;
      this.selectedSeat = null;
      this.preview = null;
      this.previewErr = "";
      if (this.selectedShowId) this.tryPreview();
    },
    async selectShow(s) {
      if (this.reservation) return;
      const t = this.selectedTicket;
      if (t && t.showtime_id === s.id) return; // 不能选原场次
      this.selectedShowId = s.id;
      this.selectedSeat = null;
      this.preview = null;
      this.previewErr = "";
      await this.loadSeats(s.id);
      this.tryPreview();
    },
    async loadSeats(id, silent = false) {
      if (!silent) this.seatLoading = true;
      try {
        const data = await this.api(`/showtimes/${id}/seats`);
        this.seatMap = data.seats || {};
      } finally {
        this.seatLoading = false;
      }
    },
    async clickSeat(cell) {
      if (this.reservation || cell.status !== "available") return;
      this.selectedSeat = { row: cell.row, col: cell.col, code: cell.code };
      await this.tryPreview();
    },
    async tryPreview() {
      if (!this.selectedTicket || !this.selectedShow || !this.selectedSeat) return;
      try {
        this.preview = await this.api("/exchange/preview", {
          method: "POST",
          body: JSON.stringify({
            ticket_id: this.selectedTicketId,
            new_show_id: this.selectedShowId,
          }),
        });
        this.previewErr = "";
      } catch (e) {
        this.preview = null;
        this.previewErr = e.message;
      }
    },

    async reserve() {
      if (!this.canReserve) return;
      this.reserving = true;
      this.result = null;
      this.idemKey = "web-" + this.selectedTicketId + "-" + Date.now() + "-" + Math.random().toString(36).slice(2, 8);
      try {
        const data = await this.api("/exchange/reserve", {
          method: "POST",
          body: JSON.stringify({
            ticket_id: this.selectedTicketId,
            new_show_id: this.selectedShowId,
            row: this.selectedSeat.row,
            col: this.selectedSeat.col,
            idempotency_key: this.idemKey,
          }),
        });
        this.reservation = data.reservation;
        this.ttlSeconds = data.ttl_seconds;
        this.remain = Math.max(0, Math.round((new Date(data.reservation.expires_at) - new Date()) / 1000));
        this.result = { type: "info", text: `新座位 ${data.reservation.new_seat_code} 已限时预留，${this.ttlSeconds} 秒内完成结算，超时自动释放。` };
        this.startTimers();
        await this.loadSeats(this.selectedShowId, true);
      } catch (e) {
        this.result = { type: "err", text: "预留失败：" + e.message };
        await this.loadSeats(this.selectedShowId, true);
      } finally {
        this.reserving = false;
      }
    },
    startTimers() {
      clearInterval(this.timer);
      clearInterval(this.pollTimer);
      this.timer = setInterval(() => {
        this.remain = Math.max(0, this.remain - 1);
      }, 1000);
      // 每 2 秒同步改签单真实状态（处理后台超时回收）
      this.pollTimer = setInterval(async () => {
        if (!this.reservation) return;
        try {
          const r = await this.api("/reservations/" + this.reservation.reservation_no);
          if (r.status !== "reserved") {
            await this.onReservationResolved(r);
          } else {
            this.remain = Math.max(0, Math.round((new Date(r.expires_at) - new Date()) / 1000));
          }
        } catch (_) { /* 轮询失败忽略 */ }
      }, 2000);
    },
    async onReservationResolved(r) {
      this.reservation = r;
      clearInterval(this.timer);
      clearInterval(this.pollTimer);
      const map = {
        completed: { type: "ok", text: "改签完成：新票已出票，原座位已释放。" },
        failed: { type: "err", text: "结算失败：新座位已释放，原订单与原座位完整保留，可重新改签。" },
        expired: { type: "err", text: "预留超时：新座位已被释放，原票不受影响。" },
      };
      this.result = map[r.status] || { type: "info", text: "改签单状态：" + r.status };
      this.reservation = null;
      this.selectedSeat = null;
      this.preview = null;
      await this.refreshAll();
    },

    // 模拟支付/退款渠道回执
    async sendReceipt(success, label, sameReceipt = false) {
      if (!this.reservation || this.busy) return;
      this.busy = true;
      try {
        const receiptNo = sameReceipt && this._lastReceipt
          ? this._lastReceipt
          : (this.settleKind === "refund" ? "RFD-" : "PAY-") + Date.now();
        if (!sameReceipt) this._lastReceipt = receiptNo;
        const data = await this.api("/exchange/receipt", {
          method: "POST",
          body: JSON.stringify({
            reservation_no: this.reservation.reservation_no,
            success,
            receipt_no,
          }),
        });
        const actionText = {
          completed: "✅ 改签完成（本次回执触发改签）",
          failed: "❌ 回执为失败：已释放新座位、保留原票",
          replayed: "↩️ 重复回执：已忽略，未触发第二次改签（幂等）",
          expired_late_refunded: "⏰ 预留已超时：迟到回执未放行，款项自动原路退回",
          expired_noted: "⏰ 预留已超时：失败回执仅记账，原票保留",
        }[data.action] || data.action;
        const type = data.action === "completed" ? "ok"
          : data.action === "failed" || data.action.includes("expired") ? "err" : "info";
        this.result = { type, text: `${label} → ${actionText}` };
        if (["completed", "failed"].includes(data.action) || data.action.startsWith("expired")) {
          await this.onReservationResolved(data.reservation);
        }
      } catch (e) {
        this.result = { type: "err", text: label + " 被拒绝：" + e.message };
      } finally {
        this.busy = false;
      }
    },
  },
  template: `
  <div>
    <header>
      <h1>🎬 星映会员 · 跨场次改签</h1>
      <span class="sub">先看清新旧票价与券优惠，再确认补/退差额 · 支付成功后才释放原座</span>
    </header>

    <div v-if="loading" style="padding:40px;text-align:center;color:#9aa0c3">加载中…</div>

    <div v-else class="layout">
      <!-- 左：我的票 -->
      <div class="panel">
        <h2>我的订单 O1001 <span class="tag">同行 3 张，可只改一张</span></h2>
        <div v-for="t in tickets" :key="t.id"
             class="ticket"
             :class="{ selected: t.id===selectedTicketId, dim: t.status!=='active' || reservation }"
             @click="selectTicket(t)">
          <div class="row1">
            <span>{{ t.showtime.movie.title }}</span>
            <span class="badge" :class="t.status">{{ badge(t.status) }}</span>
          </div>
          <div class="row2">
            <span>{{ t.showtime.hall }}</span>
            <span>{{ fmtTime(t.showtime.start_at) }}</span>
            <span>座 {{ t.seat_code }}</span>
          </div>
          <div class="row3" v-if="t.discount_amount>0">
            🎟 券抵扣 {{ fmt(t.discount_amount) }} · 实付 {{ fmt(t.paid_amount) }}
          </div>
          <div class="row3" v-else>实付 {{ fmt(t.paid_amount) }}</div>
        </div>
        <div class="coupon-list">
          <div class="hint">我的会员券</div>
          <div class="ci" v-for="c in coupons" :key="c.id">
            <span>{{ c.name }}（{{ c.code }}）</span>
            <span class="badge" :class="c.status">{{ couponBadge(c.status) }}</span>
          </div>
        </div>
      </div>

      <!-- 中：场次与座位 -->
      <div class="panel">
        <h2>② 选择新场次与新座位 <span class="tag" v-if="!selectedTicket">请先在左侧选择要改签的票</span></h2>
        <div v-for="s in showtimes" :key="s.id"
             class="show"
             :class="{ selected: s.id===selectedShowId, disabled: selectedTicket && selectedTicket.showtime_id===s.id || reservation }"
             @click="selectShow(s)">
          <div>
            <div class="t">{{ s.movie.title }} · {{ s.hall }}</div>
            <div class="m">{{ fmtTime(s.start_at) }}
              <span v-if="selectedTicket && selectedTicket.showtime_id===s.id">（原场次，不可选）</span>
            </div>
          </div>
          <div class="p">票价 {{ fmt(s.base_price) }}</div>
        </div>

        <template v-if="selectedShow">
          <div class="screen" style="margin-top:22px"><span>银 幕</span></div>
          <div class="seat-grid" :style="{gridTemplateColumns:'repeat('+selectedShow.cols+', 34px)'}">
            <template v-for="(row, ri) in seatRows" :key="ri">
              <div v-for="cell in row" :key="cell.code"
                   class="seat"
                   :class="{ selected: selectedSeat && selectedSeat.code===cell.code,
                             available: cell.status==='available',
                             sold: cell.status==='sold', held: cell.status==='held' }"
                   @click="clickSeat(cell)">
                {{ cell.code }}
              </div>
            </template>
          </div>
          <div class="legend">
            <span><i class="a"></i>可选</span>
            <span><i class="s"></i>已售</span>
            <span><i class="h"></i>预留中</span>
            <span><i class="m"></i>已选</span>
          </div>
          <p class="hint" style="text-align:center;margin-top:10px" v-if="seatLoading">座位状态刷新中…</p>
        </template>
      </div>

      <!-- 右：对照与结算 -->
      <div class="panel">
        <h2>③ 原票 / 新票 / 券 / 补退对照</h2>

        <div v-if="!selectedTicket" class="hint">请先选择一张要改签的票。</div>

        <template v-else>
          <div class="compare">
            <div class="mini">
              <div class="h">原票</div>
              <div class="title">{{ selectedTicket.showtime.movie.title }}</div>
              <div class="line"><span>{{ selectedTicket.showtime.hall }} {{ fmtTime(selectedTicket.showtime.start_at) }}</span></div>
              <div class="line"><span>座位</span><b>{{ selectedTicket.seat_code }}</b></div>
              <div class="line"><span>票面价</span><b>{{ fmt(selectedTicket.face_price) }}</b></div>
              <div class="line"><span>券优惠</span><b :class="{'free':selectedTicket.discount_amount>0}">-{{ fmt(selectedTicket.discount_amount) }}</b></div>
              <div class="line"><span>原实付</span><b>{{ fmt(selectedTicket.paid_amount) }}</b></div>
            </div>
            <div class="mini new">
              <div class="h">新票</div>
              <template v-if="selectedShow">
                <div class="title">{{ selectedShow.movie.title }}</div>
                <div class="line"><span>{{ selectedShow.hall }} {{ fmtTime(selectedShow.start_at) }}</span></div>
                <div class="line"><span>座位</span><b>{{ selectedSeat ? selectedSeat.code : '—' }}</b></div>
                <div class="line"><span>票面价</span><b>{{ fmt(selectedShow.base_price) }}</b></div>
                <div class="line"><span>券优惠（按原依据）</span>
                  <b :class="{'free':preview && preview.new_discount>0}">
                    {{ preview ? '-'+fmt(preview.new_discount) : '—' }}
                  </b>
                </div>
                <div class="line"><span>新实付</span><b>{{ preview ? fmt(preview.new_paid) : '—' }}</b></div>
              </template>
              <div v-else class="hint">请选择新场次</div>
            </div>
          </div>

          <div class="coupon-box">
            <div class="name">
              🎟 已使用会员券
              <span v-if="couponOfTicket" class="badge used">使用中 · 随票迁移</span>
              <span v-else class="badge available">无券（原价票）</span>
            </div>
            <div v-if="couponOfTicket" class="desc">
              {{ couponOfTicket.name }}（{{ couponOfTicket.code }}）。改签不重新核销券，
              仅把同一优惠依据带到新票；已使用/已过期的券不能再次抵扣。
            </div>
            <div v-if="preview && preview.coupon_basis" class="basis">
              优惠依据快照：{{ preview.coupon_basis.description }}
            </div>
            <div v-if="previewErr" class="result err" style="margin-top:8px">{{ previewErr }}</div>
          </div>

          <div v-if="preview" class="settle">
            <div class="hint">补退明细：新实付 {{ fmt(preview.new_paid) }} − 原实付 {{ fmt(preview.old_paid) }}</div>
            <div class="big" :class="settleKind">
              <span v-if="settleKind==='pay'">需补交 {{ fmt(preview.delta) }}</span>
              <span v-else-if="settleKind==='refund'">应退回 {{ fmt(-preview.delta) }}</span>
              <span v-else>差额为 ¥0.00，无需结算</span>
            </div>
            <div class="calc">
              新票面 {{ fmt(preview.new_face) }} − 新券抵扣 {{ fmt(preview.new_discount) }}
              − 原实付 {{ fmt(preview.old_paid) }}
            </div>
          </div>

          <div v-if="!reservation">
            <button :disabled="!canReserve" @click="reserve">
              {{ reserving ? '预留中…' : '限时预留新座位并锁单' }}
            </button>
            <ol class="steps">
              <li>新座位先限时预留，原座位暂不释放；</li>
              <li>{{ settleKind==='refund' ? '退款' : '补款' }}成功后才出新票、释放原座；</li>
              <li>只改当前这一张，同行其余票留在原场次。</li>
            </ol>
          </div>

          <div v-else>
            <div class="countdown">⏳ 新座位 {{ reservation.new_seat_code }} 预留剩余 {{ remain }} 秒（超时自动释放）</div>

            <!-- 补款场景 -->
            <template v-if="settleKind==='pay'">
              <button @click="sendReceipt(true, '模拟补款成功回执')">模拟补款成功（完成改签）</button>
              <div class="btn-row">
                <button class="danger" @click="sendReceipt(false, '模拟补款失败回执')">模拟补款失败</button>
                <button class="ghost" @click="sendReceipt(true, '重复补款回执', true)">再发一次成功回执</button>
              </div>
            </template>
            <!-- 退款场景 -->
            <template v-else-if="settleKind==='refund'">
              <button @click="sendReceipt(true, '模拟退款成功回执')">模拟退款成功（完成改签）</button>
              <div class="btn-row">
                <button class="danger" @click="sendReceipt(false, '模拟退款失败回执')">模拟退款失败</button>
                <button class="ghost" @click="sendReceipt(true, '重复退款回执', true)">再发一次成功回执</button>
              </div>
            </template>
            <template v-else>
              <button @click="sendReceipt(true, '零差额确认')">确认改签（差额 ¥0）</button>
            </template>

            <p class="hint" style="margin-top:10px">
              回执由本地模拟支付接口产生，可重复点击验证：成功回执只生效一次，
              晚到或重复到达都不会触发第二次改签。
            </p>
          </div>

          <div v-if="result" class="result" :class="result.type">{{ result.text }}</div>
        </template>
      </div>
    </div>
  </div>
  `,
}).mount("#app");

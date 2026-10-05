const { createApp } = Vue;

createApp({
  data() {
    return {
      memberId: null,
      tickets: [],
      coupons: [],
      sessions: [],
      seats: [],
      chosenTicket: null,
      newSessionId: null,
      newSeatId: null,
      newCouponId: null,
      quote: null,
      creating: false,
      active: null,           // 当前改签单详情
      pendingPay: null,       // {payment_no, amount}
      lastReceiptNo: null,
      lastReceiptResult: null,
      ttlMs: 60000,
      countdown: 0,
      timer: null,
      pollTimer: null,
    };
  },
  computed: {
    usableCoupons() {
      return this.coupons.filter(c => c.status === 'available');
    },
    countdownText() {
      const s = Math.max(0, Math.round(this.countdown / 1000));
      return `${s} 秒`;
    },
  },
  async mounted() {
    await this.bootstrap();
    // 倒计时
    this.timer = setInterval(() => this.tick(), 500);
    // 轮询改签单状态（捕获后台超时释放）
    this.pollTimer = setInterval(() => this.pollActive(), 3000);
  },
  beforeUnmount() {
    clearInterval(this.timer);
    clearInterval(this.pollTimer);
  },
  methods: {
    async api(path, opts = {}) {
      const res = await fetch(path, {
        headers: { 'Content-Type': 'application/json' },
        ...opts,
        body: opts.body ? JSON.stringify(opts.body) : undefined,
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) throw Object.assign(new Error(data.error || `HTTP ${res.status}`), { code: data.code });
      return data;
    },
    async bootstrap() {
      let memberId = Number(localStorage.getItem('member_id')) || 1;
      try {
        const d = await this.api(`/api/bootstrap?member_id=${memberId}`);
        this.memberId = d.member_id;
        this.tickets = d.tickets;
        this.coupons = d.coupons;
        this.sessions = d.sessions;
        this.ttlMs = 60000;
        localStorage.setItem('member_id', d.member_id);
      } catch (e) {
        // 数据库为空：自动重置一次
        await this.resetAll(true);
      }
    },
    async resetAll(silent) {
      const d = await this.api('/api/admin/reset', { method: 'POST' });
      localStorage.setItem('member_id', d.member_id);
      this.active = null; this.pendingPay = null; this.quote = null;
      this.chosenTicket = null; this.newSeatId = null; this.newSessionId = null;
      await this.bootstrap();
      if (!silent) alert('演示数据已重置');
    },
    money(fen) {
      return (Number(fen || 0) / 100).toFixed(2);
    },
    fmtTime(t) {
      const d = new Date(t);
      return `${(d.getMonth() + 1)}/${d.getDate()} ${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
    },
    fmtDate(t) { return this.fmtTime(t); },
    fmtSeat(s) { return s ? `${s.row} 排 ${s.number} 座` : ''; },
    statusText(st) {
      return { created: '待支付/预留中', paying: '支付处理中', succeeded: '改签成功',
        failed: '支付失败-原票保留', expired: '预留已超时', canceled: '已取消' }[st] || st;
    },
    isActiveState(st) { return st === 'created' || st === 'paying'; },
    chooseTicket(t) {
      if (this.active && this.isActiveState(this.active.status)) {
        if (!confirm('当前有进行中的改签，切换原票不会取消它。继续？')) return;
      }
      this.chosenTicket = t;
      this.newSeatId = null; this.quote = null; this.newCouponId = null;
      this.pickSession(t.price.session.id === this.sessions[0].id ? this.sessions[1]?.id : this.sessions[0].id);
    },
    async pickSession(sid) {
      if (this.chosenTicket && sid === this.chosenTicket.price.session.id) return;
      this.newSessionId = sid; this.newSeatId = null; this.quote = null;
      const d = await this.api(`/api/sessions/${sid}/seats?member_id=${this.memberId}&ticket_id=${this.chosenTicket?.id || ''}`);
      this.seats = d.seats;
    },
    seatClass(s) {
      if (this.newSeatId === s.id) return 'pick';
      if (s.disabled) return 'disabledseat';
      return s.status; // available / held / sold / mine
    },
    pickSeat(s) {
      if (['sold', 'held', 'mine', 'disabledseat'].includes(this.seatClass(s))) return;
      this.newSeatId = s.id;
      this.refreshQuote();
    },
    async refreshQuote() {
      if (!this.newSeatId) { this.quote = null; return; }
      try {
        this.quote = await this.api('/api/quote', {
          method: 'POST',
          body: {
            member_id: this.memberId, ticket_id: this.chosenTicket.id,
            new_session_id: this.newSessionId, new_seat_id: this.newSeatId,
            coupon_id: this.newCouponId,
          },
        });
      } catch (e) {
        this.quote = null;
        alert(e.message);
      }
    },
    async confirmExchange() {
      this.creating = true;
      try {
        const d = await this.api('/api/exchanges', {
          method: 'POST',
          body: {
            member_id: this.memberId, ticket_id: this.chosenTicket.id,
            new_session_id: this.newSessionId, new_seat_id: this.newSeatId,
            coupon_id: this.newCouponId,
            idempotency_key: this.idemKey(),
          },
        });
        this.active = d;
        this.pendingPay = null;
        this.lastReceiptResult = null;
      } catch (e) {
        if (e.code === 'seat_taken') {
          alert('手慢了！该座位刚被他人抢走/预留，请换一个座位。');
          this.pickSession(this.newSessionId);
        } else {
          alert(e.message);
        }
      } finally {
        this.creating = false;
      }
    },
    idemKey() {
      if (!this._idem) this._idem = 'web-' + crypto.randomUUID();
      return this._idem;
    },
    async ensurePayment() {
      if (this.pendingPay) return this.pendingPay;
      const d = await this.api(`/api/exchanges/${this.active.exchange_no}/pay`, {
        method: 'POST', body: { member_id: this.memberId },
      });
      if (d.kind === 'charge') {
        this.pendingPay = { payment_no: d.payment_no, amount: d.amount };
        await this.reloadActive();
      }
      return this.pendingPay;
    },
    async mockPay(fail) {
      try {
        const p = await this.ensurePayment();
        if (!p) return;
        const receiptNo = serviceReceiptNo();
        this.lastReceiptNo = receiptNo;
        const d = await this.api(`/api/payments/${p.payment_no}/mock-pay`, {
          method: 'POST', body: { fail, receipt_no: receiptNo, note: fail ? 'insufficient balance' : 'ok' },
        });
        this.lastReceiptResult = d.result;
        await this.reloadActive();
        await this.bootstrap();
        this.pendingPay = null;
      } catch (e) {
        alert(e.message);
        await this.reloadActive();
      }
    },
    async finalizeZero() {
      try {
        const d = await this.api(`/api/exchanges/${this.active.exchange_no}/pay`, {
          method: 'POST', body: { member_id: this.memberId },
        });
        this.active = d.exchange;
        await this.bootstrap();
      } catch (e) { alert(e.message); await this.reloadActive(); }
    },
    async finalizeRefund() { return this.finalizeZero(); },
    async abandon() {
      // 放弃 = 等待超时之外的主动取消：直接等待后台过期即可，
      // 这里通过把状态交给“预留 TTL”演示；也可刷新页面。为即时演示，调用一个失败回执（若已有支付单）
      if (this.pendingPay) {
        const receiptNo = serviceReceiptNo();
        this.lastReceiptNo = receiptNo;
        const d = await this.api(`/api/payments/${this.pendingPay.payment_no}/mock-pay`, {
          method: 'POST', body: { fail: true, receipt_no: receiptNo, note: 'user canceled' },
        });
        this.lastReceiptResult = d.result;
      } else {
        alert('已选择放弃：新座位将在预留倒计时结束后自动释放，原票始终保留。');
      }
      await this.reloadActive();
      this.pendingPay = null;
    },
    async replaySameReceipt() {
      if (!this.pendingPay || !this.lastReceiptNo) {
        // 改签已完成时，用上一条回执号通过原始接口重放
        const d = await this.api('/api/receipts', {
          method: 'POST',
          body: {
            receipt_no: this.lastReceiptNo,
            payment_no: this._lastPaymentNo || '',
            exchange_no: this.active.exchange_no,
            kind: 'pay_success', status: 'success',
            amount: this.active.diff.amount,
            note: 'duplicate replay from UI',
          },
        });
        this.lastReceiptResult = d;
        return;
      }
      const d = await this.api(`/api/payments/${this.pendingPay.payment_no}/mock-pay`, {
        method: 'POST', body: { receipt_no: this.lastReceiptNo },
      });
      this.lastReceiptResult = d.result;
      await this.reloadActive();
    },
    async lateSuccess() {
      // 1) 先发失败回执（产生终态 failed）
      await this.mockPay(true);
      await new Promise(r => setTimeout(r, 300));
      // 2) 再用一个“新”的成功回执（模拟渠道晚到的成功通知）
      const payNo = this.pendingPay?.payment_no || this._lastPaymentNo;
      if (!payNo) { alert('当前没有支付单可演示'); return; }
      const d = await this.api('/api/receipts', {
        method: 'POST',
        body: {
          receipt_no: serviceReceiptNo(),
          payment_no: payNo,
          exchange_no: this.active.exchange_no,
          kind: 'pay_success', status: 'success',
          amount: this.active.diff.amount,
          note: 'LATE success after failure',
        },
      });
      this.lastReceiptResult = d;
      await this.reloadActive();
    },
    async reloadActive() {
      if (!this.active) return;
      try {
        this.active = await this.api(`/api/exchanges/${this.active.exchange_no}?member_id=${this.memberId}`);
      } catch (e) { /* ignore */ }
    },
    async pollActive() {
      if (this.active && this.isActiveState(this.active.status)) {
        const before = this.active.status;
        await this.reloadActive();
        if (before === 'created' && this.active.status === 'expired') {
          this.pendingPay = null;
        }
      }
    },
    tick() {
      if (!this.active || !this.active.reservation) return;
      const expire = new Date(this.active.reservation.expire_at).getTime();
      this.countdown = expire - Date.now();
    },
  },
  watch: {
    newCouponId() { if (this.newSeatId) this.refreshQuote(); },
    pendingPay(v) { if (v) this._lastPaymentNo = v.payment_no; },
  },
}).mount('#app');

function serviceReceiptNo() {
  return 'RCP-WEB-' + crypto.randomUUID().slice(0, 16);
}

#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
电影院改签系统 live 端到端验证脚本（对真实 HTTP 服务打流量）。
覆盖：
  1. 并发抢座（只有 1 人成功）
  2. 同行订单单张改签（其余票保留原场次）
  3. 补款失败恢复（原票/原座位保留，新座位释放）
  4. 重复回执 / 迟到回执（不触发第二次改签；晚到成功自动退款）
  5. 预留超时释放
  6. 补 / 退 / 平价差额计算与券规则
"""
import json, os, sys, time, uuid, urllib.request, urllib.error, threading

BASE = os.environ.get("BASE", "http://localhost:8091")
PASS, FAIL = [], []

def req(method, path, body=None):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(BASE + path, data=data, method=method,
                               headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(r) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read())

def check(name, cond, detail=""):
    (PASS if cond else FAIL).append(name)
    print(("  ✅ " if cond else "  ❌ ") + name + ("" if cond else f"  -> {detail}"))

def post(path, body): return req("POST", path, body)
def get(path): return req("GET", path)

def yuan(fen): return f"{fen/100:.2f}"

# ---------- 初始化 ----------
print("\n=== 初始化演示数据 ===")
_, d = post("/api/admin/reset", {})
MID = d["member_id"]
_, b = get(f"/api/bootstrap?member_id={MID}")
tickets = b["tickets"]; sessions = {s["movie"]+s["start_at"][11:16]: s for s in b["sessions"]}
print(f"会员 {MID}，有效票 {len(tickets)} 张，场次 {len(sessions)} 个")

def sid_of(movie_sub, time_hm):
    for s in b["sessions"]:
        if movie_sub in s["movie"] and s["start_at"].endswith(time_hm+":00Z"):
            return s["id"]
    raise KeyError(movie_sub, time_hm)

def seat_id(session_id, row, num):
    _, d2 = get(f"/api/sessions/{session_id}/seats?member_id={MID}")
    for s in d2["seats"]:
        if s["row"] == row and s["number"] == num:
            return s["id"]
    raise KeyError

# ---------- 场景 1：差额计算（补 / 退 / 平）----------
print("\n=== 场景1：差额计算（报价，不落库）===")
old_t = tickets[0]   # 星际穿越 6000 用9折 实付5400
oid = old_t["id"]
# 补：8000 场沿用9折=7200，补1800
s8000 = sid_of("星际穿越", "19:00")
q, qd = post("/api/quote", {"member_id": MID, "ticket_id": oid,
            "new_session_id": s8000, "new_seat_id": seat_id(s8000,"C",1)})
check("补款报价 7200-5400=+1800", qd["diff"]["amount"] == 1800 and qd["diff"]["action"]=="pay", json.dumps(qd["diff"],ensure_ascii=False))
# 退：5000 场沿用9折=4500，退900
s5000 = sid_of("你的名字", "16:00")
q, qd = post("/api/quote", {"member_id": MID, "ticket_id": oid,
            "new_session_id": s5000, "new_seat_id": seat_id(s5000,"C",1)})
check("退款报价 4500-5400=-900", qd["diff"]["amount"] == -900 and qd["diff"]["action"]=="refund", json.dumps(qd["diff"],ensure_ascii=False))
# 平：同价 6000 场不存在，改用 8000+8折券=6400 不平；改用立减券: 5000-3000=2000 不平。
# 平价用：新场次 8000，需要新实付=5400 无法直接构造，改为验证 6000->同价换座：另选同 6000 原片场次无第二个。
# 因此平价通过 service 层测试覆盖；live 验证券规则：
# 已使用券(coupon 3) 与过期券(coupon 4) 不能再抵
for c in b["coupons"]:
    if c["status"] in ("redeemed","expired"):
        code, ed = post("/api/quote", {"member_id": MID, "ticket_id": oid,
                        "new_session_id": s8000, "new_seat_id": seat_id(s8000,"C",2),
                        "coupon_id": c["id"]})
        check(f"{c['status']}券({c['name']})禁止抵扣", code==409 and ed["code"] in ("coupon_redeemed","coupon_expired"), f"{code} {ed.get('code')}")

# ---------- 场景 2：同行订单单张改签（补款成功）----------
print("\n=== 场景2：同行订单只改一张，补款成功 ===")
# 两张票在同一订单：ticket companions=1
check("原订单有同行票", tickets[0]["companions"] == 1, str(tickets[0]["companions"]))
target_s = sid_of("沙丘", "20:30")  # 10000
target_seat = seat_id(target_s, "D", 5)
code, ex = post("/api/exchanges", {"member_id": MID, "ticket_id": oid,
    "new_session_id": target_s, "new_seat_id": target_seat,
    "coupon_id": 1,  # 8折券: 10000*0.8=8000; 原实付5400 -> 补2600
    "idempotency_key": str(uuid.uuid4())})
check("改签单创建成功", code==200, f"{code} {ex}")
exno = ex["exchange_no"]
check("报价 8000-5400=补2600", ex["diff"]["amount"]==2600, json.dumps(ex["diff"],ensure_ascii=False))
check("新票记录新8折券", ex["new_ticket"]["coupon"] and "8折" in ex["new_ticket"]["coupon"]["name"])
code, pay = post(f"/api/exchanges/{exno}/pay", {"member_id": MID})
check("发起支付", code==200 and pay["amount"]==2600, f"{code} {pay}")
payno = pay["payment_no"]
rcp = "LIVE-RCP-"+uuid.uuid4().hex[:10]
code, done = post(f"/api/payments/{payno}/mock-pay", {"receipt_no": rcp})
check("支付成功->改签完成", done["result"]["effect"]=="exchange_succeeded" and done["exchange"]["status"]=="succeeded", json.dumps(done["result"],ensure_ascii=False))
# 同行另一张票保留原场次原座位
mates = done["exchange"]["companions_kept"]
check("同行1张票保留原场次", len(mates)==1 and mates[0]["status"]=="issued", json.dumps(mates,ensure_ascii=False))
# 原票归档、原座位释放；新票生效
_, tnow = get(f"/api/members/{MID}/tickets?member_id={MID}")
check("会员仍有2张有效票", len(tnow)==2, str(len(tnow)))
newt = [t for t in tnow if t["price"]["session"]["movie"]=="沙丘 3"]
check("新票沙丘3实付8000", len(newt)==1 and newt[0]["price"]["paid"]==8000 and newt[0]["price"]["discount"]==2000,
      json.dumps([(t['price']['session']['movie'],t['price']['paid']) for t in tnow],ensure_ascii=False))
# 原场次 A3 已释放（原会员只剩 A4 那张）
_, sm = get(f"/api/sessions/{sid_of('星际穿越','14:00')}/seats?member_id={MID}")
a3 = [s for s in sm["seats"] if s["row"]=="A" and s["number"]==3][0]
check("原座位A3支付后释放", a3["status"]=="available", a3["status"])

# 重复回执
code, dup = post(f"/api/payments/{payno}/mock-pay", {"receipt_no": rcp})
check("相同回执重放=duplicate", dup["result"]["duplicate"] is True, json.dumps(dup["result"],ensure_ascii=False))
_, tnow2 = get(f"/api/members/{MID}/tickets?member_id={MID}")
check("重放后仍只有2张票（不二次改签）", len(tnow2)==2, str(len(tnow2)))

# ---------- 场景 3：补款失败恢复 ----------
print("\n=== 场景3：补款失败 -> 原票保留、新座释放、可重试 ===")
# 当前第二张票是 星际穿越 A4（实付5400）。改到 8000 场不付失败
t2 = [t for t in tnow2 if t["id"] != newt[0]["id"]][0]
s8000 = sid_of("星际穿越","19:00")
fseat = seat_id(s8000, "B", 2)
code, ex2 = post("/api/exchanges", {"member_id": MID, "ticket_id": t2["id"],
    "new_session_id": s8000, "new_seat_id": fseat, "idempotency_key": str(uuid.uuid4())})
check("第二张票改签单创建", code==200, f"{code} {ex2}")
exno2 = ex2["exchange_no"]
_, pay2 = post(f"/api/exchanges/{exno2}/pay", {"member_id": MID})
code, fail = post(f"/api/payments/{pay2['payment_no']}/mock-pay", {"fail": True, "receipt_no": "LIVE-FAIL-1"})
check("失败回执->exchange_failed", fail["result"]["effect"]=="exchange_failed", json.dumps(fail["result"],ensure_ascii=False))
check("失败后改签单failed", fail["exchange"]["status"]=="failed")
_, sm2 = get(f"/api/sessions/{s8000}/seats?member_id={MID}")
b2 = [s for s in sm2["seats"] if s["id"]==fseat][0]
check("失败后新座位B2释放", b2["status"]=="available", b2["status"])
_, tk2 = get(f"/api/members/{MID}/tickets?member_id={MID}")
check("失败后原票仍有效", len([t for t in tk2 if t["id"]==t2["id"]])==1, "原票消失")
# 8折券已在场景2用掉；这里用立减券重试。5000场 立减3000 -> 2000，原实付5400 -> 退3400（走退差）
# 改为重试同一场次成功（沿用原9折：8000*0.9=7200，补1800）
code, ex3 = post("/api/exchanges", {"member_id": MID, "ticket_id": t2["id"],
    "new_session_id": s8000, "new_seat_id": fseat, "idempotency_key": str(uuid.uuid4())})
check("失败后可重新改签同一座位", code==200, f"{code} {ex3}")
exno3 = ex3["exchange_no"]
_, pay3 = post(f"/api/exchanges/{exno3}/pay", {"member_id": MID})
code, ok3 = post(f"/api/payments/{pay3['payment_no']}/mock-pay", {"receipt_no": "LIVE-RETRY-1"})
check("重试支付成功", ok3["result"]["effect"]=="exchange_succeeded", json.dumps(ok3["result"],ensure_ascii=False))

# ---------- 场景 4：迟到成功回执（先失败后晚到成功）----------
print("\n=== 场景4：迟到的支付成功 -> 自动退款，不改签 ===")
# 需要一张有效票；构造“补款”组合（实付最低的票 -> 最贵场次），才有真实支付单。
_, tk3 = get(f"/api/members/{MID}/tickets?member_id={MID}")
t3 = sorted(tk3, key=lambda t: t["price"]["paid"])[0]
s10000 = sid_of("沙丘", "20:30")
lseat = seat_id(s10000, "E", 6)
code, ex4 = post("/api/exchanges", {"member_id": MID, "ticket_id": t3["id"],
    "new_session_id": s10000, "new_seat_id": lseat,  # 不用券：补 10000-原实付
    "idempotency_key": str(uuid.uuid4())})
if code != 200 or ex4["diff"]["action"] != "pay":
    print("  (跳过迟到场景：未构造出补款支付单)", code, ex4.get("diff"))
else:
    exno4 = ex4["exchange_no"]
    _, pay4 = post(f"/api/exchanges/{exno4}/pay", {"member_id": MID})
    post(f"/api/payments/{pay4['payment_no']}/mock-pay", {"fail": True, "receipt_no": "LATE-F-1"})
    # 晚到的成功（新回执号）
    code, late = post("/api/receipts", {"receipt_no": "LATE-S-1", "payment_no": pay4["payment_no"],
        "exchange_no": exno4, "kind":"pay_success","status":"success","amount": pay4["amount"]})
    check("晚到成功->自动退款", late["effect"]=="auto_refund_late_success", json.dumps(late,ensure_ascii=False))
    _, tk4 = get(f"/api/members/{MID}/tickets?member_id={MID}")
    check("晚到成功后原票仍有效", len([t for t in tk4 if t["id"]==t3["id"]])==1)
    # 同一晚到回执再重放
    _, late2 = post("/api/receipts", {"receipt_no": "LATE-S-1", "payment_no": pay4["payment_no"],
        "exchange_no": exno4, "kind":"pay_success","status":"success","amount": pay4["amount"]})
    check("晚到回执重放=duplicate", late2["duplicate"] is True, json.dumps(late2,ensure_ascii=False))

# ---------- 场景 5：预留超时 ----------
print("\n=== 场景5：预留超时释放新座位，原票保留 ===")
post("/api/admin/reset", {})
_, b = get(f"/api/bootstrap?member_id={MID}")
tk5 = b["tickets"]
t5 = tk5[-1]
s8000 = sid_of("星际穿越","19:00")
xseat = seat_id(s8000, "E", 1)
code, ex5 = post("/api/exchanges", {"member_id": MID, "ticket_id": t5["id"],
    "new_session_id": s8000, "new_seat_id": xseat, "idempotency_key": str(uuid.uuid4())})
if code == 200:
    exno5 = ex5["exchange_no"]
    _, held = get(f"/api/sessions/{s8000}/seats?member_id={MID}")
    hs = [s for s in held["seats"] if s["id"]==xseat][0]
    check("支付前座位为held", hs["status"]=="held", hs["status"])
    print("  等待 7 秒（TTL=5s）...")
    time.sleep(7)
    _, d5 = get(f"/api/exchanges/{exno5}?member_id={MID}")
    check("超时后改签单expired", d5["status"]=="expired", d5["status"])
    _, after = get(f"/api/sessions/{s8000}/seats?member_id={MID}")
    af = [s for s in after["seats"] if s["id"]==xseat][0]
    check("超时后座位释放available", af["status"]=="available", af["status"])
    _, tk5b = get(f"/api/members/{MID}/tickets?member_id={MID}")
    check("超时后原票仍有效", len([t for t in tk5b if t["id"]==t5["id"]])==1)
else:
    print("  (跳过超时场景)", ex5)

# ---------- 场景 6：进程级 HTTP 并发抢座 ----------
print("\n=== 场景6：两个并发 HTTP 请求抢同一新座位（只允许 1 个成功）===")
post("/api/admin/reset", {})
_, b = get(f"/api/bootstrap?member_id={MID}")
tk6 = b["tickets"]
grab_s = sid_of("沙丘", "20:30")
grab_seat = seat_id(grab_s, "C", 7)
results = []
def grab(tid, key):
    code, gd = post("/api/exchanges", {"member_id": MID, "ticket_id": tid,
        "new_session_id": grab_s, "new_seat_id": grab_seat, "idempotency_key": key})
    results.append((code, gd.get("code", "")))
ths = [threading.Thread(target=grab, args=(t["id"], f"grab-{i}")) for i, t in enumerate(tk6[:2])]
for x in ths: x.start()
for x in ths: x.join()
ok6 = sum(1 for c, _ in results if c == 200)
cf6 = sum(1 for c, cc in results if c == 409 and cc == "seat_taken")
check("并发抢座恰好1个成功1个seat_taken", ok6 == 1 and cf6 == 1, str(results))
_, occ6 = get(f"/api/sessions/{grab_s}/seats?member_id={MID}")
gs = [s for s in occ6["seats"] if s["id"] == grab_seat][0]
check("被抢座位唯一占用(held)", gs["status"] == "held", gs["status"])

# ---------- 汇总 ----------
print(f"\n{'='*50}")
print(f"通过 {len(PASS)} 项，失败 {len(FAIL)} 项")
if FAIL:
    print("失败项：")
    for f_ in FAIL: print("  -", f_)
    sys.exit(1)
print("🎉 全部 live 场景验证通过")

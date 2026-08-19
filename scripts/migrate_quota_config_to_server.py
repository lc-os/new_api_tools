#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
额度管理配置迁移脚本：本地 SQLite 配置 → 服务器 new_api_tools API

用法:
  1. 先改下方 SERVER 和 PASSWORD
  2. python3 scripts/migrate_quota_config_to_server.py --preview   # 预览将迁移的配置
  3. python3 scripts/migrate_quota_config_to_server.py --apply     # 实际写入服务器

说明:
  - 从本地 backend/data/quota-refresh.db 读取 额度管理 配置
  - 通过服务器 3001 端口的 API 写入（需要服务器 ADMIN_PASSWORD）
  - 幂等：重复执行无副作用
"""
import argparse
import json
import sqlite3
import sys
import urllib.request
from pathlib import Path

# ====== 修改这两个值 ======
SERVER = "http://10.126.10.166:3001"   # 服务器 new_api_tools 地址
PASSWORD = ""                          # 服务器 ADMIN_PASSWORD
# ==========================

LOCAL_DB = Path(__file__).parent.parent / "backend" / "data" / "quota-refresh.db"


def read_local_config(db_path):
    """读取本地 SQLite 里的额度管理配置"""
    conn = sqlite3.connect(db_path)
    conn.row_factory = sqlite3.Row
    out = {}

    s = conn.execute(
        "SELECT enabled, mode, quota_amount, shared_budget, refresh_time "
        "FROM quota_refresh_settings WHERE id=1"
    ).fetchone()
    if s:
        out["settings"] = dict(s)

    caps = conn.execute(
        "SELECT token_id, cap_yuan, unlocked FROM quota_refresh_token_caps "
        "ORDER BY token_id"
    ).fetchall()
    out["caps"] = [dict(c) for c in caps]

    assignments = conn.execute(
        "SELECT token_id, quota, share FROM quota_refresh_assignments "
        "ORDER BY token_id"
    ).fetchall()
    out["assignments"] = [dict(a) for a in assignments]
    conn.close()
    return out


def api_call(url, token, method="GET", body=None):
    req = urllib.request.Request(url, method=method)
    req.add_header("Authorization", f"Bearer {token}")
    if body is not None:
        req.add_header("Content-Type", "application/json")
        req.data = json.dumps(body).encode("utf-8")
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            return json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        return {"success": False, "error": e.read().decode("utf-8")[:300], "http": e.code}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--preview", action="store_true", help="只预览配置，不写入")
    ap.add_argument("--apply", action="store_true", help="写入服务器")
    args = ap.parse_args()

    cfg = read_local_config(LOCAL_DB)
    print("===== 本地配置预览 =====")
    print(f"  DB 文件: {LOCAL_DB}")
    if "settings" in cfg:
        s = cfg["settings"]
        print(f"  settings: enabled={s['enabled']} mode={s['mode']} "
              f"quota_amount={s['quota_amount']} shared_budget={s['shared_budget']} "
              f"refresh_time={s['refresh_time']}")
    print(f"  caps: {len(cfg.get('caps', []))} 个令牌")
    if cfg.get("caps"):
        first = cfg["caps"][0]
        print(f"    示例: token_id={first['token_id']} cap_yuan={first['cap_yuan']} unlocked={first['unlocked']}")
    print(f"  assignments: {len(cfg.get('assignments', []))} 条")

    if args.preview or not args.apply:
        print("\n预览模式，未写入。加 --apply 实际写入。")
        return

    if not PASSWORD:
        print("请先设置脚本顶部 PASSWORD = 服务器 ADMIN_PASSWORD")
        sys.exit(1)

    # 登录
    login = api_call(f"{SERVER}/api/auth/login", "", "POST", {"password": PASSWORD})
    token = login.get("token")
    if not token:
        print(f"登录失败: {json.dumps(login, ensure_ascii=False)[:200]}")
        sys.exit(1)
    print(f"\n登录成功")

    # 1. 写 settings
    if "settings" in cfg:
        s = cfg["settings"]
        body = {
            "enabled": bool(s["enabled"]),
            "mode": s["mode"],
            "shared_budget": float(s["shared_budget"]),
            "refresh_time": s["refresh_time"],
            # quota_amount 在共享模式不直接用，也带上
            "quota_amount": s["quota_amount"],
        }
        r = api_call(f"{SERVER}/api/quota-refresh/config", token, "PUT", body)
        print(f"  PUT /api/quota-refresh/config → {json.dumps(r, ensure_ascii=False)[:200]}")

    # 2. 写 caps（unlocked 从 SQLite INTEGER 转 bool）
    if cfg.get("caps"):
        items = [
            {"token_id": c["token_id"], "cap_yuan": float(c["cap_yuan"]),
             "unlocked": bool(c["unlocked"])}
            for c in cfg["caps"]
        ]
        r = api_call(f"{SERVER}/api/quota-refresh/caps", token, "PUT",
                     {"items": items})
        print(f"  PUT /api/quota-refresh/caps ({len(items)} 个) → {json.dumps(r, ensure_ascii=False)[:200]}")

    # 3. 写 assignments
    if cfg.get("assignments"):
        r = api_call(f"{SERVER}/api/quota-refresh/assignments", token, "PUT",
                     {"items": cfg["assignments"]})
        print(f"  PUT /api/quota-refresh/assignments ({len(cfg['assignments'])} 条) → {json.dumps(r, ensure_ascii=False)[:200]}")

    # 验证
    r = api_call(f"{SERVER}/api/quota-refresh/config", token)
    print(f"\n===== 服务器配置验证 =====")
    print(f"  {json.dumps(r.get('data', r), ensure_ascii=False)[:400]}")


if __name__ == "__main__":
    main()

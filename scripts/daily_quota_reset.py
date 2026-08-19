#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
每日额度重置脚本：按近 30 天各 token 用量占比，分配全站每日预算。

设计：
- 全站日预算（默认 ¥200）按各 token 近 30 天 token 消耗占比分配
- 每个 token 的 remain_quota 每日 0 点重置为日额度，unlimited_quota=false
- 额度用尽后 NewAPI 自动拒绝请求（无需额外处理），次日 0 点重置恢复
- 不动 status 字段（避免覆盖管理员手动禁用）

用法：
  python3 daily_quota_reset.py [--budget 200] [--dsn "host=... port=5432 user=... password=... dbname=..."]
  建议 cron: 0 0 * * * python3 /path/to/daily_quota_reset.py >> /var/log/daily_quota_reset.log 2>&1
"""
import argparse
import datetime
import sys

import pg8000.native

# 保底额度：近 30 天无消耗的 token 也给一点（quota，50 万 = ¥1）
MIN_DAILY_QUOTA = 100_000
# 500,000 quota = ¥1
QUOTA_PER_YUAN = 500_000


def parse_args():
    p = argparse.ArgumentParser(description="每日 token 额度重置")
    p.add_argument("--budget", type=float, default=200.0,
                   help="全站每日预算（元），默认 200")
    p.add_argument("--dsn", default="",
                   help="PostgreSQL DSN，缺省读环境变量 DAILY_QUOTA_DSN")
    p.add_argument("--dry-run", action="store_true", help="只打印不执行")
    return p.parse_args()


def connect(dsn: str):
    env = __import__("os").environ
    if not dsn:
        dsn = env.get("DAILY_QUOTA_DSN", "")
    if dsn:
        parts = dict(kv.split("=", 1) for kv in dsn.split() if "=" in kv)
    else:
        parts = {
            "host": env.get("DB_HOST", "127.0.0.1"),
            "port": env.get("DB_PORT", "5432"),
            "user": env.get("DB_USER", "postgres"),
            "password": env.get("DB_PASSWORD", ""),
            "dbname": env.get("DB_NAME", "new-api"),
        }
    return pg8000.native.Connection(
        user=parts.get("user"), password=parts.get("password", ""),
        host=parts.get("host"), port=int(parts.get("port", 5432)),
        database=parts.get("dbname", "new-api"))


def main():
    args = parse_args()
    budget_quota = int(args.budget * QUOTA_PER_YUAN)  # 日预算 → quota
    days30 = int(datetime.datetime.now().timestamp()) - 30 * 86400

    con = connect(args.dsn)
    try:
        # 近 30 天各 token 的 token 消耗量
        rows = con.run(f"""
            SELECT token_name, sum(prompt_tokens) + sum(completion_tokens)
            FROM logs
            WHERE type = 2 AND created_at >= {days30}
            GROUP BY token_name""")
        usage = {r[0]: float(r[1] or 0) for r in rows}
        total = sum(usage.values()) or 1.0

        # 计算每个 token 的日额度
        quotas = {}
        for name, tok in usage.items():
            share = tok / total
            quotas[name] = max(int(budget_quota * share), MIN_DAILY_QUOTA)

        # 近 30 天无消耗的启用中 token 也设保底额度，防止漏网
        rows = con.run("SELECT name FROM tokens WHERE status = 1")
        for r in rows:
            quotas.setdefault(r[0], MIN_DAILY_QUOTA)

        print(f"[{datetime.datetime.now():%Y-%m-%d %H:%M:%S}] 全站日预算 ¥{args.budget:.0f} "
              f"({budget_quota:,} quota)，按 30 天用量占比分配，共 {len(quotas)} 个 token")
        for name, q in sorted(quotas.items(), key=lambda kv: -kv[1]):
            print(f"  {name:<16} {q:>14,} quota ≈ ¥{q / QUOTA_PER_YUAN:.2f}/天")

        if args.dry_run:
            return

        # 重置所有有消耗的 token（不动 status，避免覆盖手动禁用）
        for name, q in quotas.items():
            con.run("""
                UPDATE tokens SET remain_quota = $1, unlimited_quota = false
                WHERE name = $2""", q=q, name=name)
        print(f"已重置 {len(quotas)} 个 token 的每日额度")
    finally:
        con.close()


if __name__ == "__main__":
    try:
        main()
    except Exception as e:
        print(f"ERROR: {e}", file=sys.stderr)
        sys.exit(1)

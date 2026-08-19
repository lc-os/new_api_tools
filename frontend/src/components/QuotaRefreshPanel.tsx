import { useState, useEffect, useCallback } from 'react'
import { useAuth } from '../contexts/AuthContext'
import { useToast } from './Toast'
import { Timer, Loader2, Save, Play, History, Zap, Users, Wallet, Trash2, Wand2, TrendingUp, TrendingDown, RefreshCw, Lock as LockIcon, LockOpen as LockOpenIcon } from 'lucide-react'
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from './ui/card'
import { Button } from './ui/button'
import { Badge } from './ui/badge'
import { Input } from './ui/input'
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription, DialogFooter } from './ui/dialog'

// 与全站一致：500,000 quota = 1 元
const QUOTA_PER_YUAN = 500000

interface QuotaRefreshConfig {
  enabled: boolean
  quota_amount: number
  refresh_time: string
  mode: string // per-token | shared
  shared_budget: number
  last_run_at: number
  last_run_info: string
  updated_at: number
  wallet_balance: number // 共享池当前剩余额度（启用令牌用户钱包合计）
}

interface TokenCap {
  token_id: number
  token_name: string
  cap_yuan: number // 每人每日上限（元），默认 20；0 = 当天不分配额度
  unlocked: boolean // 配置：已解除限制（走共享池）
  remain_quota: number // 令牌当前剩余额度（unlimited 时无意义）
  unlimited: boolean // 线上实际状态：无限额（走钱包/共享池）
  used_quota: number // 当天 0 点起该令牌已用额度（logs type=2）
  cache_hit_rate: number // 当天缓存命中率 %（-1 = 无用量数据）
}

interface QuotaRefreshRun {
  id: number
  run_at: number
  triggered: number
  quota_amount: number
  status: string
  detail: string
}

interface AssignmentItem {
  token_id: number
  token_name: string
  quota: number
  share: number
}

function formatTime(ts: number) {
  if (!ts) return '-'
  return new Date(ts * 1000).toLocaleString('zh-CN', {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  })
}

const MODE_LABELS: Record<string, { label: string; desc: string }> = {
  'per-token': { label: '每令牌额度', desc: '统一设置每个令牌额度，或使用共享总额度生成的分配表，每个令牌用完即停' },
  shared: { label: '共享总额度', desc: '所有令牌共享一个额度池，用尽全停，每天重置' },
}

export function QuotaRefreshPanel() {
  const { token } = useAuth()
  const { showToast } = useToast()
  const apiUrl = import.meta.env.VITE_API_URL || ''

  const getAuthHeaders = useCallback(() => ({
    'Content-Type': 'application/json',
    'Authorization': `Bearer ${token}`,
  }), [token])

  const [config, setConfig] = useState<QuotaRefreshConfig | null>(null)
  const [runs, setRuns] = useState<QuotaRefreshRun[]>([])
  const [enabled, setEnabled] = useState(false)
  const [mode, setMode] = useState('per-token')
  const [moneyInput, setMoneyInput] = useState('') // per-token: 每令牌额度；shared: 共享总额度
  const [refreshTime, setRefreshTime] = useState('00:00')
  const [assignments, setAssignments] = useState<AssignmentItem[]>([])
  // 分配表行内编辑草稿：保留原始输入字符串，避免 toFixed 回显导致无法输入多位/小数
  const [drafts, setDrafts] = useState<Record<number, string>>({})
  const [budgetInput, setBudgetInput] = useState('') // 按用量占比生成分配表用的总预算
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [running, setRunning] = useState(false)
  const [genLoading, setGenLoading] = useState(false)
  const [walletBalance, setWalletBalance] = useState(0) // 共享池当前剩余额度
  const [walletRefreshing, setWalletRefreshing] = useState(false)
  const [caps, setCaps] = useState<TokenCap[]>([]) // 每人上限配置
  const [capsSaving, setCapsSaving] = useState(false)
  const [capsRefreshing, setCapsRefreshing] = useState(false)
  const [bulkCapInput, setBulkCapInput] = useState('') // 快捷批量设置金额（元）
  const [usageDialogOpen, setUsageDialogOpen] = useState(false) // 共享池余额点击弹框：今日使用明细
  const [savedCapsSnapshot, setSavedCapsSnapshot] = useState('') // 已保存的上限配置快照（判断未保存修改）

  // 仅拉取共享池余额（不覆盖 config/分配表等编辑中状态）
  const fetchWallet = useCallback(async () => {
    try {
      const res = await fetch(`${apiUrl}/api/quota-refresh/config`, { headers: getAuthHeaders() })
      const j = await res.json()
      if (j.success) setWalletBalance(j.data.wallet_balance ?? 0)
    } catch (e) { /* 网络抖动时保持旧值 */ }
  }, [apiUrl, getAuthHeaders])

  // 共享池余额每 1 分钟自动刷新（与时段定价检测频率一致）
  useEffect(() => {
    fetchWallet()
    const t = setInterval(fetchWallet, 60_000)
    return () => clearInterval(t)
  }, [fetchWallet])

  // 每人上限配置
  const fetchCaps = useCallback(async () => {
    try {
      const res = await fetch(`${apiUrl}/api/quota-refresh/caps`, { headers: getAuthHeaders() })
      const j = await res.json()
      if (j.success) {
        setCaps(j.data.items || [])
        // 服务端是权威：每次拉取后重置快照，未保存修改从此时起算
        setSavedCapsSnapshot(JSON.stringify((j.data.items || []).map((c: TokenCap) => [c.token_id, c.cap_yuan, c.unlocked])))
      }
    } catch (e) { /* 网络抖动时保持旧值 */ }
  }, [apiUrl, getAuthHeaders])

  useEffect(() => { fetchCaps() }, [fetchCaps])

  const updateCap = (tokenId: number, patch: Partial<TokenCap>) => {
    setCaps(prev => prev.map(c => c.token_id === tokenId ? { ...c, ...patch } : c))
  }

  const saveCaps = async (items?: { token_id: number; cap_yuan: number; unlocked: boolean }[]) => {
    setCapsSaving(true)
    try {
      const res = await fetch(`${apiUrl}/api/quota-refresh/caps`, {
        method: 'PUT',
        headers: getAuthHeaders(),
        body: JSON.stringify({ items: items ?? caps.map(c => ({ token_id: c.token_id, cap_yuan: c.cap_yuan, unlocked: c.unlocked })) }),
      })
      const j = await res.json()
      if (j.success) {
        showToast('success', j.data?.message || '上限配置已保存并立即生效')
        await fetchCaps()
      } else {
        showToast('error', j.error?.message || '保存上限配置失败')
      }
    } catch (e) {
      showToast('error', '保存上限配置失败')
    } finally {
      setCapsSaving(false)
    }
  }

  // 有未保存的上限修改（本地编辑 vs 已保存快照）
  const capsSnapshot = JSON.stringify(caps.map(c => [c.token_id, c.cap_yuan, c.unlocked]))
  const capsDirty = caps.length > 0 && capsSnapshot !== savedCapsSnapshot

  // 从已保存快照取某令牌的上限（用于判断是否真的有修改，防止失焦残留值误提交）
  const savedCapOf = (tokenId: number): number | null => {
    try {
      const arr = JSON.parse(savedCapsSnapshot || '[]') as [number, number, boolean][]
      const hit = arr.find(a => a[0] === tokenId)
      return hit ? hit[1] : null
    } catch { return null }
  }

  // 上限输入框失焦：仅当值确实变化（与已保存值不同）才提交，否则忽略
  const handleCapBlur = (c: TokenCap, raw: string) => {
    const v = parseFloat(raw)
    const newCap = isNaN(v) ? 0 : v
    const savedCap = savedCapOf(c.token_id)
    if (savedCap !== null && Math.abs(newCap - savedCap) < 0.001) return
    commitCap({ ...c, cap_yuan: newCap })
  }

  // 针对单人调整：失焦时立即提交该令牌的上限（后端同步剩余额度 remain = 上限 − 当天用量），
  // 并更新本地剩余显示与已保存快照（不整体刷新，避免覆盖其他令牌的未保存修改）。
  const commitCap = async (c: TokenCap) => {
    if (c.cap_yuan < 0) return
    setCapsSaving(true)
    try {
      const res = await fetch(`${apiUrl}/api/quota-refresh/caps`, {
        method: 'PUT',
        headers: getAuthHeaders(),
        body: JSON.stringify({ items: [{ token_id: c.token_id, cap_yuan: c.cap_yuan, unlocked: c.unlocked }] }),
      })
      const j = await res.json()
      if (j.success) {
        // 本地同步该令牌剩余额度（上限 − 当天已用）与已保存快照
        const remainQuota = Math.max(Math.round(c.cap_yuan * QUOTA_PER_YUAN) - (c.used_quota || 0), 0)
        setCaps(prev => prev.map(x => x.token_id === c.token_id ? { ...x, remain_quota: remainQuota } : x))
        setSavedCapsSnapshot(prev => {
          const arr = JSON.parse(prev || '[]') as [number, number, boolean][]
          const idx = arr.findIndex(a => a[0] === c.token_id)
          if (idx >= 0) arr[idx] = [c.token_id, c.cap_yuan, c.unlocked]
          else arr.push([c.token_id, c.cap_yuan, c.unlocked])
          return JSON.stringify(arr)
        })
        showToast('success', `已更新 ${c.token_name} 的上限，剩余额度同步为 ¥${(remainQuota / QUOTA_PER_YUAN).toFixed(2)}`)
      } else {
        showToast('error', j.error?.message || '更新该令牌上限失败')
      }
    } catch (e) {
      showToast('error', '更新该令牌上限失败')
    } finally {
      setCapsSaving(false)
    }
  }

  // 剩余量分档着色：0 红 / <20% 橙 / <50% 黄 / 其余绿（按剩余占上限比例）
  const remainClass = (c: TokenCap): string => {
    const remain = Math.max(c.cap_yuan - (c.used_quota || 0) / QUOTA_PER_YUAN, 0)
    const rate = c.cap_yuan > 0 ? remain / c.cap_yuan : 0
    if (rate <= 0) return 'text-red-600 font-semibold'
    if (rate < 0.2) return 'text-orange-500 font-medium'
    if (rate < 0.5) return 'text-amber-500'
    return 'text-green-600'
  }

  // 快捷批量：所有令牌统一设为同一金额（立即生效）
  const applyBulkCap = async () => {
    const yuan = parseFloat(bulkCapInput)
    if (isNaN(yuan) || yuan < 0) {
      showToast('error', '请输入有效的金额（元），可为 0（当天无额度）')
      return
    }
    const items = caps.map(c => ({ token_id: c.token_id, cap_yuan: yuan, unlocked: c.unlocked }))
    setCaps(caps.map(c => ({ ...c, cap_yuan: yuan }))) // 本地先行显示
    await saveCaps(items)
  }

  const handleRefreshWallet = async () => {
    setWalletRefreshing(true)
    await fetchWallet()
    setWalletRefreshing(false)
  }

  // 刷新每人上限列表（含各令牌当前剩余量）
  const handleRefreshCaps = async () => {
    setCapsRefreshing(true)
    await fetchCaps()
    setCapsRefreshing(false)
  }

  const fetchAll = useCallback(async () => {
    try {
      const [cfgRes, runsRes, asgRes] = await Promise.all([
        fetch(`${apiUrl}/api/quota-refresh/config`, { headers: getAuthHeaders() }),
        fetch(`${apiUrl}/api/quota-refresh/runs?limit=5`, { headers: getAuthHeaders() }),
        fetch(`${apiUrl}/api/quota-refresh/assignments`, { headers: getAuthHeaders() }),
      ])
      const cfgData = await cfgRes.json()
      const runsData = await runsRes.json()
      const asgData = await asgRes.json()
      if (cfgData.success) {
        setConfig(cfgData.data)
        setWalletBalance(cfgData.data.wallet_balance ?? 0)
        setEnabled(cfgData.data.enabled)
        // 旧配置 uniform 并入 per-token
        const cfgMode = cfgData.data.mode === 'uniform' ? 'per-token' : (cfgData.data.mode || 'per-token')
        setMode(cfgMode)
        if (cfgMode === 'shared') {
          setMoneyInput(cfgData.data.shared_budget > 0 ? String(cfgData.data.shared_budget) : '200')
        } else {
          setMoneyInput(cfgData.data.quota_amount > 0 ? (cfgData.data.quota_amount / QUOTA_PER_YUAN).toFixed(2) : '')
        }
        setRefreshTime(cfgData.data.refresh_time || '00:00')
      }
      if (runsData.success) setRuns(runsData.data.items)
      if (asgData.success) {
        setAssignments(asgData.data.items || [])
        setDrafts({}) // 新数据加载后丢弃编辑草稿
      }
    } catch (e) {
      showToast('error', '加载定时刷新配置失败')
    } finally {
      setLoading(false)
    }
  }, [apiUrl, getAuthHeaders, showToast])

  useEffect(() => { fetchAll() }, [fetchAll])

  const saveConfig = async () => {
    const money = parseFloat(moneyInput)
    if (mode === 'shared') {
      // 共享总额度：必须设置共享池金额
      if (!money || money <= 0) {
        showToast('error', '请输入共享总额度（元）')
        return
      }
    } else {
      // 每令牌额度：统一额度 与 分配表 二选一
      if ((!money || money <= 0) && assignments.length === 0) {
        showToast('error', '请填写统一额度，或先生成每令牌分配表')
        return
      }
    }
    setSaving(true)
    try {
      // 1) 每令牌额度：分配表随配置一起保存（无需单独点保存分配表）
      if (mode === 'per-token') {
        const items = assignments.filter(a => a.quota > 0)
        if (items.length > 0) {
          const asgRes = await fetch(`${apiUrl}/api/quota-refresh/assignments`, {
            method: 'PUT',
            headers: getAuthHeaders(),
            body: JSON.stringify({ items: items.map(a => ({ token_id: a.token_id, quota: a.quota })) }),
          })
          const asgData = await asgRes.json()
          if (!asgData.success) throw new Error(asgData?.error?.message || '分配表保存失败')
        }
      }
      // 2) 保存配置
      const res = await fetch(`${apiUrl}/api/quota-refresh/config`, {
        method: 'PUT',
        headers: getAuthHeaders(),
        body: JSON.stringify({
          enabled,
          quota_amount: money > 0 ? Math.round(money * QUOTA_PER_YUAN) : 0,
          refresh_time: refreshTime,
          mode,
          shared_budget: mode === 'shared' ? money : 0,
        }),
      })
      const data = await res.json()
      if (!data.success) throw new Error(data?.error?.message || '保存失败')
      showToast('success', '配置已保存')
      setConfig(data.data)
      fetchAll()
    } catch (e) {
      showToast('error', e instanceof Error ? e.message : '保存失败')
    } finally {
      setSaving(false)
    }
  }

  const runNow = async () => {
    setRunning(true)
    try {
      const res = await fetch(`${apiUrl}/api/quota-refresh/run`, {
        method: 'POST',
        headers: getAuthHeaders(),
      })
      const data = await res.json()
      if (!data.success) throw new Error(data?.error?.message || '执行失败')
      showToast('success', `执行成功：${data.data.detail}`)
      fetchAll()
    } catch (e) {
      showToast('error', e instanceof Error ? e.message : '执行失败')
    } finally {
      setRunning(false)
    }
  }

  // 两档推荐对话框状态
  const [recDialog, setRecDialog] = useState(false)
  const [recData, setRecData] = useState<{
    basis: string
    budget: number
    big: { yuan: number; quota: number; count: number; token_ids: number[]; tokens: string[] }
    small: { yuan: number; quota: number; count: number; token_ids: number[]; tokens: string[] }
    idle: { yuan: number; quota: number; count: number; token_ids: number[]; tokens: string[] }
  } | null>(null)
  const [recBig, setRecBig] = useState('')
  const [recSmall, setRecSmall] = useState('')
  const [recIdle, setRecIdle] = useState('0.2')

  const generateAssignments = async () => {
    const budget = parseFloat(budgetInput)
    if (!budget || budget <= 0) { showToast('error', '请输入生成总额度（元）'); return }
    setGenLoading(true)
    try {
      // 按用量占比：推荐两档额度（大/小），弹窗确认后填充
      const res = await fetch(`${apiUrl}/api/quota-refresh/assignments/recommend`, {
        method: 'POST',
        headers: getAuthHeaders(),
        body: JSON.stringify({ budget, days: 7 }),
      })
      const data = await res.json()
      if (!data.success) throw new Error(data?.error?.message || '生成失败')
      setRecData(data.data)
      setRecBig(String(data.data.big.yuan))
      setRecSmall(String(data.data.small.yuan))
      setRecIdle(String(data.data.idle?.yuan ?? 0.2))
      setRecDialog(true)
    } catch (e) {
      showToast('error', e instanceof Error ? e.message : '生成失败')
    } finally {
      setGenLoading(false)
    }
  }

  // 确认两档推荐：高用量令牌填大额度，低用量令牌填小额度
  const confirmRecommend = async () => {
    if (!recData) return
    const big = parseFloat(recBig)
    const small = parseFloat(recSmall)
    const idle = parseFloat(recIdle)
    if (!big || big <= 0) { showToast('error', '大额度无效'); return }
    if (!small || small <= 0) { showToast('error', '小额度无效'); return }
    if (recData.idle?.count > 0 && (!idle || idle <= 0)) { showToast('error', '零用量额度无效'); return }
    const items = [
      ...recData.big.token_ids.map(id => ({ token_id: id, quota: Math.round(big * QUOTA_PER_YUAN) })),
      ...recData.small.token_ids.map(id => ({ token_id: id, quota: Math.round(small * QUOTA_PER_YUAN) })),
      ...(recData.idle?.token_ids || []).map(id => ({ token_id: id, quota: Math.round(idle * QUOTA_PER_YUAN) })),
    ]
    setSaving(true)
    try {
      const res = await fetch(`${apiUrl}/api/quota-refresh/assignments`, {
        method: 'PUT',
        headers: getAuthHeaders(),
        body: JSON.stringify({ items }),
      })
      const data = await res.json()
      if (!data.success) throw new Error(data?.error?.message || '保存失败')
      showToast('success', `已按两档填充：高用量 ${recData.big.count} 个 ¥${big.toFixed(2)} + 低用量 ${recData.small.count} 个 ¥${small.toFixed(2)}` + (recData.idle?.count ? ` + 零用量 ${recData.idle.count} 个 ¥${idle.toFixed(2)}` : ''))
      setRecDialog(false)
      setDrafts({}) // 新分配表加载前丢弃草稿
      // 填充后每日预算 = 两档合计
      const filledTotal = items.reduce((s, it) => s + it.quota, 0)
      setBudgetInput((filledTotal / QUOTA_PER_YUAN).toFixed(2))
      fetchAll()
    } catch (e) {
      showToast('error', e instanceof Error ? e.message : '保存失败')
    } finally {
      setSaving(false)
    }
  }

  const updateAssignmentQuota = (tokenId: number, yuan: string) => {
    // 保留原始输入（不立即 toFixed 回显，否则“4”变“4.00”后无法继续输入成“40”）
    setDrafts(prev => ({ ...prev, [tokenId]: yuan }))
    const v = parseFloat(yuan)
    const quota = v > 0 ? Math.round(v * QUOTA_PER_YUAN) : 0
    const next = assignments.map(a =>
      a.token_id === tokenId ? { ...a, quota } : a
    )
    setAssignments(next)
    // 编辑单个令牌每日额度后，每日预算自动更新为分配表合计（随编辑实时变化）
    const total = next.reduce((s, a) => s + a.quota, 0)
    setBudgetInput((total / QUOTA_PER_YUAN).toFixed(2))
  }

  const clearAssignments = async () => {
    setSaving(true)
    try {
      const res = await fetch(`${apiUrl}/api/quota-refresh/assignments`, {
        method: 'DELETE',
        headers: getAuthHeaders(),
      })
      const data = await res.json()
      if (!data.success) throw new Error(data?.error?.message || '清空失败')
      showToast('success', '分配表已清空')
      setAssignments([])
      setDrafts({})
    } catch (e) {
      showToast('error', e instanceof Error ? e.message : '清空失败')
    } finally {
      setSaving(false)
    }
  }

  const totalAssigned = assignments.reduce((s, a) => s + a.quota, 0)

  // 分配表表格（per-token 与 shared-方式二 复用）
  const assignmentTableJsx = (
    <div className="max-h-72 overflow-y-auto rounded-lg border">
      <table className="w-full text-xs">
        <thead className="sticky top-0 bg-muted/80 backdrop-blur">
          <tr className="text-left text-muted-foreground">
            <th className="px-3 py-2 font-medium">令牌</th>
            <th className="px-3 py-2 font-medium">每日额度（元）</th>
            <th className="px-3 py-2 font-medium text-right">额度（quota）</th>
          </tr>
        </thead>
        <tbody className="divide-y">
          {assignments.map((a) => (
            <tr key={a.token_id} className="hover:bg-muted/30">
              <td className="px-3 py-1.5 font-medium">{a.token_name || `#${a.token_id}`}</td>
              <td className="px-3 py-1.5">
                <Input
                  type="number"
                  min="0"
                  step="0.01"
                  className="h-7 w-28"
                  value={drafts[a.token_id] !== undefined
                    ? drafts[a.token_id]
                    : (a.quota > 0 ? (a.quota / QUOTA_PER_YUAN).toFixed(2) : '')}
                  onChange={(e) => updateAssignmentQuota(a.token_id, e.target.value)}
                />
              </td>
              <td className="px-3 py-1.5 text-right font-mono text-muted-foreground">{a.quota.toLocaleString('zh-CN')}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )

  // 下次执行时间：今天的 HH:mm，已过则明天
  const nextRunLabel = (() => {
    if (!config || !config.enabled) return null
    const [h, m] = (config.refresh_time || '00:00').split(':').map(Number)
    const now = new Date()
    const next = new Date(now.getFullYear(), now.getMonth(), now.getDate(), h, m, 0)
    if (next.getTime() <= now.getTime()) next.setDate(next.getDate() + 1)
    return `下次执行 ${next.toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })}`
  })()

  const money = parseFloat(moneyInput)
  const previewQuota = money > 0 ? Math.round(money * QUOTA_PER_YUAN) : 0

  // 是否有未保存的修改：开启状态/额度/刷新时间 与已保存配置不一致时提醒
  const hasUnsaved = (() => {
    if (!config) return false
    if (enabled !== config.enabled) return true
    if (refreshTime !== (config.refresh_time || '00:00')) return true
    const cfgMoney = config.mode === 'shared'
      ? config.shared_budget
      : config.quota_amount / QUOTA_PER_YUAN
    if (Math.abs(cfgMoney - money) > 0.001) return true
    return false
  })()

  return (
    <Card className="border-primary/20">
      <CardHeader className="pb-3">
        <CardTitle className="flex items-center gap-2 text-base">
          <Timer className="h-4 w-4 text-primary" />
          定时刷新额度
          <Badge variant={enabled ? 'success' : 'secondary'} className="ml-1">
            {enabled ? '已开启' : '已停用'}
          </Badge>
        </CardTitle>
        <CardDescription>
          每天固定时间按所选模式重置额度，额度用尽自动停止。上次执行：
          {config?.last_run_at ? `${formatTime(config.last_run_at)}（${config.last_run_info}）` : '从未执行'}
        </CardDescription>
      </CardHeader>
      <CardContent>
        {/* 共享池余额（剩余量）：每 1 分钟自动刷新，可手动刷新 */}
        {mode === 'shared' && (
          <div className="mb-4 flex items-center justify-between rounded-lg border border-primary/20 bg-primary/5 px-4 py-3">
            <div className="flex items-center gap-3">
              <Wallet className="h-5 w-5 text-primary" />
              <div>
                <div className="flex items-center gap-1.5">
                  <span className="text-sm text-muted-foreground">共享池余额（剩余量）</span>
                  <Button
                    variant="ghost"
                    size="icon"
                    className="h-6 w-6"
                    onClick={handleRefreshWallet}
                    title="手动刷新"
                    disabled={walletRefreshing}
                  >
                    {walletRefreshing
                      ? <Loader2 className="h-3.5 w-3.5 animate-spin" />
                      : <RefreshCw className="h-3.5 w-3.5" />}
                  </Button>
                </div>
                <p className="text-[11px] text-muted-foreground">每 1 分钟自动刷新 · 用完都停，次日定时重置</p>
              </div>
            </div>
            <span
              className="cursor-pointer font-mono text-2xl font-bold text-primary transition-opacity hover:opacity-70"
              onClick={() => { fetchCaps(); setUsageDialogOpen(true) }}
              title="点击查看今日各令牌使用金额"
            >
              ¥{(walletBalance / QUOTA_PER_YUAN).toFixed(2)}
            </span>
          </div>
        )}
        {/* 模式选择：每令牌额度（统一设置/分配表） | 共享总额度 */}
        <div className="mb-4 grid gap-2 sm:grid-cols-2">
          {(['per-token', 'shared'] as const).map((m) => (
            <button
              key={m}
              onClick={() => {
                setMode(m)
                // 切换模式时按各自语义重置金额：shared 无已保存值时默认 200 元
                if (m === 'shared') {
                  const saved = config?.shared_budget && config.shared_budget > 0 ? String(config.shared_budget) : '200'
                  setMoneyInput(saved)
                } else {
                  const saved = config?.quota_amount && config.quota_amount > 0
                    ? (config.quota_amount / QUOTA_PER_YUAN).toFixed(2)
                    : ''
                  setMoneyInput(saved)
                }
              }}
              className={`rounded-lg border p-3 text-left transition-colors ${
                mode === m ? 'border-primary bg-primary/5 ring-1 ring-primary' : 'hover:border-primary/40'
              }`}
            >
              <div className="flex items-center gap-1.5 text-sm font-medium">
                {m === 'per-token' ? <Users className="h-4 w-4 text-primary" /> :
                 <Wallet className="h-4 w-4 text-primary" />}
                {MODE_LABELS[m].label}
              </div>
              <p className="mt-1 text-[11px] leading-snug text-muted-foreground">{MODE_LABELS[m].desc}</p>
            </button>
          ))}
        </div>

        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
          <div className="space-y-1.5">
            <label className="block text-xs font-medium text-muted-foreground">定时器开关</label>
            <div className="inline-flex rounded-lg border bg-muted/50 p-1">
              <Button
                variant={enabled ? 'default' : 'ghost'}
                size="sm"
                className="h-8"
                onClick={() => setEnabled(true)}
              >
                开启
              </Button>
              <Button
                variant={!enabled ? 'default' : 'ghost'}
                size="sm"
                className="h-8"
                onClick={() => setEnabled(false)}
              >
                停用
              </Button>
            </div>
            {hasUnsaved && (
              <p className="text-[11px] font-medium text-amber-600">⚠ 有未保存的修改，点「保存配置」生效</p>
            )}
            {!hasUnsaved && config && (
              <p className="text-[11px] text-muted-foreground">已保存，定时器{enabled ? '开启中' : '停用中'}</p>
            )}
          </div>

          <div className="space-y-1.5">
            <label className="block text-xs font-medium text-muted-foreground">
              {mode === 'shared' ? '共享总额度（元/天）' : '额度大小（元/令牌/天）'}
            </label>
            <Input
              type="number"
              min="0.01"
              step="0.01"
              placeholder={mode === 'shared' ? '例如：200' : '例如：5'}
              value={moneyInput}
              onChange={(e) => setMoneyInput(e.target.value)}
            />
            {previewQuota > 0 && (
              <p className="text-[11px] text-muted-foreground">
                {mode === 'shared' ? (
                  <>全部令牌每天共享 <span className="font-mono text-primary">{previewQuota.toLocaleString('zh-CN')}</span> 额度（{money.toFixed(2)} 元），用尽即停</>
                ) : (
                  <>每个令牌每天 <span className="font-mono text-primary">{previewQuota.toLocaleString('zh-CN')}</span> 额度</>
                )}
              </p>
            )}
          </div>

          <div className="space-y-1.5">
            <label className="block text-xs font-medium text-muted-foreground">刷新时间（每天）</label>
            <Input
              type="time"
              value={refreshTime}
              onChange={(e) => setRefreshTime(e.target.value)}
            />
            {nextRunLabel && <p className="text-[11px] text-muted-foreground">{nextRunLabel}</p>}
          </div>

          <div className="space-y-1.5">
            {/* 与其它列相同的 label 占位，保证按钮与时间选择框同一水平线 */}
            <span className="block text-xs">&nbsp;</span>
            <div className="flex gap-2">
              <Button onClick={saveConfig} disabled={saving} className="h-10 flex-1">
                {saving ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : <Save className="mr-2 h-4 w-4" />}
                保存配置
              </Button>
              <Button variant="outline" onClick={runNow} disabled={running} className="h-10" title="立即执行一次刷新">
                {running ? <Loader2 className="h-4 w-4 animate-spin" /> : <Play className="h-4 w-4" />}
              </Button>
            </div>
          </div>
        </div>

        {/* 每令牌额度模式：统一额度在顶部输入；可按用量占比生成分配表；分配表可编辑 */}
        {mode === 'per-token' && (
          <div className="mt-4 space-y-3 border-t pt-3">
            {/* 按用量占比生成分配表入口 */}
            <div className="rounded-lg border border-primary/30 bg-primary/5 p-3">
              <p className="flex items-center gap-1.5 text-sm font-medium">
                <TrendingUp className="h-4 w-4 text-primary" />
                按用量占比生成（近7天）
              </p>
              <p className="mt-1 text-[11px] leading-snug text-muted-foreground">
                输入全站每日预算，按各令牌近7天 token 用量分为高/低两档推荐额度（大额度组 / 小额度组），
                确认后填充到分配表。想所有令牌等额则直接用上方统一额度即可。
              </p>
              <div className="mt-2 flex flex-wrap items-end gap-2">
                <div className="space-y-1">
                  <label className="block text-xs font-medium text-muted-foreground">全站每日预算（元/天）</label>
                  <Input
                    type="number"
                    min="0.01"
                    step="0.01"
                    placeholder="例如：200"
                    value={budgetInput}
                    onChange={(e) => setBudgetInput(e.target.value)}
                    className="h-9 w-36"
                  />
                </div>
                <Button variant="outline" size="sm" onClick={generateAssignments} disabled={genLoading} className="h-9">
                  {genLoading ? <Loader2 className="mr-1.5 h-4 w-4 animate-spin" /> : <Wand2 className="mr-1.5 h-4 w-4" />}
                  生成推荐
                </Button>
              </div>
            </div>

            {/* 分配表：查看/编辑/清空（保存随「保存配置」统一提交） */}
            <div className="space-y-2">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <p className="text-[11px] text-muted-foreground">
                  编辑后无需单独保存，点上方「保存配置」统一提交。有分配表时每天按表重置（优先于统一额度），否则按上方统一额度。
                </p>
                {assignments.length > 0 && (
                  <Button variant="ghost" size="sm" onClick={clearAssignments} className="h-9 text-destructive hover:text-destructive">
                    <Trash2 className="mr-1.5 h-4 w-4" />
                    清空
                  </Button>
                )}
              </div>
              {/* 每日预算：随下方单个令牌额度编辑实时更新 */}
              {assignments.length > 0 && (
                <div className="flex flex-wrap items-center gap-x-3 gap-y-1 rounded-md border border-primary/20 bg-primary/5 px-3 py-2">
                  <span className="text-xs font-medium text-muted-foreground">每日预算</span>
                  <span className="font-mono text-lg font-bold text-primary">¥{(totalAssigned / QUOTA_PER_YUAN).toFixed(2)}</span>
                  <span className="text-[11px] text-muted-foreground">
                    /天 · 共 {assignments.length} 个令牌 · 编辑下方单个令牌额度实时更新
                  </span>
                </div>
              )}
              {assignmentTableJsx}
            </div>
          </div>
        )}

        {/* 共享总额度模式：共享额度池，用完都停，每天重置 */}
        {mode === 'shared' && (
          <div className="mt-4 space-y-3 border-t pt-3">
            <div className="rounded-lg border border-primary/30 bg-primary/5 p-3">
              <p className="flex items-center gap-1.5 text-sm font-medium">
                <Wallet className="h-4 w-4 text-primary" />
                共享额度池：用完都停
              </p>
              <p className="mt-1 text-[11px] leading-snug text-muted-foreground">
                在上方「共享总额度（元/天）」设置全站每日总额度，所有令牌共享同一个额度池：
                池内有余额时正常调用（从管理员账户钱包扣减），池用尽后全部令牌停止使用，每天刷新时间恢复。
              </p>
              {previewQuota > 0 && (
                <p className="mt-2 text-[11px]">
                  当前共享池：<span className="font-mono text-primary">{previewQuota.toLocaleString('zh-CN')}</span>{' '}
                  额度/天（{money.toFixed(2)} 元）
                </p>
              )}
            </div>

            {/* 每人上限：共享池内个人封顶，可开关解除限制 */}
            <div className="rounded-lg border p-3">
              <p className="flex items-center justify-between text-sm font-medium">
                <span className="flex items-center gap-1.5">
                  <LockIcon className="h-4 w-4 text-primary" />
                  每人上限
                </span>
                <Button
                  variant="ghost"
                  size="icon"
                  className="h-6 w-6"
                  onClick={handleRefreshCaps}
                  title="刷新列表与剩余量"
                  disabled={capsRefreshing}
                >
                  {capsRefreshing
                    ? <Loader2 className="h-3.5 w-3.5 animate-spin" />
                    : <RefreshCw className="h-3.5 w-3.5" />}
                </Button>
              </p>
              <p className="mt-1 text-[11px] leading-snug text-muted-foreground">
                每个令牌每日上限（可逐行调整）：达到上限后该令牌自动停用，其余令牌不受影响。
                打开「解除限制」后，该令牌不再受个人上限约束，改用共享池剩余额度（池用尽后全部令牌停止）。
                修改后点「保存上限配置」立即生效。
              </p>
              <div className="mt-2 flex items-center gap-2">
                <span className="whitespace-nowrap text-[11px] text-muted-foreground">快捷批量：</span>
                <Input
                  type="number"
                  min="0"
                  step="0.01"
                  placeholder="金额（元）"
                  className="h-7 w-24"
                  value={bulkCapInput}
                  onChange={(e) => setBulkCapInput(e.target.value)}
                  onKeyDown={(e) => { if (e.key === 'Enter') applyBulkCap() }}
                />
                <Button
                  variant="outline"
                  size="sm"
                  className="h-7 gap-1"
                  onClick={applyBulkCap}
                  disabled={capsSaving}
                  title="一键将全部令牌的每日上限设为该金额"
                >
                  {capsSaving ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Wand2 className="h-3.5 w-3.5" />}
                  应用到全部令牌
                </Button>
              </div>
              <div className="mt-2 max-h-72 overflow-y-auto rounded-md border">
                <table className="w-full text-xs">
                  <thead className="sticky top-0 bg-muted/60">
                    <tr>
                      <th className="px-2.5 py-1.5 text-left font-medium">令牌</th>
                      <th className="px-2.5 py-1.5 text-left font-medium">每日上限（元）</th>
                      <th className="px-2.5 py-1.5 text-right font-medium">当前剩余（元）</th>
                      <th className="px-2.5 py-1.5 text-right font-medium">缓存命中</th>
                      <th className="px-2.5 py-1.5 text-right font-medium">解除限制</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y">
                    {caps.map((c) => (
                      <tr key={c.token_id} className="hover:bg-muted/30">
                        <td className="px-2.5 py-1">{c.token_name}</td>
                        <td className="px-2.5 py-1">
                          <Input
                            type="number"
                            min="0"
                            step="0.01"
                            className="h-7 w-24"
                            value={c.cap_yuan > 0 ? c.cap_yuan : ''}
                            onChange={(e) => updateCap(c.token_id, { cap_yuan: parseFloat(e.target.value) || 0 })}
                            onBlur={(e) => handleCapBlur(c, e.target.value)}
                            onKeyDown={(e) => { if (e.key === 'Enter') handleCapBlur(c, (e.target as HTMLInputElement).value) }}
                          />
                        </td>
                        <td className={`px-2.5 py-1 text-right font-mono ${c.unlimited ? 'text-muted-foreground' : remainClass(c)}`}>
                          {c.unlimited
                            ? <span className="text-[10px]">不限（走共享池）</span>
                            : `¥${Math.max(c.cap_yuan - (c.used_quota || 0) / QUOTA_PER_YUAN, 0).toFixed(2)}`}
                        </td>
                        <td className="px-2.5 py-1 text-right font-mono text-muted-foreground">
                          {c.cache_hit_rate >= 0 ? `${c.cache_hit_rate.toFixed(1)}%` : '—'}
                        </td>
                        <td className="px-2.5 py-1 text-right">
                          <Button
                            variant={c.unlocked ? 'default' : 'outline'}
                            size="sm"
                            className="h-7 gap-1 px-2 text-[11px]"
                            onClick={() => updateCap(c.token_id, { unlocked: !c.unlocked })}
                          >
                            {c.unlocked ? <LockOpenIcon className="h-3 w-3" /> : <LockIcon className="h-3 w-3" />}
                            {c.unlocked ? '已解除' : '已限制'}
                          </Button>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              <div className="mt-2 flex items-center justify-between gap-2">
                <div className="flex min-w-0 flex-col gap-0.5">
                  <span className="text-[11px] text-muted-foreground">
                    共 {caps.length} 个令牌 · {caps.filter(c => c.unlocked).length} 个已解除限制（走共享池）
                  </span>
                  {capsDirty && (
                    <span className="text-[11px] font-medium text-amber-600">
                      ⚠ 有未保存的修改，点「保存上限配置」生效
                    </span>
                  )}
                </div>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => saveCaps()}
                  disabled={capsSaving}
                  className={`gap-1 ${capsDirty ? 'border-amber-500 text-amber-600 hover:bg-amber-50 hover:text-amber-700' : ''}`}
                >
                  {capsSaving ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Save className="h-3.5 w-3.5" />}
                  保存上限配置
                </Button>
              </div>
            </div>
          </div>
        )}

        {runs.length > 0 && (
          <div className="mt-4 border-t pt-3">
            <p className="mb-2 flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
              <History className="h-3.5 w-3.5" />
              最近执行记录
            </p>
            <div className="space-y-1.5">
              {runs.map((r) => (
                <div key={r.id} className="flex flex-wrap items-center gap-x-3 gap-y-1 rounded-md bg-muted/40 px-3 py-1.5 text-xs">
                  <span className="flex items-center gap-1 font-medium">
                    <Zap className="h-3 w-3 text-amber-500" />
                    {formatTime(r.run_at)}
                  </span>
                  <Badge variant={r.status === 'success' ? 'success' : 'secondary'} className="text-[10px] px-1.5">
                    {r.status === 'success' ? '成功' : r.status === 'noop' ? '无操作' : r.status}
                  </Badge>
                  <span className="text-muted-foreground">{r.detail}</span>
                  <span className="ml-auto font-mono">
                    {r.quota_amount > 0 ? `额度 ¥${(r.quota_amount / QUOTA_PER_YUAN).toFixed(2)}` : ''}
                  </span>
                </div>
              ))}
            </div>
          </div>
        )}

        {loading && (
          <div className="mt-4 flex items-center justify-center py-4">
            <Loader2 className="h-5 w-5 animate-spin text-primary" />
          </div>
        )}
      </CardContent>

      {/* 两档额度推荐对话框 */}
      <Dialog open={recDialog} onOpenChange={setRecDialog}>
        <DialogContent className="sm:max-w-4xl">
          <DialogHeader>
            <DialogTitle>按用量占比推荐额度</DialogTitle>
            <DialogDescription>
              按近 7 天 token 用量把令牌分为两档：高用量（大额度）、低用量（小额度）。可调整两档金额，确认后按档位填充到分配表。
            </DialogDescription>
          </DialogHeader>
          {recData && (
            <div className="space-y-4 py-2">
              <div className="grid gap-3 lg:grid-cols-2">
                {/* 大额度组（高用量） */}
                <div className="rounded-lg border border-primary/30 bg-primary/5 p-3">
                  <div className="flex items-center justify-between">
                    <span className="flex items-center gap-1.5 text-sm font-medium">
                      <TrendingUp className="h-4 w-4 text-primary" />
                      大额度（高用量 {recData.big.count} 个令牌）
                    </span>
                    <span className="text-xs text-muted-foreground">
                      合计 ¥{(parseFloat(recBig) * recData.big.count).toFixed(2)}
                    </span>
                  </div>
                  <div className="mt-2 flex items-center gap-2">
                    <span className="text-xs text-muted-foreground">每日额度（元/令牌）</span>
                    <Input
                      type="number"
                      min="0.01"
                      step="0.01"
                      value={recBig}
                      onChange={(e) => setRecBig(e.target.value)}
                      className="h-9 w-28"
                    />
                  </div>
                  <div className="mt-2 max-h-64 overflow-y-auto rounded-md border bg-background/60">
                    <table className="w-full text-xs">
                      <tbody className="divide-y">
                        {recData.big.tokens.map((name, i) => (
                          <tr key={i} className="hover:bg-muted/30">
                            <td className="px-2.5 py-1">{name}</td>
                            <td className="px-2.5 py-1 text-right font-mono text-muted-foreground">
                              ¥{(parseFloat(recBig) || 0).toFixed(2)}/天
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                </div>

                {/* 小额度组（低用量） */}
                <div className="rounded-lg border border-muted bg-muted/30 p-3">
                  <div className="flex items-center justify-between">
                    <span className="flex items-center gap-1.5 text-sm font-medium">
                      <TrendingDown className="h-4 w-4 text-muted-foreground" />
                      小额度（低用量 {recData.small.count} 个令牌）
                    </span>
                    <span className="text-xs text-muted-foreground">
                      合计 ¥{(parseFloat(recSmall) * recData.small.count).toFixed(2)}
                    </span>
                  </div>
                  <div className="mt-2 flex items-center gap-2">
                    <span className="text-xs text-muted-foreground">每日额度（元/令牌）</span>
                    <Input
                      type="number"
                      min="0.01"
                      step="0.01"
                      value={recSmall}
                      onChange={(e) => setRecSmall(e.target.value)}
                      className="h-9 w-28"
                    />
                  </div>
                  <div className="mt-2 max-h-64 overflow-y-auto rounded-md border bg-background/60">
                    <table className="w-full text-xs">
                      <tbody className="divide-y">
                        {recData.small.tokens.map((name, i) => (
                          <tr key={i} className="hover:bg-muted/30">
                            <td className="px-2.5 py-1">{name}</td>
                            <td className="px-2.5 py-1 text-right font-mono text-muted-foreground">
                              ¥{(parseFloat(recSmall) || 0).toFixed(2)}/天
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                </div>
              </div>

              <p className="text-[11px] text-muted-foreground">
                按占比估算总额 ¥{recData.budget.toFixed(2)}/天（含零用量令牌最低额度），当前配置：大额度组 ¥
                {(parseFloat(recBig) * recData.big.count).toFixed(2)} + 小额度组 ¥
                {(parseFloat(recSmall) * recData.small.count).toFixed(2)}
                {recData.idle?.count > 0 && <> + 零用量组 ¥{(parseFloat(recIdle) * recData.idle.count).toFixed(2)}</>}
                ，可自行调整各档数值。
              </p>

              {/* 零用量组（近7天无调用）：最低额度，防突发使用突破预算 */}
              {recData.idle?.count > 0 && (
                <div className="rounded-lg border border-dashed border-muted-foreground/30 bg-muted/20 p-3">
                  <div className="flex items-center justify-between">
                    <span className="flex items-center gap-1.5 text-sm font-medium">
                      <TrendingDown className="h-4 w-4 text-muted-foreground" />
                      零用量（近7天无调用 {recData.idle.count} 个令牌）
                    </span>
                    <span className="text-xs text-muted-foreground">
                      合计 ¥{(parseFloat(recIdle) * recData.idle.count).toFixed(2)}
                    </span>
                  </div>
                  <p className="mt-0.5 text-[11px] text-muted-foreground">
                    固定最低额度，防止这些令牌突发使用突破全站预算；填充后可在分配表中调整。
                  </p>
                  <div className="mt-2 flex items-center gap-2">
                    <span className="text-xs text-muted-foreground">每日额度（元/令牌）</span>
                    <Input
                      type="number"
                      min="0.01"
                      step="0.01"
                      value={recIdle}
                      onChange={(e) => setRecIdle(e.target.value)}
                      className="h-9 w-28"
                    />
                  </div>
                  <div className="mt-2 max-h-32 overflow-y-auto rounded-md border bg-background/60">
                    <table className="w-full text-xs">
                      <tbody className="divide-y">
                        {recData.idle.tokens.map((name, i) => (
                          <tr key={i} className="hover:bg-muted/30">
                            <td className="px-2.5 py-1">{name}</td>
                            <td className="px-2.5 py-1 text-right font-mono text-muted-foreground">
                              ¥{(parseFloat(recIdle) || 0).toFixed(2)}/天
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                </div>
              )}
            </div>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setRecDialog(false)} disabled={saving}>
              取消
            </Button>
            <Button onClick={confirmRecommend} disabled={saving}>
              {saving ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : <Wand2 className="mr-2 h-4 w-4" />}
              确定填充
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* 共享池余额点击弹框：今日各令牌使用金额明细 */}
      <Dialog open={usageDialogOpen} onOpenChange={setUsageDialogOpen}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>今日使用金额（按令牌）</DialogTitle>
            <DialogDescription>
              当天 0 点起各令牌实际消耗金额（logs type=2），按使用量降序排列
            </DialogDescription>
          </DialogHeader>
          <div className="max-h-72 overflow-y-auto rounded-md border">
            <table className="w-full text-xs">
              <thead className="sticky top-0 bg-muted/60">
                <tr>
                  <th className="px-2.5 py-1.5 text-left font-medium">令牌</th>
                  <th className="px-2.5 py-1.5 text-right font-medium">今日使用（元）</th>
                  <th className="px-2.5 py-1.5 text-right font-medium">当前剩余（元）</th>
                  <th className="px-2.5 py-1.5 text-right font-medium">缓存命中</th>
                </tr>
              </thead>
              <tbody className="divide-y">
                {[...caps]
                  .sort((a, b) => (b.used_quota || 0) - (a.used_quota || 0))
                  .map((c) => (
                    <tr key={c.token_id} className={c.used_quota > 0 ? '' : 'opacity-50'}>
                      <td className="px-2.5 py-1">{c.token_name}</td>
                      <td className="px-2.5 py-1 text-right font-mono">
                        ¥{((c.used_quota || 0) / QUOTA_PER_YUAN).toFixed(2)}
                      </td>
                      <td className={`px-2.5 py-1 text-right font-mono ${c.unlimited ? 'text-muted-foreground' : remainClass(c)}`}>
                        {c.unlimited
                          ? '—'
                          : `¥${Math.max(c.cap_yuan - (c.used_quota || 0) / QUOTA_PER_YUAN, 0).toFixed(2)}`}
                      </td>
                      <td className="px-2.5 py-1 text-right font-mono text-muted-foreground">
                        {c.cache_hit_rate >= 0 ? `${c.cache_hit_rate.toFixed(1)}%` : '—'}
                      </td>
                    </tr>
                  ))}
              </tbody>
            </table>
          </div>
          <DialogFooter className="flex items-center justify-between gap-2">
            <span className="text-xs text-muted-foreground">共 {caps.length} 个令牌</span>
            <span className="text-sm font-medium">
              今日合计：
              <span className="font-mono text-primary">
                ¥{(caps.reduce((s, c) => s + (c.used_quota || 0), 0) / QUOTA_PER_YUAN).toFixed(2)}
              </span>
            </span>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </Card>
  )
}

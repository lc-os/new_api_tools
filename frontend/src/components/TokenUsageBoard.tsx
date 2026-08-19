import { useState, useCallback, useEffect } from 'react'
import { useAuth } from '../contexts/AuthContext'
import { useToast } from './Toast'
import { ChartColumn, LoaderCircle, Search, RefreshCw, X, Filter } from 'lucide-react'
import { Card, CardContent, CardHeader, CardTitle } from './ui/card'
import { Button } from './ui/button'
import { Badge } from './ui/badge'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from './ui/table'
import { cn } from '../lib/utils'

const PERIODS = [
  { label: '全部时间', value: 'all' },
  { label: '今天', value: 'today' },
  { label: '近 7 天', value: '7d' },
  { label: '近 30 天', value: '30d' },
  { label: '近 90 天', value: '90d' },
]

const TOP_LIMIT = 10

interface BoardSummary {
  total_tokens: number
  prompt_tokens: number
  completion_tokens: number
  request_count: number
  token_count: number
  model_count: number
}

interface TokenOption {
  token_id: number
  user_name: string
}

interface ModelOption {
  model_name: string
  request_count: number
}

interface DailyTopUser {
  day: string
  token_id: number
  user_name: string
  top_model_name: string
  request_count: number
  total_tokens: number
}

interface DailyTopModel {
  day: string
  model_name: string
  token_count: number
  request_count: number
  total_tokens: number
}

interface BoardLogItem {
  id: number
  created_at: number
  user_name: string
  token_id: number
  model_name: string
  prompt_tokens: number
  completion_tokens: number
  channel_id: number
  group_name: string
  request_id: string
}

interface BoardLogs {
  total: number
  page_size: number
  items: BoardLogItem[]
}

interface BoardData {
  meta: { generated_at: number }
  summary: BoardSummary
  token_options: TokenOption[]
  model_options: ModelOption[]
  daily_top_users: DailyTopUser[]
  daily_top_models: DailyTopModel[]
  logs: BoardLogs
}

function getTimeRange(period: string): { start: number; end: number } {
  const now = Math.floor(Date.now() / 1000)
  const d = new Date()
  const todayStart = Math.floor(new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime() / 1000)
  const daySec = 1440 * 60
  switch (period) {
    case 'all':
      return { start: 0, end: now }
    case 'today':
      return { start: todayStart, end: now }
    case '7d':
      return { start: now - 7 * daySec, end: now }
    case '90d':
      return { start: now - 90 * daySec, end: now }
    default:
      return { start: now - 30 * daySec, end: now }
  }
}

function formatNumber(n: number) {
  return (n || 0).toLocaleString('zh-CN')
}

function formatCompact(n: number) {
  if (!n) return '0'
  if (n >= 1e9) return `${(n / 1e9).toFixed(2)}B`
  if (n >= 1e6) return `${(n / 1e6).toFixed(2)}M`
  if (n >= 1e3) return `${(n / 1e3).toFixed(1)}K`
  return formatNumber(n)
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

export function TokenUsageBoard() {
  const { token } = useAuth()
  const { showToast } = useToast()
  const [data, setData] = useState<BoardData | null>(null)
  const [status, setStatus] = useState<'idle' | 'loading' | 'error'>('idle')
  const [error, setError] = useState('')
  const [period, setPeriod] = useState('30d')
  const [model, setModel] = useState('')
  const [tokenId, setTokenId] = useState(0)
  const [page, setPage] = useState(1)

  const fetchBoard = useCallback(async (p = page, tid = tokenId, mdl = model, per = period) => {
    if (!token) return
    setStatus('loading')
    setError('')
    const range = getTimeRange(per)
    const params = new URLSearchParams({
      start: String(range.start),
      end: String(range.end),
      page: String(p),
      page_size: '50',
    })
    if (mdl) params.set('model', mdl)
    if (tid > 0) params.set('token_id', String(tid))
    try {
      const res = await fetch(`/api/token-usage/board?${params.toString()}`, {
        headers: {
          'Content-Type': 'application/json',
          Authorization: `Bearer ${token}`,
        },
        cache: 'no-store',
      })
      const json = await res.json()
      if (!res.ok || !json.success) throw new Error(json?.error?.message || json?.message || '加载用量看板失败')
      setData(json.data)
      setPage(p)
      setTokenId(tid)
      setModel(mdl)
      setPeriod(per)
      setStatus('idle')
    } catch (e) {
      const msg = e instanceof Error ? e.message : '加载用量看板失败'
      setStatus('error')
      setError(msg)
      showToast('error', msg)
    }
  }, [period, model, page, showToast, token, tokenId])

  useEffect(() => {
    fetchBoard(1, 0, '', '30d')
  }, [token])

  const selectUser = (id: number) => {
    fetchBoard(1, tokenId === id ? 0 : id, model, period)
  }
  const selectModel = (m: string) => {
    fetchBoard(1, tokenId, model === m ? '' : m, period)
  }
  const clearFilters = () => {
    fetchBoard(1, 0, '', period)
  }

  const hasDrilldown = tokenId > 0 || !!model
  const totalPages = data ? Math.max(Math.ceil(data.logs.total / data.logs.page_size), 1) : 1
  const summary = data?.summary

  return (
    <div className="space-y-6">
      <Card>
        <CardContent className="p-4 sm:p-5">
          <div className="flex flex-col gap-4 lg:flex-row lg:items-end lg:justify-between">
            <div className="min-w-0">
              <div className="flex items-center gap-2">
                <ChartColumn className="h-5 w-5 text-primary" />
                <h2 className="text-lg font-semibold">用量看板</h2>
              </div>
              <p className="mt-1 text-sm text-muted-foreground">
                {data?.meta?.generated_at ? (
                  <span className="ml-2">更新于 {formatTime(data.meta.generated_at)}</span>
                ) : null}
              </p>
            </div>
            <div className="grid w-full gap-2 sm:grid-cols-2 lg:grid-cols-4 lg:w-auto">
              <label className="flex flex-col gap-1 text-xs font-medium text-muted-foreground">
                时间范围
                <select
                  value={period}
                  onChange={(e) => setPeriod(e.target.value)}
                  className="h-9 rounded-md border border-input bg-background px-2 text-sm text-foreground"
                >
                  {PERIODS.map((p) => (
                    <option key={p.value} value={p.value}>
                      {p.label}
                    </option>
                  ))}
                </select>
              </label>
              <label className="flex flex-col gap-1 text-xs font-medium text-muted-foreground">
                用户令牌
                <select
                  value={tokenId}
                  onChange={(e) => setTokenId(Number(e.target.value))}
                  className="h-9 max-w-[220px] rounded-md border border-input bg-background px-2 text-sm text-foreground"
                >
                  <option value={0}>全部用户令牌</option>
                  {(data?.token_options || []).map((t) => (
                    <option key={t.token_id} value={t.token_id}>
                      {t.user_name}
                    </option>
                  ))}
                </select>
              </label>
              <label className="flex flex-col gap-1 text-xs font-medium text-muted-foreground">
                模型
                <select
                  value={model}
                  onChange={(e) => setModel(e.target.value)}
                  className="h-9 max-w-[220px] rounded-md border border-input bg-background px-2 text-sm text-foreground"
                >
                  <option value="">全部模型</option>
                  {(data?.model_options || []).map((m) => (
                    <option key={m.model_name} value={m.model_name}>
                      {m.model_name} · {formatNumber(m.request_count)} 次
                    </option>
                  ))}
                </select>
              </label>
              <div className="flex items-end gap-2">
                <Button
                  onClick={() => fetchBoard(1, tokenId, model, period)}
                  disabled={status === 'loading'}
                  className="h-9 flex-1"
                >
                  {status === 'loading' ? (
                    <LoaderCircle className="mr-2 h-4 w-4 animate-spin" />
                  ) : (
                    <Search className="mr-2 h-4 w-4" />
                  )}
                  查询
                </Button>
                <Button
                  variant="outline"
                  size="icon"
                  className="h-9 w-9"
                  onClick={() => fetchBoard(page, tokenId, model, period)}
                  disabled={status === 'loading'}
                  title="刷新"
                >
                  <RefreshCw className={cn('h-4 w-4', status === 'loading' && 'animate-spin')} />
                </Button>
              </div>
            </div>
          </div>
          {hasDrilldown && (
            <div className="mt-3 flex flex-wrap items-center gap-2">
              <Filter className="h-3.5 w-3.5 text-muted-foreground" />
              <span className="text-xs text-muted-foreground">当前下钻：</span>
              {tokenId > 0 && (
                <Badge variant="outline" className="gap-1">
                  令牌 #{tokenId}
                  <button type="button" onClick={() => fetchBoard(1, 0, model, period)} className="ml-1">
                    <X className="h-3 w-3" />
                  </button>
                </Badge>
              )}
              {model && (
                <Badge variant="outline" className="gap-1">
                  {model}
                  <button type="button" onClick={() => fetchBoard(1, tokenId, '', period)} className="ml-1">
                    <X className="h-3 w-3" />
                  </button>
                </Badge>
              )}
              <Button variant="ghost" size="sm" className="h-7 text-xs" onClick={clearFilters}>
                清除筛选
              </Button>
            </div>
          )}
        </CardContent>
      </Card>

      {error && (
        <Card className="border-destructive/40 bg-destructive/5">
          <CardContent className="p-4 text-sm text-destructive">{error}</CardContent>
        </Card>
      )}

      {!data && status === 'loading' && (
        <div className="flex min-h-[320px] items-center justify-center">
          <LoaderCircle className="h-10 w-10 animate-spin text-primary" />
        </div>
      )}

      {data && summary && (
        <>
          <section className="grid gap-3 sm:grid-cols-3">
            <SummaryCard
              label="Tokens"
              value={formatCompact(summary.total_tokens)}
              hint={`${formatCompact(summary.prompt_tokens)} 入 / ${formatCompact(summary.completion_tokens)} 出`}
            />
            <SummaryCard
              label="请求数"
              value={formatNumber(summary.request_count)}
              hint={`${formatNumber(summary.token_count)} 令牌 · ${formatNumber(summary.model_count)} 模型`}
            />
            <SummaryCard
              label="日志明细"
              value={formatNumber(data.logs.total)}
              hint="当前条件下可分页查看"
            />
          </section>

          <section className="grid gap-4 xl:grid-cols-2">
            <DailyTopUsers items={data.daily_top_users} onSelectUser={selectUser} />
            <DailyTopModels items={data.daily_top_models} onSelectModel={selectModel} />
          </section>

          <LogTable
            logs={data.logs}
            page={page}
            totalPages={totalPages}
            loading={status === 'loading'}
            onPageChange={(p) => fetchBoard(p, tokenId, model, period)}
          />
        </>
      )}

      {!data && status === 'idle' && !error && (
        <Card className="border-dashed">
          <CardContent className="p-10 text-center text-sm text-muted-foreground">
            还没有可展示的数据，点击「查询」加载。
          </CardContent>
        </Card>
      )}
    </div>
  )
}

function SummaryCard({ label, value, hint }: { label: string; value: string; hint: string }) {
  return (
    <Card>
      <CardContent className="p-4">
        <p className="text-xs font-medium text-muted-foreground">{label}</p>
        <p className="mt-1.5 text-xl font-bold tracking-tight">{value}</p>
        <p className="mt-1 text-xs text-muted-foreground">{hint}</p>
      </CardContent>
    </Card>
  )
}

function SectionCard({ title, extra, children }: { title: string; extra?: string; children: React.ReactNode }) {
  return (
    <Card className="flex flex-col">
      <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2 p-4">
        <CardTitle className="text-base font-semibold">{title}</CardTitle>
        {extra ? <span className="text-xs text-muted-foreground">{extra}</span> : null}
      </CardHeader>
      <CardContent className="p-4 pt-0">{children}</CardContent>
    </Card>
  )
}

function DailyTopUsers({ items, onSelectUser }: { items: DailyTopUser[]; onSelectUser: (id: number) => void }) {
  const top = items.slice(0, TOP_LIMIT)
  return (
    <SectionCard title="每日用量冠军" extra={`最近 ${top.length} 天 · 按当日请求数`}>
      <div className="space-y-2">
        {top.map((item, i) => (
          <button
            type="button"
            key={`${item.day}-${item.token_id}`}
            onClick={() => onSelectUser(item.token_id)}
            className="grid w-full grid-cols-[3.4rem_1fr_auto] items-center gap-3 rounded-lg border bg-muted/30 px-3 py-2.5 text-left transition hover:border-primary/40 hover:bg-primary/5"
          >
            <div className="rounded-md bg-background px-2 py-1.5 text-center shadow-sm">
              <p className="text-xs font-bold text-primary">#{i + 1}</p>
              <p className="text-[10px] text-muted-foreground">{item.day.slice(5)}</p>
            </div>
            <div className="min-w-0">
              <p className="truncate text-sm font-semibold">{item.user_name}</p>
              <p className="mt-0.5 truncate text-xs text-muted-foreground">主力：{item.top_model_name}</p>
            </div>
            <div className="text-right">
              <p className="text-sm font-semibold text-primary">{formatNumber(item.request_count)} 次</p>
              <p className="text-xs text-muted-foreground">{formatCompact(item.total_tokens)} tokens</p>
            </div>
          </button>
        ))}
        {top.length === 0 && (
          <p className="rounded-lg bg-muted/40 p-6 text-center text-sm text-muted-foreground">暂无每日排行</p>
        )}
      </div>
    </SectionCard>
  )
}

function DailyTopModels({ items, onSelectModel }: { items: DailyTopModel[]; onSelectModel: (m: string) => void }) {
  const top = items.slice(0, TOP_LIMIT)
  return (
    <SectionCard title="每日模型冠军" extra={`最近 ${top.length} 天`}>
      <div className="space-y-2">
        {top.map((item, i) => (
          <button
            type="button"
            key={`${item.day}-${item.model_name}`}
            onClick={() => onSelectModel(item.model_name)}
            className="grid w-full grid-cols-[3.4rem_1fr_auto] items-center gap-3 rounded-lg border bg-muted/30 px-3 py-2.5 text-left transition hover:border-primary/40 hover:bg-primary/5"
          >
            <div className="rounded-md bg-background px-2 py-1.5 text-center shadow-sm">
              <p className="text-xs font-bold text-primary">#{i + 1}</p>
              <p className="text-[10px] text-muted-foreground">{item.day.slice(5)}</p>
            </div>
            <div className="min-w-0">
              <p className="truncate text-sm font-semibold">{item.model_name}</p>
              <p className="mt-0.5 text-xs text-muted-foreground">覆盖 {formatNumber(item.token_count)} 令牌</p>
            </div>
            <div className="text-right">
              <p className="text-sm font-semibold text-primary">{formatNumber(item.request_count)} 次</p>
              <p className="text-xs text-muted-foreground">{formatCompact(item.total_tokens)} tokens</p>
            </div>
          </button>
        ))}
        {top.length === 0 && (
          <p className="rounded-lg bg-muted/40 p-6 text-center text-sm text-muted-foreground">暂无每日模型数据</p>
        )}
      </div>
    </SectionCard>
  )
}

function LogTable({
  logs,
  page,
  totalPages,
  loading,
  onPageChange,
}: {
  logs: BoardLogs
  page: number
  totalPages: number
  loading: boolean
  onPageChange: (p: number) => void
}) {
  return (
    <SectionCard title="消费日志明细" extra={`共 ${formatNumber(logs.total)} 条`}>
      <div className="max-h-[520px] overflow-auto">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>时间</TableHead>
              <TableHead>用户/令牌</TableHead>
              <TableHead>模型</TableHead>
              <TableHead className="text-right">输入</TableHead>
              <TableHead className="text-right">输出</TableHead>
              <TableHead className="text-right">通道</TableHead>
              <TableHead>分组</TableHead>
              <TableHead>请求 ID</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {logs.items.map((item) => (
              <TableRow key={item.id}>
                <TableCell className="whitespace-nowrap text-muted-foreground">{formatTime(item.created_at)}</TableCell>
                <TableCell>
                  <p className="font-medium">{item.user_name}</p>
                  <p className="text-xs text-muted-foreground">#{item.token_id}</p>
                </TableCell>
                <TableCell className="font-medium">{item.model_name}</TableCell>
                <TableCell className="text-right">{formatNumber(item.prompt_tokens)}</TableCell>
                <TableCell className="text-right">{formatNumber(item.completion_tokens)}</TableCell>
                <TableCell className="text-right">{item.channel_id || '-'}</TableCell>
                <TableCell>{item.group_name || '-'}</TableCell>
                <TableCell className="max-w-[12rem] truncate text-xs text-muted-foreground">
                  {item.request_id || '-'}
                </TableCell>
              </TableRow>
            ))}
            {logs.items.length === 0 && (
              <TableRow>
                <TableCell colSpan={8} className="py-10 text-center text-muted-foreground">
                  当前筛选条件下没有日志
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </div>
      <div className="mt-3 flex items-center justify-between border-t pt-3 text-sm text-muted-foreground">
        <span>
          第 {page} / {totalPages} 页{loading ? ' · 加载中…' : ''}
        </span>
        <div className="flex gap-2">
          <Button variant="outline" size="sm" disabled={page <= 1 || loading} onClick={() => onPageChange(page - 1)}>
            上一页
          </Button>
          <Button
            variant="outline"
            size="sm"
            disabled={page >= totalPages || loading}
            onClick={() => onPageChange(page + 1)}
          >
            下一页
          </Button>
        </div>
      </div>
    </SectionCard>
  )
}

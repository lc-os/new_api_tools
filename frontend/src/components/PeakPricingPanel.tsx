import { useState, useEffect, useCallback } from 'react'
import { useAuth } from '../contexts/AuthContext'
import { useToast } from './Toast'
import { Sun, Loader2, Save, Play, Camera, TrendingUp } from 'lucide-react'
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from './ui/card'
import { Button } from './ui/button'
import { Badge } from './ui/badge'
import { Input } from './ui/input'

interface PeakBaseline {
  model_ratio: Record<string, number>
  completion_ratio: Record<string, number>
  cache_ratio: Record<string, number>
  captured_at: number
}

interface PeakPricingConfig {
  enabled: boolean
  peak_periods: string
  offpeak_ratio: number
  peak_multiplier: number
  current_state: string // 上次应用的时段
  now_state: string // 当前时刻所处时段
  last_switch_at: number
  updated_at: number
  baseline: PeakBaseline
  prices: ModelPrice[]
}

interface ModelPrice {
  model: string
  peak_hit: number // 缓存命中 元/百万tokens
  peak_miss: number // 输入未命中 元/百万tokens
  peak_output: number // 输出 元/百万tokens
  offpeak_hit: number
  offpeak_miss: number
  offpeak_output: number
}

function fmtPrice(v: number) {
  if (v == null) return '-'
  return v.toFixed(3).replace(/\.?0+$/, '')
}

export function PeakPricingPanel() {
  const { token } = useAuth()
  const { showToast } = useToast()
  const apiUrl = import.meta.env.VITE_API_URL || ''

  const getAuthHeaders = useCallback(() => ({
    'Content-Type': 'application/json',
    'Authorization': `Bearer ${token}`,
  }), [token])

  const [cfg, setCfg] = useState<PeakPricingConfig | null>(null)
  const [enabled, setEnabled] = useState(false)
  const [peakPeriods, setPeakPeriods] = useState('09:00-12:00,14:00-18:00')
  const [peakMultiplier, setPeakMultiplier] = useState('2')
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [applying, setApplying] = useState(false)
  const [capturing, setCapturing] = useState(false)

  const fetchAll = useCallback(async () => {
    try {
      const res = await fetch(`${apiUrl}/api/peak-pricing/config`, { headers: getAuthHeaders() })
      const data = await res.json()
      if (data.success) {
        setCfg(data.data)
        setEnabled(data.data.enabled)
        setPeakPeriods(data.data.peak_periods || '09:00-12:00,14:00-18:00')
        setPeakMultiplier(String(data.data.peak_multiplier || 2))
      }
    } catch (e) {
      showToast('error', '加载时段定价配置失败')
    } finally {
      setLoading(false)
    }
  }, [apiUrl, getAuthHeaders, showToast])

  useEffect(() => { fetchAll() }, [fetchAll])

  const saveConfig = async () => {
    const multiplier = parseFloat(peakMultiplier)
    if (!peakPeriods.trim()) { showToast('error', '请填写高峰时间段'); return }
    if (!multiplier || multiplier < 1 || multiplier > 10) { showToast('error', '高峰价格倍数应在 1~10 之间'); return }
    setSaving(true)
    try {
      const res = await fetch(`${apiUrl}/api/peak-pricing/config`, {
        method: 'PUT',
        headers: getAuthHeaders(),
        body: JSON.stringify({ enabled, peak_periods: peakPeriods, peak_multiplier: multiplier }),
      })
      const data = await res.json()
      if (!data.success) throw new Error(data?.error?.message || '保存失败')
      showToast('success', `配置已保存并立即生效${data.data.captured ? '（已捕获空闲价基准）' : ''}${data.data.state_desc ? '：' + data.data.state_desc : ''}`)
      fetchAll()
    } catch (e) {
      showToast('error', e instanceof Error ? e.message : '保存失败')
    } finally {
      setSaving(false)
    }
  }

  const applyNow = async () => {
    setApplying(true)
    try {
      const res = await fetch(`${apiUrl}/api/peak-pricing/apply`, {
        method: 'POST',
        headers: getAuthHeaders(),
      })
      const data = await res.json()
      if (!data.success) throw new Error(data?.error?.message || '应用失败')
      showToast('success', `已应用${data.data.state_desc}`)
      fetchAll()
    } catch (e) {
      showToast('error', e instanceof Error ? e.message : '应用失败')
    } finally {
      setApplying(false)
    }
  }

  const captureBaseline = async () => {
    setCapturing(true)
    try {
      const res = await fetch(`${apiUrl}/api/peak-pricing/capture`, {
        method: 'POST',
        headers: getAuthHeaders(),
      })
      const data = await res.json()
      if (!data.success) throw new Error(data?.error?.message || '捕获失败')
      showToast('success', '已捕获当前 new-api 定价为高峰价基准')
      fetchAll()
    } catch (e) {
      showToast('error', e instanceof Error ? e.message : '捕获失败')
    } finally {
      setCapturing(false)
    }
  }

  const baseline = cfg?.baseline
  const multiplier = parseFloat(peakMultiplier) || 2
  const nowState = cfg?.now_state === 'peak' ? '高峰' : '空闲'

  return (
    <Card className="border-primary/20">
      <CardHeader className="pb-3">
        <CardTitle className="flex items-center gap-2 text-base">
          <Sun className="h-4 w-4 text-amber-500" />
          高峰/空闲时段定价（DeepSeek）
          <Badge variant={enabled ? 'success' : 'secondary'} className="ml-1">
            {enabled ? '已启用' : '未启用'}
          </Badge>
        </CardTitle>
        <CardDescription>
          默认按<b>空闲价</b>计费；高峰时段（北京时间 9:00-12:00、14:00-18:00）价格为空闲价的
          {multiplier} 倍。开启/停用/调整后点「保存配置」立即生效，系统每分钟检查时段自动切换 deepseek-v4-* 定价。
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
          <div className="space-y-1.5">
            <label className="block text-xs font-medium text-muted-foreground">启用时段定价</label>
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
            <p className="text-[11px] text-muted-foreground">当前时刻：{nowState}时段</p>
          </div>

          <div className="space-y-1.5">
            <label className="block text-xs font-medium text-muted-foreground">高峰时间段（北京时间）</label>
            <Input
              value={peakPeriods}
              onChange={(e) => setPeakPeriods(e.target.value)}
              placeholder="09:00-12:00,14:00-18:00"
              className="font-mono"
            />
            <p className="text-[11px] text-muted-foreground">多个时段用逗号分隔，如 09:00-12:00,14:00-18:00</p>
          </div>

          <div className="space-y-1.5">
            <label className="block text-xs font-medium text-muted-foreground">高峰价格倍数（空闲价 × N）</label>
            <div className="flex items-center gap-2">
              <Input
                type="number"
                min="1"
                max="10"
                step="0.5"
                value={peakMultiplier}
                onChange={(e) => setPeakMultiplier(e.target.value)}
                className="w-28"
              />
              <span className="text-xs text-muted-foreground">高峰价 = 空闲价 × {multiplier}</span>
            </div>
            <p className="text-[11px] text-muted-foreground">官方默认 2（高峰价 = 空闲价 × 2）</p>
          </div>

          <div className="space-y-1.5">
            <span className="block text-xs">&nbsp;</span>
            <div className="flex flex-col gap-2">
              <Button onClick={saveConfig} disabled={saving} className="h-9">
                {saving ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : <Save className="mr-2 h-4 w-4" />}
                保存配置
              </Button>
              <div className="flex gap-2">
                <Button variant="outline" onClick={applyNow} disabled={applying || !enabled} className="h-9 flex-1" title="立即按当前时刻应用价格">
                  {applying ? <Loader2 className="h-4 w-4 animate-spin" /> : <Play className="h-4 w-4" />}
                  立即应用
                </Button>
                <Button variant="outline" onClick={captureBaseline} disabled={capturing} className="h-9" title="把当前 new-api 定价捕获为空闲价（默认价）基准">
                  {capturing ? <Loader2 className="h-4 w-4 animate-spin" /> : <Camera className="h-4 w-4" />}
                </Button>
              </div>
            </div>
          </div>
        </div>

        {/* 模型定价预览（直接读取模型定价，元/百万tokens） */}
        {cfg?.prices && cfg.prices.length > 0 ? (
          <div className="border-t pt-3">
            <p className="mb-2 flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
              <TrendingUp className="h-3.5 w-3.5" />
              模型定价（元 / 百万 tokens，直接读取模型定价；空闲价 = 基准（默认），高峰价 = 空闲 × {multiplier}）
              {cfg.baseline && cfg.baseline.captured_at > 0 && (
                <span className="font-normal">｜基准捕获于 {new Date(cfg.baseline.captured_at * 1000).toLocaleString('zh-CN')}</span>
              )}
            </p>
            <div className="overflow-x-auto rounded-lg border">
              <table className="w-full text-xs">
                <thead className="bg-muted/80">
                  <tr className="text-left text-muted-foreground">
                    <th className="px-3 py-2 font-medium">模型</th>
                    <th className="px-3 py-2 font-medium">输入（未命中）元/百万</th>
                    <th className="px-3 py-2 font-medium">输入（缓存命中）元/百万</th>
                    <th className="px-3 py-2 font-medium">输出元/百万</th>
                  </tr>
                </thead>
                <tbody className="divide-y">
                  {cfg.prices.map((p) => (
                    <tr key={p.model} className="hover:bg-muted/30">
                      <td className="px-3 py-1.5 font-medium">{p.model}</td>
                      <td className="px-3 py-1.5">
                        <span className="font-mono text-primary">{fmtPrice(p.peak_miss)}</span>
                        <span className="mx-1 text-muted-foreground">/</span>
                        <span className="font-mono text-muted-foreground">{fmtPrice(p.offpeak_miss)}</span>
                      </td>
                      <td className="px-3 py-1.5">
                        <span className="font-mono text-primary">{fmtPrice(p.peak_hit)}</span>
                        <span className="mx-1 text-muted-foreground">/</span>
                        <span className="font-mono text-muted-foreground">{fmtPrice(p.offpeak_hit)}</span>
                      </td>
                      <td className="px-3 py-1.5">
                        <span className="font-mono text-primary">{fmtPrice(p.peak_output)}</span>
                        <span className="mx-1 text-muted-foreground">/</span>
                        <span className="font-mono text-muted-foreground">{fmtPrice(p.offpeak_output)}</span>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <p className="mt-1.5 text-[11px] text-muted-foreground">
              切换时段时后台按此模型定价改写 new-api 的 ModelRatio（输出/命中价随 ModelRatio 同比例缩放），
              只影响 deepseek-v4-* 四个模型，其余模型不变。
            </p>
          </div>
        ) : baseline ? (
          <div className="border-t pt-3 text-xs text-muted-foreground">
            尚未捕获高峰价基准，模型定价不可用。点击「捕获基准」从 new-api 模型定价读取当前 deepseek-v4-* 价格。
          </div>
        ) : null}

        {loading && (
          <div className="flex items-center justify-center py-3">
            <Loader2 className="h-5 w-5 animate-spin text-primary" />
          </div>
        )}
      </CardContent>
    </Card>
  )
}

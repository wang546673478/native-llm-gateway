// 时间展示工具 — 后端统一返回 UTC RFC3339(如 "2026-08-05T06:08:54Z"),
// 前端一律转本地时区再展示,避免中国用户看到 UTC 时间(差 8 小时)。

// fmtTime 只显示时分秒(本地时区),非法/空值 → '—'
export function fmtTime(v?: string | null): string {
  const d = parseUTC(v)
  if (!d) return '—'
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
}

// fmtDateTime 显示完整本地时间 "YYYY-MM-DD HH:MM:SS",非法/空值 → '—'
export function fmtDateTime(v?: string | null): string {
  const d = parseUTC(v)
  if (!d) return '—'
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${fmtTime(v)}`
}

function parseUTC(v?: string | null): Date | null {
  if (!v) return null
  const d = new Date(v)
  if (isNaN(d.getTime())) return null
  return d
}

function pad(n: number): string {
  return String(n).padStart(2, '0')
}

import type { Locale } from './messages'

// Status codes and reasons the API sends as snake_case identifiers. They
// are shown as words: English drops the underscores (which is what the
// interface did before it had a second language); Traditional Chinese
// looks the code up here. Every set is open on the server side (the API
// documents its statuses as "open for new states"), so an unknown code
// falls back to the code itself rather than to a wrong word.
export type Group = 'order' | 'side' | 'market' | 'deposit' | 'withdrawal' | 'feed' | 'role' | 'reject'

const enumsZhTW: Record<Group, Record<string, string>> = {
  order: {
    open: '掛單中',
    partially_filled: '部分成交',
    filled: '已成交',
    cancelled: '已取消',
    rejected: '已拒絕',
  },
  side: { buy: '買', sell: '賣' },
  market: {
    active: '交易中',
    halted: '暫停',
    cancel_only: '僅可撤單',
    delisted: '已下架',
  },
  deposit: {
    detected: '已偵測',
    confirming: '確認中',
    credited: '已入帳',
    orphaned: '區塊已被重組',
    dropped: '已丟棄',
    reversed: '已沖銷',
  },
  withdrawal: {
    requested: '已申請',
    policy_check: '風控檢查中',
    auto_approved: '自動核准',
    pending_review: '待人工審核',
    approved: '已核准',
    funds_locked: '資金已鎖定',
    signed: '已簽章',
    broadcast: '已廣播',
    confirmed: '已確認',
    rejected: '已拒絕',
    failed: '失敗',
  },
  feed: {
    connecting: '連線中',
    authenticating: '驗證中',
    open: '已連線',
    live: '即時',
    closed: '已中斷',
  },
  role: { maker: '掛單方', taker: '吃單方' },
  reject: {
    invalid_order: '訂單格式不正確',
    invalid_price_tick: '價格不在最小跳動的格點上',
    invalid_qty_step: '數量不是最小單位的整數倍',
    below_min_notional: '低於最小成交金額',
    above_max_qty: '超過最大數量',
    duplicate_order_id: '訂單編號重複',
    empty_book: '訂單簿是空的,沒有對手單',
    quote_qty_too_small: '花費金額太小',
    market_not_active: '市場目前不接受新單',
    insufficient_balance: '餘額不足',
    account_frozen: '帳戶已凍結',
    policy_denied: '風控規則拒絕',
    unknown: '原因不明',
  },
}

export function enumLabel(locale: Locale, group: Group, raw: string): string {
  if (locale === 'zh-TW') return enumsZhTW[group][raw] ?? raw
  return raw.replace(/_/g, ' ')
}

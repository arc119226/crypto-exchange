// The interface's two languages (ADR-0010). English is the source: every
// key is declared here once, `Key` is derived from it, and the Traditional
// Chinese catalogue is typed against it so a missing translation fails
// `tsc` instead of showing a key to a user. Amounts are never part of a
// message -- they stay decimal strings and are placed next to the text.
//
// Placeholders are `{name}`; the values are inserted verbatim.

export type Locale = 'en' | 'zh-TW'

export const LOCALES: readonly Locale[] = ['en', 'zh-TW']

export const en = {
  'common.loading': 'Loading…',
  'col.time': 'Time',
  'col.asset': 'Asset',
  'col.price': 'Price',
  'col.qty': 'Qty',
  'col.side': 'Side',
  'col.status': 'Status',
  'col.amount': 'Amount',
  'col.tx': 'Tx',

  'nav.markets': 'Markets',
  'nav.wallet': 'Wallet',
  'nav.login': 'Log in',
  'nav.register': 'Register',
  'nav.logout': 'Log out',
  'nav.private_stream': 'private stream: {status}',
  'lang.switch': 'Language',

  'auth.login': 'Log in',
  'auth.register': 'Register',
  'auth.email': 'Email',
  'auth.password': 'Password',
  'auth.password_hint': 'Password (8+ characters)',
  'auth.create_account': 'Create account',
  'auth.have_account': 'Have one?',
  'auth.no_account': 'No account?',

  'markets.title': 'Markets',
  'markets.col.market': 'Market',
  'markets.col.last': 'Last',
  'markets.col.change': '24h change',
  'markets.col.volume': '24h volume',
  'markets.col.trades': 'Trades',

  'ticker.change': '24h change',
  'ticker.high': '24h high',
  'ticker.low': '24h low',
  'ticker.volume': '24h volume',
  'ticker.fees': 'Fees',
  'ticker.fee_detail': 'maker {maker} bps / taker {taker} bps',

  'trade.public_stream': 'public stream {status}',
  'trade.loading_market': 'Loading market…',
  'trade.place_order': 'Place order',
  'trade.buy': 'Buy {asset}',
  'trade.sell': 'Sell {asset}',
  'trade.limit': 'Limit',
  'trade.market': 'Market',
  'trade.price_label': 'Price ({quote}, tick {tick})',
  'trade.spend_label': 'Spend ({quote})',
  'trade.qty_label': 'Quantity ({base}, step {step})',
  'trade.result': '{status} · filled {filled}',
  'trade.rejected': 'Rejected: {reason}',
  'trade.market_status': 'Market {status}',
  'trade.validate.quote_qty': 'Enter the quote amount to spend',
  'trade.validate.min_notional': 'Below the minimum notional {min} {quote}',
  'trade.validate.qty': 'Enter a quantity',
  'trade.validate.qty_step': 'Quantity must be a multiple of {step}',
  'trade.validate.max_qty': 'Above the maximum quantity {max}',
  'trade.validate.price': 'Enter a price',
  'trade.validate.price_tick': 'Price must be a multiple of {tick}',

  'book.title': 'Order book',
  'book.seq': 'seq {seq}',
  'book.resyncing': 'resyncing…',
  'book.connecting': 'connecting…',
  'book.spread': 'spread {spread}',

  'trades.title': 'Recent trades',
  'trades.empty': 'No trades yet',

  'orders.title': 'Open orders',
  'orders.col.filled': 'Filled',
  'orders.market_price': 'market',
  'orders.quote_qty': '{qty} quote',
  'orders.cancel': 'Cancel',
  'orders.empty': 'No open orders',

  'fills.title': 'My fills',
  'fills.col.fee': 'Fee',
  'fills.col.role': 'Role',
  'fills.col.order': 'Order',
  'fills.empty': 'No fills yet',

  'balances.title': 'Balances',
  'balances.col.available': 'Available',
  'balances.col.hold': 'On hold',
  'balances.col.total': 'Total',

  'wallet.account_id': 'Account ID',
  'wallet.deposits': 'Deposits',
  'wallet.withdrawals': 'Withdrawals',
  'wallet.col.confirmations': 'Confirmations',
  'wallet.col.to': 'To',
  'wallet.deposits_empty': 'No deposits yet',
  'wallet.withdrawals_empty': 'No withdrawals yet',
  'wallet.deposit': 'Deposit',
  'wallet.deposits_disabled': '(deposits disabled)',
  'wallet.send_to': 'Send {asset} on chain {chain} to',
  'wallet.deposit_min': 'Minimum {min} {asset}; credited after {confirmations} confirmations.',
  'wallet.withdraw': 'Withdraw',
  'wallet.withdrawals_disabled': '(withdrawals disabled)',
  'wallet.amount': 'Amount',
  'wallet.amount_label': 'Amount (min {min}, fee {fee} {asset})',
  'wallet.to_address': 'To address',
  'wallet.request_withdrawal': 'Request withdrawal',
  'wallet.withdrawal_created': 'Withdrawal {id} {status}',

  'error.network': 'Could not reach the server',
  'error.with_detail': '{detail}',
  'error.http.400': 'The request was not understood',
  'error.http.401': 'Please log in again',
  'error.http.403': 'Not allowed',
  'error.http.404': 'Not found',
  'error.http.409': 'Conflicts with an earlier request',
  'error.http.422': 'The request was refused',
  'error.http.429': 'Too many requests; slow down',
  'error.http.503': 'The service is temporarily unavailable',
  'error.http.other': 'Something went wrong',
  'error.detail.email_registered': 'This email is already registered',
  'error.detail.bad_credentials': 'Invalid email or password',
  'error.detail.frozen': 'This account is frozen',
  'error.detail.engine_unavailable': 'The trading engine is unavailable; try again shortly',
  'error.detail.no_cancels': 'This market no longer accepts cancels',
  'error.detail.no_deposit_address': 'No deposit address is available; try again shortly',
  'error.detail.deposits_disabled': 'Deposits are not enabled on this deployment',
  'error.detail.withdrawals_disabled': 'Withdrawals are not enabled on this deployment',
  'error.detail.auth_required': 'Please log in first',
  'error.detail.session_expired': 'The session has expired; log in again',
} as const

export type Key = keyof typeof en

export const zhTW: Record<Key, string> = {
  'common.loading': '載入中…',
  'col.time': '時間',
  'col.asset': '資產',
  'col.price': '價格',
  'col.qty': '數量',
  'col.side': '方向',
  'col.status': '狀態',
  'col.amount': '金額',
  'col.tx': '交易哈希',

  'nav.markets': '市場',
  'nav.wallet': '錢包',
  'nav.login': '登入',
  'nav.register': '註冊',
  'nav.logout': '登出',
  'nav.private_stream': '私有推播:{status}',
  'lang.switch': '語言',

  'auth.login': '登入',
  'auth.register': '註冊',
  'auth.email': '電子郵件',
  'auth.password': '密碼',
  'auth.password_hint': '密碼(至少 8 個字元)',
  'auth.create_account': '建立帳號',
  'auth.have_account': '已經有帳號?',
  'auth.no_account': '還沒有帳號?',

  'markets.title': '市場',
  'markets.col.market': '市場',
  'markets.col.last': '最新價',
  'markets.col.change': '24 小時漲跌',
  'markets.col.volume': '24 小時成交量',
  'markets.col.trades': '成交筆數',

  'ticker.change': '24 小時漲跌',
  'ticker.high': '24 小時最高',
  'ticker.low': '24 小時最低',
  'ticker.volume': '24 小時成交量',
  'ticker.fees': '手續費',
  'ticker.fee_detail': '掛單方 {maker} bps / 吃單方 {taker} bps',

  'trade.public_stream': '公開推播 {status}',
  'trade.loading_market': '載入市場中…',
  'trade.place_order': '下單',
  'trade.buy': '買入 {asset}',
  'trade.sell': '賣出 {asset}',
  'trade.limit': '限價',
  'trade.market': '市價',
  'trade.price_label': '價格({quote},最小跳動 {tick})',
  'trade.spend_label': '花費({quote})',
  'trade.qty_label': '數量({base},最小單位 {step})',
  'trade.result': '{status} · 已成交 {filled}',
  'trade.rejected': '已拒絕:{reason}',
  'trade.market_status': '市場{status}',
  'trade.validate.quote_qty': '請輸入要花費的金額',
  'trade.validate.min_notional': '低於最小成交金額 {min} {quote}',
  'trade.validate.qty': '請輸入數量',
  'trade.validate.qty_step': '數量必須是 {step} 的整數倍',
  'trade.validate.max_qty': '超過最大數量 {max}',
  'trade.validate.price': '請輸入價格',
  'trade.validate.price_tick': '價格必須是 {tick} 的整數倍',

  'book.title': '訂單簿',
  'book.seq': '序號 {seq}',
  'book.resyncing': '重新同步中…',
  'book.connecting': '連線中…',
  'book.spread': '價差 {spread}',

  'trades.title': '最近成交',
  'trades.empty': '尚無成交',

  'orders.title': '未成交訂單',
  'orders.col.filled': '已成交',
  'orders.market_price': '市價',
  'orders.quote_qty': '{qty} 計價幣',
  'orders.cancel': '撤單',
  'orders.empty': '沒有未成交的訂單',

  'fills.title': '我的成交',
  'fills.col.fee': '手續費',
  'fills.col.role': '角色',
  'fills.col.order': '訂單',
  'fills.empty': '尚無成交',

  'balances.title': '餘額',
  'balances.col.available': '可用',
  'balances.col.hold': '凍結',
  'balances.col.total': '總計',

  'wallet.account_id': '帳戶 ID',
  'wallet.deposits': '充值紀錄',
  'wallet.withdrawals': '提現紀錄',
  'wallet.col.confirmations': '確認數',
  'wallet.col.to': '收款地址',
  'wallet.deposits_empty': '尚無充值紀錄',
  'wallet.withdrawals_empty': '尚無提現紀錄',
  'wallet.deposit': '充值',
  'wallet.deposits_disabled': '(暫停充值)',
  'wallet.send_to': '在鏈 {chain} 上把 {asset} 轉到',
  'wallet.deposit_min': '最少 {min} {asset};{confirmations} 個區塊確認後入帳。',
  'wallet.withdraw': '提現',
  'wallet.withdrawals_disabled': '(暫停提現)',
  'wallet.amount': '金額',
  'wallet.amount_label': '金額(最少 {min},手續費 {fee} {asset})',
  'wallet.to_address': '收款地址',
  'wallet.request_withdrawal': '送出提現申請',
  'wallet.withdrawal_created': '提現 {id} {status}',

  'error.network': '連不上伺服器',
  'error.with_detail': '{generic}({detail})',
  'error.http.400': '伺服器看不懂這個請求',
  'error.http.401': '請重新登入',
  'error.http.403': '沒有權限',
  'error.http.404': '找不到',
  'error.http.409': '與先前的請求衝突',
  'error.http.422': '請求被拒絕',
  'error.http.429': '請求太頻繁,請稍後再試',
  'error.http.503': '服務暫時無法使用',
  'error.http.other': '發生了未預期的錯誤',
  'error.detail.email_registered': '這個電子郵件已經註冊過了',
  'error.detail.bad_credentials': '電子郵件或密碼錯誤',
  'error.detail.frozen': '這個帳戶已被凍結',
  'error.detail.engine_unavailable': '交易引擎暫時無法使用,請稍後再試',
  'error.detail.no_cancels': '這個市場目前不接受撤單',
  'error.detail.no_deposit_address': '暫時沒有可用的充值地址,請稍後再試',
  'error.detail.deposits_disabled': '這個部署沒有開放充值',
  'error.detail.withdrawals_disabled': '這個部署沒有開放提現',
  'error.detail.auth_required': '請先登入',
  'error.detail.session_expired': '登入已過期,請重新登入',
}

export const messages: Record<Locale, Record<Key, string>> = { en, 'zh-TW': zhTW }

export type Params = Record<string, string | number>

// Translate is what components receive from useLocale(): the message for
// `key` in the current locale with its placeholders filled in.
export type Translate = (key: Key, params?: Params) => string

export function translate(locale: Locale, key: Key, params?: Params): string {
  const text = messages[locale][key]
  if (!params) return text
  return text.replace(/\{(\w+)\}/g, (whole, name: string) => {
    const v = params[name]
    return v === undefined ? whole : String(v)
  })
}

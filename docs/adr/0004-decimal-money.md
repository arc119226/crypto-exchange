# ADR-0004:金額以 Decimal + 每資產 scale 表示,禁止浮點

- 狀態:已採納(Accepted)
- 日期:2026-09-05
- 相關:[ADR-0000](0000-interview-decisions.md) 第 6 輪;`docs/plan-v1.0.md` §6.5;審查 finding F13 / L02;`docs/domain.md` §7

## 背景

v0.1 只寫了「Postgres decimal」。Go 端若用 `int64`,10 ETH 的 wei(1e19)即溢位;`float64` 破壞守恆;撮合、帳本、錢包各自選型會在 DTO 邊界截斷精度。手續費與 quote 金額的捨入方向不統一會破壞「每資產借貸合計為零」。

## 選項

1. **最小單位整數 `big.Int`**(審查 L02 建議):精確、貼近鏈上;但每個金額都要帶 scale 才能顯示與相乘,`big.Int` 是可變指標型別,容易共享狀態。
2. **`int64` 最小單位**:ETH wei 溢位,排除。
3. **`shopspring/decimal` 封裝成不可變 value type**:可讀、不溢位、直接對應 NUMERIC;運算回傳新值。

## 決定

採選項 3,實作於 `internal/money`(Phase 0 完成,覆蓋率 98.7%,含 `rapid` 屬性測試):

- `money.Amount`:不可變 value type;`ParseAmount` 只接受 `^-?[0-9]+(\.[0-9]+)?$`、整數位 ≤ 18、小數位 ≤ 18(對應 `NUMERIC(36,18)`);`Add/Sub/Mul/Neg/Abs/Cmp`、`RoundUp`(手續費,對交易所有利)、`RoundDown`(市價買換算)、`Truncate`、`IsMultipleOf`(tick / step)。
- JSON 只接受字串(`MarshalJSON` 輸出字串,`UnmarshalJSON` 拒絕 JSON 數字);OpenAPI `Amount` schema `type: string` + pattern + `x-go-type: money.Amount`,產生碼直接用 `money.Amount`。
- Postgres 全部金額 `NUMERIC(36,18)`;`pgtype.Numeric ↔ money.Amount` 的 helper 放 `internal/platform/pg`(`money` 不 import pgx),必須處理 `Exp ≠ 0`,整合測試證明 2^256−1 被 Postgres 以 `22003` 拒絕。
- lint:forbidigo 在 `internal/(money|matching|ledger|trading|chain|marketdata|policy)/` 禁止 `float32/64`、`strconv.ParseFloat/FormatFloat`、`decimal.NewFromFloat`、`.InexactFloat64`(Phase 0 以故意違規證明會擋)。
- registry:`assets.scale`、`markets.price_tick / qty_step / min_notional`;建市場時驗證 `scale(qty_step) + scale(price_tick) ≤ quote.scale`,保證 `price × qty` 不需捨入(`domain.md` §7 證明)。
- 捨入方向:手續費 `ceil` 到資產 scale(對交易所有利);市價買 `floor` 到 `qty_step`(對用戶保守);API 輸入不是 tick / step 整數倍一律拒單,不自動截斷。

## 後果

- 正面:守恆不變量可以嚴格相等地測;wire format、DB、Go 三層都是十進位字串語意;不需要 `exchange:rounding` 科目。
- 負面:`decimal` 比整數慢;撮合熱路徑若成為瓶頸,Phase 1 benchmark 後再考慮在 `matching` 內部改用定點整數(對外仍是 `money.Amount`)。
- 鏈上邊界:`wei ↔ Amount` 只在 `internal/chain/evm` 一處轉換並有 round-trip 測試(Phase 4)。

# 打新账本接口速查

- 接口根地址：{{baseUrl}}
- 发现文件：{{discoveryPath}}
- 固定指针：`~/.ipo-book/api.json`（端口变化时以它为准）
- 账本数据：{{dataPath}}

## 通用约定

- 鉴权：`Authorization: Bearer <token>`，或 `X-API-Token: <token>`。令牌在发现文件的
  `token` 字段里。
- 端口不固定：`47123` 是默认值，被占用时改用随机端口。找接口按这个顺序读文件，
  并对 `baseUrl` 探一次 `/api/health`：`$IPO_BOOK_API` → `~/.ipo-book/api.json` →
  数据目录里的 `api.json`。
- 只接受本机访问：监听 `127.0.0.1`；带 `Origin` 的请求（浏览器发起）一律拒绝。
- 请求与响应都是 JSON，UTF-8，中文不转义。
- 成功：2xx + 资源对象。失败：4xx/5xx + `{"error": "中文说明", "code": "分类"}`。
- 金额一律整数「分」，费率一律整数 ppm（百万分之一），日期 `YYYY-MM-DD`。

## 接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | /api | 接口清单、版本、skill 位置 |
| GET | /api/health | 记录数、账户数、上次同步时间 |
| GET | /api/snapshot | 一次取回新股、账户、记录、汇总与来源信息 |
| GET | /api/summary | 收益汇总 |
| GET | /api/stats | 收益统计，可带 `year`、`month`、`brokerId`，附逐条明细与按年、按月拆分 |
| GET | /api/issues | 新股列表，可带 `scope`、`q`、`limit` |
| POST | /api/issues/refresh | 从北交所同步新股，body 可给 `{"scope": "all"}` |
| GET | /api/brokers | 券商账户列表 |
| POST | /api/brokers | 新增券商账户 |
| PATCH | /api/brokers/{id} | 修改券商账户 |
| DELETE | /api/brokers/{id} | 删除券商账户（账户下有记录时拒绝） |
| GET | /api/entries | 记录列表，可带 `fxCode`、`brokerId`、`status` |
| POST | /api/entries | 新增记录 |
| GET | /api/entries/{id} | 单条记录 |
| PATCH | /api/entries/{id} | 局部修改记录 |
| DELETE | /api/entries/{id} | 删除记录 |
| POST | /api/entries/{id}/sales | 追加一笔卖出 |
| PATCH | /api/entries/{id}/sales/{saleId} | 改某一笔卖出（整笔替换） |
| DELETE | /api/entries/{id}/sales/{saleId} | 删某一笔卖出 |
| GET | /api/backup | 导出备份 JSON |
| POST | /api/backup/restore | 用备份替换账本，须带 `confirm=replace` |
| GET | /api/skill | skill 目录与文件清单 |
| POST | /api/skill/export | 导出 skill 包，body 可给 `{"dir": "..."}` |

## 新股（issue）

| 字段 | 含义 |
| --- | --- |
| `fxCode` | 申购代码，920xxx |
| `stockCode` | 上市后的证券代码 |
| `name` | 名称 |
| `priceFen` | 发行价（分），0 表示未定价 |
| `issueShares` | 发行数量（股） |
| `peHundredths` | 市盈率 ×100 |
| `purchaseDate` | 申购日 |
| `resultDate` | 中签公布日 |
| `listingDate` | 上市日，空表示还没上市 |

`GET /api/issues?scope=` 的取值：`issuing` 未上市或上市日未到，`done` 已上市，
`all` 全部。`q` 匹配名称、申购代码、证券代码。返回：

```json
{ "count": 2, "scope": "issuing", "issues": [ { "fxCode": "920306", "name": "天元重工", "priceFen": 448, "purchaseDate": "2026-10-20" } ] }
```

## 券商账户（broker）

| 提交字段 | 说明 |
| --- | --- |
| `name` | 券商名称，必填，如 华泰证券 |
| `account` | 账户别名，留空为「默认账户」 |
| `commissionWan` | 卖出佣金率，万分之，如 `2.5` |
| `minCommission` | 单笔最低佣金，元，如 `5` |
| `annualRatePct` | 资金年化利率，百分数，如 `3.85` |
| `note` | 备注 |

也接受整数写法：`commissionPpm`、`minCommissionFen`、`annualRatePpm`。
同一「券商 + 账户」只能有一条，重名会返回 `409`。

```sh
curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"华泰证券","account":"主账户","commissionWan":"2.5","minCommission":"5","annualRatePct":"3.85"}' \
  "$BASE/api/brokers"
```

## 记录（entry）

| 提交字段 | 说明 |
| --- | --- |
| `brokerId` | 券商账户 id，必填 |
| `fxCode` | 申购代码，必填 |
| `stockCode` / `name` | 证券代码与名称，建议从新股列表带上 |
| `issuePrice` | 发行价，元 |
| `purchaseDate` | 申购日，留空时取新股缓存里的日期 |
| `applyShares` | 申购数量（股） |
| `allottedShares` | 中签数量（股）。留空或 0 表示还没出结果或未中签 |
| `allotPrice` | 中签缴款价，元，默认等于发行价 |
| `freezeDate` / `refundDate` | 冻款日 / 退款日，用于算资金占用天数 |
| `capitalCost` | 实际资金成本，元；不填按账户年化利率与天数估算 |
| `sales` | 卖出明细数组，见下节；整组提交，没列进去的会被删掉 |
| `otherFee` | 其他费用，元 |
| `note` | 备注 |

老的单笔写法仍然接受：`sellPrice`、`sellAmount`、`soldShares`、`sellDate`、`sellFee`，
会在内部折成一笔明细。它和 `sales` 同时出现会返回 400，不去猜调用方本意。

响应里另有派生字段（只读，提交会被拒绝）：

| 字段 | 含义 |
| --- | --- |
| `status` | 待公布 / 已申购 / 已中签 / 未中签 / 部分卖出 / 已卖出 |
| `applyFundsFen` | 申购占用资金（分） |
| `freezeDays` | 资金占用天数，`freezeEstimated` 表示按默认 2 天估算 |
| `capitalCostFen` | 采用的资金成本，`capitalCostEstimated` 表示是估算的 |
| `soldShares` / `sellAmountFen` | 卖出股数与成交金额，由明细汇总 |
| `sellPriceFen` | 按成交金额与股数回算的均价 |
| `sellDate` | 最后一笔卖出的日期 |
| `saleCount` | 卖出笔数 |
| `sellFeeFen` | 采用的卖出费用，`feeEstimated` 表示有笔次是估算的 |
| `totalCostFen` | 费用与资金成本合计 |
| `realizedFen` | 已实现净收益 |
| `holdingShares` / `holdingCostFen` | 仍持有的股数与成本 |
| `returnBps` | 已实现收益率，万分之一 |

## 卖出明细（sale）

一只票分几次卖就有几笔，每笔单独算费用（最低佣金按笔生效）。

| 字段 | 说明 |
| --- | --- |
| `date` | 成交日，`YYYY-MM-DD` |
| `shares` | 成交股数，必须大于 0 |
| `price` | 成交价，元；与 `amount` 给一个即可 |
| `amount` | 成交金额，元。填了就以它为准，`price` 只用于展示 |
| `fee` | 这笔的费用，元；不填按账户佣金率与最低佣金估算 |
| `note` | 备注 |
| `id` | 只读，库分配的明细 ID，改一笔删一笔都靠它；重复保存不会变 |
| `priceFen` / `amountFen` / `feeFen` | 只读，对应的整数分 |

```sh
# 追加一笔（只给成交价，费用按佣金率算）
curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"date":"2026-10-30","shares":500,"price":"6.20"}' "$BASE/api/entries/12/sales"

# 改这一笔（整笔替换，date/shares/price 要一起给）
curl -sS -X PATCH -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"date":"2026-10-30","shares":500,"price":"6.10"}' "$BASE/api/entries/12/sales/7"

# 删这一笔
curl -sS -X DELETE -H "Authorization: Bearer $TOKEN" "$BASE/api/entries/12/sales/7"
```

三个接口都返回算好的整条记录（含 `sales` 数组与派生字段），不用再查一次。
单条记录最多 200 笔卖出；合计股数不能超过中签数量，否则 400。
`PATCH /api/entries/{id}` 里的 `sales` 是整组替换，适合一次补录整段交割单。

## 汇总（summary）

| 字段 | 含义 |
| --- | --- |
| `entryCount` | 记录数 |
| `appliedIssues` / `allottedIssues` | 申购只数 / 中签只数 |
| `applyFundsFen` | 申购资金合计 |
| `capitalCostFen` | 资金成本合计，`estimatedCapitalFen` 是其中估算部分 |
| `totalCostFen` | 全部支出 |
| `realizedFen` | 净收益 |
| `holdingShares` / `holdingCostFen` | 持仓 |
| `returnBps` | 收益率，万分之一 |
| `annualizedBps` | 资金年化回报，万分之一 |
| `winRateBps` | 中签率，万分之一 |
| `byAccount` | 按账户分组：`brokerId`、`account`、`entryCount`、`allottedIssues`、`applyFundsFen`、`saleCount`、`sellAmountFen`、`capitalCostFen`、`realizedFen`、`holdingShares`、`returnBps` |

## 收益统计（stats）

`GET /api/stats?year=2026&month=10&brokerId=1`，三个参数都可省略，省略即不筛选。

| 参数 | 含义 |
| --- | --- |
| `year` | 按申购日筛选年份，1990–2200 |
| `month` | 按申购日筛选月份，1–12；可以只给月份，跨年取所有该月 |
| `brokerId` | 只看某个券商账户 |

```json
{
  "filter": {"year": 2026, "month": 10, "brokerId": 0},
  "summary": { "entryCount": 3, "realizedFen": 516026, "returnBps": 22389, "...": "同 /api/summary" },
  "details": [{"entryId": 12, "fxCode": "920306", "name": "天元重工", "account": "平安证券 · 尾号4169",
               "purchaseDate": "2026-10-20", "allottedShares": 300, "applyFundsFen": 8149120,
               "saleCount": 2, "sellAmountFen": 746500, "capitalCostFen": 0,
               "realizedFen": 516026, "returnBps": 22389, "holdingShares": 300, "status": "已中签"}],
  "byYear":  [{"key": "2026", "label": "2026 年", "entryCount": 3, "realizedFen": 516026, "...": ""}],
  "byMonth": [{"key": "2026-10", "label": "2026 年 10 月", "entryCount": 3, "...": ""}],
  "years": [2026, 2025, 2024],
  "accounts": [{"id": 1, "label": "平安证券 · 尾号4169"}]
}
```

- 时间按**申购日**归属：一个批次从申购到卖出可能跨月，整批算在申购当月，各月、各年相加
  正好等于总计。没填申购日的记录 `key` 为空、`undated` 为真，单独一组。
- `byMonth` **只受 `brokerId` 影响**：年份、月份都不参与。界面里点某个月会把 `year`、`month`
  都设上，若 `byMonth` 也跟着收窄，别的年份的月份就消失了。想要一个月的数据读 `summary`；
  想要某一年的逐月拆分，读 `byMonth` 里 `key` 以该年份开头的几组。
- `details` 是筛选后记录的逐条明细，按申购日从新到旧、同日按代码倒序，没填申购日的垫底
  （与「我的打新」列表同一顺序），**不受分组影响**。逐行相加等于 `summary`，字段与分组同口径，
  所以「这个月赚在哪几只票上」直接读它，不要自己按 `purchaseDate` 再分一次组。
- `years`、`accounts` 列的是账本全集，不受当前筛选影响，可以直接拿去生成下拉框。
- 每个分组与 `summary` 同字段、同口径：金额是分，收益率是万分之一。

## 错误码

| HTTP | code | 含义 |
| --- | --- | --- |
| 400 | `invalid` | 字段缺失、格式不对、数值为负、未知字段 |
| 400 | `confirm_required` | 恢复备份缺少 `confirm=replace` |
| 401 | `unauthorized` | 令牌缺失或不对 |
| 403 | `forbidden` | 非本机访问，或带 `Origin` 的浏览器请求 |
| 404 | `not_found` | 记录或账户不存在，或路径写错 |
| 409 | `conflict` | 同一账户重复录入同一只新股，或账户重名 |
| 500 | `internal` | 程序内部错误，把 `error` 原文告诉用户 |

## 示例：一次完整的记账

```sh
BASE=...; TOKEN=...   # 从发现文件取
# 下面的 curl 都假定没有走 HTTP 代理；有代理时加 --noproxy '*'。

# 1. 建账户
curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"华泰证券","account":"主账户","commissionWan":"2.5","annualRatePct":"3.85"}' \
  "$BASE/api/brokers"

# 2. 记申购（返回里的 id 假设为 12）
curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"brokerId":1,"fxCode":"920306","name":"天元重工","issuePrice":"4.48","applyShares":5000}' \
  "$BASE/api/entries"

# 3. 中签 1000 股
curl -sS -X PATCH -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"allottedShares":1000}' "$BASE/api/entries/12"

# 4. 卖出：先卖一笔 400 股，再卖一笔 600 股（费用都留空，按佣金率算）
curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"date":"2026-10-28","shares":400,"price":"6.20"}' "$BASE/api/entries/12/sales"
curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"date":"2026-10-30","shares":600,"amount":"3720.00"}' "$BASE/api/entries/12/sales"

# 5. 看汇总
curl -sS -H "Authorization: Bearer $TOKEN" "$BASE/api/summary"
```

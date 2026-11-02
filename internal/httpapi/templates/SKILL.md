---
name: ipo-book
description: 读写本机「打新账本」桌面应用里的北交所打新记录：查可打新股、记一笔申购、更新中签与卖出结果、查看收益统计。当用户提到打新账本、新股申购、中签、卖出新股、北交所打新或打新收益时使用。
---

# 打新账本

「打新账本」是跑在本机的桌面应用，它把账本开成了只在本机可访问的 HTTP 接口。
通过下面的接口读写打新记录，效果与用户在界面里点按完全一致。

- 接口根地址：{{baseUrl}}
- 账本数据：{{dataPath}}
- 本 skill 目录：{{skillDir}}
- 导出时间：{{exportedAt}}（应用版本 {{version}}）

## 连接

**端口不固定**：`47123` 只是默认值，端口被占用时应用会改用随机端口。不要硬编码
端口，也不要挨个端口试，按顺序找下面这几个文件：

| 顺序 | 位置 | 说明 |
| --- | --- | --- |
| 1 | `$IPO_BOOK_API` | 环境变量指定的文件，优先级最高 |
| 2 | `~/.ipo-book/api.json` | **固定指针**，跨平台都在这里，应用每次启动都重写 |
| 3 | 数据目录里的 `{{discoveryName}}` | 即 `{{discoveryPath}}`，应用直接写在这里 |

指针文件里除了地址与令牌，还有一个 `discovery` 字段指向真正的发现文件。读到的
`baseUrl` 先探一次 `GET /api/health`，只有真能应答的才算数——进程崩掉留下的旧
文件会指向一个没人监听的端口。

```sh
for f in "${IPO_BOOK_API:-}" "$HOME/.ipo-book/api.json" "{{discoveryPath}}"; do
  [ -f "$f" ] || continue
  BASE=$(sed -n 's/.*"baseUrl": *"\([^"]*\)".*/\1/p' "$f")
  TOKEN=$(sed -n 's/.*"token": *"\([^"]*\)".*/\1/p' "$f")
  curl -sf --noproxy '*' --max-time 3 -H "Authorization: Bearer $TOKEN" \
    "$BASE/api/health" >/dev/null 2>&1 && break
done
```

`--noproxy '*'` 别省：接口只在本机，机器上如果设了 HTTP 代理，不加这个连
127.0.0.1 也会被转发出去，只会得到连接失败。

- 三个文件都不存在或都探不通，说明应用没在运行。请用户先打开「打新账本」，
  **不要自己启动 .app**：桌面应用要用户自己点开，强行拉起只会撞上单实例锁。
- 令牌等同于账本的写权限，不要写进仓库、日志或聊天记录。

包里的 `scripts/ipobook.sh` 已经按上面的顺序找好了，可以直接用：

```sh
./scripts/ipobook.sh doctor     # 看不到接口时先跑这个，它打印找到的文件与地址
./scripts/ipobook.sh health
./scripts/ipobook.sh get "/api/issues?scope=issuing&limit=10"
```

`IPO_BOOK_API` 环境变量可以指向另一个 `api.json`。

## 单位（最容易出错的地方）

| 类型 | 读取时看这些字段 | 提交时怎么写 |
| --- | --- | --- |
| 金额 | 以 `Fen` 结尾，单位「分」，如 `issuePriceFen` | 用不带后缀的字段按「元」，如 `"issuePrice": "4.48"` |
| 股数 | `applyShares`、`allottedShares`、`soldShares` | 整数，单位股 |
| 卖出 | 每笔在 `sales[]` 里，`sellAmountFen`/`soldShares`/`sellPriceFen`/`saleCount` 是明细汇总 | `sales: [{"date","shares","price"或"amount","fee"}]`，金额按元 |
| 佣金率 | `commissionPpm`，百万分之一 | `commissionWan`，万分之，如 `2.5` |
| 资金年化 | `annualRatePpm`，百万分之一 | `annualRatePct`，百分数，如 `3.85` |
| 日期 | `purchaseDate`、`resultDate`、`sellDate` | `YYYY-MM-DD` |

读取一律用整数分，不要自己把元四舍五入。写入用「元」更不容易错，服务端按十进制
精确换算，全程不经过浮点。

## 常见流程

### 看最近能打的新股

```sh
./scripts/ipobook.sh get "/api/issues?scope=issuing&limit=10"
```

`scope`：`issuing` 是还没上市或上市日未到的，`done` 是已上市，`all` 是全部。
`fxCode` 是申购代码（920xxx），`priceFen` 是发行价（分），`purchaseDate` 是申购日。

### 记一笔申购

先看有哪些券商账户，`brokerId` 必填：

```sh
./scripts/ipobook.sh get /api/brokers
```

```sh
./scripts/ipobook.sh post /api/entries '{
  "brokerId": 1,
  "fxCode": "920306",
  "name": "天元重工",
  "issuePrice": "4.48",
  "purchaseDate": "2026-10-20",
  "applyShares": 1000
}'
```

返回里的 `id` 要记住，之后更新都用它。

### 出结果后更新中签

```sh
./scripts/ipobook.sh patch /api/entries/12 '{"allottedShares": 500}'
```

PATCH 只改提交的字段，其余保持原样。没中签就写 `"allottedShares": 0`。

### 卖出后更新

一只票常常分几次卖，账本按**笔**记：每笔有自己的 `id`，各有成交日、股数、成交价与费用。
刚卖掉一笔就直接追加，不用先把整条记录读出来再写回：

```sh
./scripts/ipobook.sh post /api/entries/12/sales '{
  "date": "2026-10-30",
  "shares": 500,
  "price": "6.20"
}'
```

- `price`（成交价，元）与 `amount`（成交金额，元）给一个就行。交割单上有金额就传 `amount`，
  多笔成交的均价常除不尽；只给 `price` 时金额按「股数 × 成交价」算。
- `fee` 不填就按账户的佣金率和最低佣金**逐笔**估算，结果里标 `feeEstimated: true`。
  用户说了实际费用就显式传 `fee`，不要让程序猜。
- 记错了改某一笔：`PATCH /api/entries/12/sales/{saleId}`，整笔替换，要把这一笔的
  `date`/`shares`/`price` 一起传。
- 删某一笔：`DELETE /api/entries/12/sales/{saleId}`。

一次要写好几笔（补录历史交割单），用整组提交更省事：`PATCH /api/entries/12`，`sales`
数组就是这只票的**全部**卖出，没列进去的会被删掉。

```sh
./scripts/ipobook.sh patch /api/entries/12 '{"sales":[
  {"date":"2026-10-28","shares":400,"price":"6.20"},
  {"date":"2026-10-30","shares":600,"amount":"3720.00","fee":"2.50"}
]}'
```

写入后返回整条记录，`soldShares` 是卖出股数合计、`sellAmountFen` 是成交金额合计、
`sellPriceFen` 是均价、`saleCount` 是笔数，都是明细算出来的，不要自己再算一遍。

### 看收益

```sh
./scripts/ipobook.sh get /api/summary
```

`realizedFen` 是净收益（已扣资金成本与费用），`returnBps` 是收益率（万分之一，
6264 表示 62.64%），`holdingShares` 是还持有的股数。

要按年、按月或按券商看，用 `/api/stats`：

```sh
# 今年的
./scripts/ipobook.sh get "/api/stats?year=2026"
# 2026 年 10 月、平安证券那个账户
./scripts/ipobook.sh get "/api/stats?year=2026&month=10&brokerId=1"
# 只看券商账户 2 的历史
./scripts/ipobook.sh get "/api/stats?brokerId=2"
```

返回 `summary`（同 `/api/summary`）、`details`（逐条明细）、`byYear`、`byMonth`、`years`、`accounts`。
时间按**申购日**归属，整个批次算在申购当月，各月相加正好等于总计；`byMonth` 只跟
`brokerId` 走，年份与月份都不影响它（界面按月翻账本就靠这一份全量列表），所以它比
`summary` 长得多。用户问「这个月赚了多少」，就用 `?year=YYYY&month=M` 读 `summary.realizedFen`；
问「赚在哪几只票上」，读同一份返回里的 `details`（一行一条记录，逐行相加等于 `summary`），
不要自己再按日期分一次组。

## 规则与边界

1. **不要编数据**。发行价、中签数量、卖出价、费用都要有来源（新股列表、用户口述、
   券商 App 截图）。拿不准就问，不要猜一个填进去。
2. **先查再写**。同一券商账户下同一只新股只允许一条记录，重复提交会返回 `409`。
   要更新就 `PATCH /api/entries/{id}`。
3. **删除要确认**。`DELETE` 会真的删掉记录，只有用户明确要求时才调用。
4. **整体恢复**（`POST /api/backup/restore?confirm=replace`）会替换全部账本，
   除非用户明确要求恢复，否则不要碰。
5. **写完告诉用户改了什么**：哪只票、哪个账户、哪个字段从多少变成多少。
6. 非 2xx 时 `error` 是给用户看的中文说明，`code` 是分类：`invalid` 填错、
   `conflict` 重复、`not_found` 不存在、`unauthorized` 令牌不对、`confirm_required` 缺少确认。
   照 `error` 的说法修，不要重复重试同一个请求。

## 更多

- `references/api.md`：全部接口、字段、错误码与示例。
- `GET /api`：运行中的接口清单，路由改了它以它为准。
- `GET /api/health`：记录数、账户数与上次同步新股的时间。

# 虚拟运营商离线批价引擎（Go 重写）

面向海量语音/短信/流量话单的离线批价系统。支持四种话单格式解码、复杂资费规则、
跨月/跨套餐用量切分、去重合并、分片并行与断点续跑、资费变更重算差额，并由一套
**完全独立的 Python 应收（oracle）** 逐用户核对金额，作为正确性裁判。

## 能力对照（需求 → 实现）

| # | 需求 | 实现 |
|---|------|------|
| 1 | 定长/分隔符/ASN.1 二进制解码 | `internal/codec`：fixed(120列)、delimited(`\|`)、ASN.1 DER(手写 TLV 互通)、附加 lpbin 长度前缀 |
| 2 | 免费额度/阶梯价/夜间/漫游可配置 | `tariffs/*.json` + `internal/pipeline/rating.go`，基点制系数、整数厘计价 |
| 3 | 跨月、跨套餐变更按生效时间切分 | `tariff.Catalog.SplitBoundaries` + 片段按秒占比整数分摊 |
| 4 | 重复去重、长通话拆段合并 | record_id 去重；(user_id, call_ref) 分组 E5 校验后合并 |
| 5 | 按用户分片并行、失败续跑 | FNV-1a 分片，分片 checkpoint 原子写，DONE 跳过、失败分片重跑 |
| 6 | 资费变更按时间段重算、输出用户差额 | `engine recompute` + `internal/reconcile.DiffTasks`，独立任务目录 |
| 7 | 批价与账务汇总对平、不平列明细 | 引擎内两级对平 + oracle 逐用户×月对账，差异输出 `oracle_reconcile.json` |
| 8 | 处理速度与阶段耗时可视化 | 运营页面阶段条形图、分片状态网格、CLI `show` |

## 目录结构

```
cmd/engine/           CLI 与 HTTP 服务入口
internal/
  codec/              四种格式编解码 + FNV 分片
  model/              领域对象
  store/              JSONL 流式存储、原子 checkpoint
  tariff/             资费/订阅目录、夜间窗口、切分边界
  pipeline/           接入、去重合并、切分批价、分片汇总（核心规则）
  task/               任务编排、并行、续跑、阶段统计
  reconcile/          内部对平、oracle 对账、重算差额
  web/                运营页面（embed 单文件 SPA，无外部依赖）
scripts/
  gen_config.py       资费（几百种）+ 订阅生成
  gen_cdrs.py         千万级话单生成（异常注入）
  oracle.py           独立应收（不依赖 Go 代码）
  der.py / domain.py  Python 独立 DER 编解码与域规则
  e2e_verify.sh       一键端到端验收
docs/SPEC.md          确定性规则说明书（Go 与 Python 共同遵守）
```

## 快速开始

```bash
# 一键验收（小数据，约 1 分钟）
bash scripts/e2e_verify.sh 50000 /tmp/rating-e2e

# 手动分步
go build -o engine ./cmd/engine
python3 -I scripts/gen_config.py --out ./data/demo --users 300 --plans 16
python3 -I scripts/gen_cdrs.py  --data ./data/demo --count 100000 --shards 16
./engine run       --data ./data/demo --shards 16 --parallel 8 --name t1
python3 -I scripts/oracle.py   --data ./data/demo
./engine reconcile --data ./data/demo --task t1     # 要求 对平=true
./engine show      --data ./data/demo --task t1     # 阶段耗时
./engine exceptions--data ./data/demo --task t1
./engine recompute --data ./data/demo --old t1 \
        --start 2026-11-01T00:00:00Z --end 2026-12-01T00:00:00Z
./engine serve     --data ./data/demo --addr :8080  # 运营页面
```

## 运营页面（五个功能区）

总览（KPI/异常分布）、话单接入（inbox 文件、新建任务）、资费配置（版本/阶梯/夜间/漫游、订阅）、
批价任务（阶段耗时条形图、64 分片状态网格、对平、oracle 对账）、重算对比（窗口重算、用户差额）、
异常话单（E1~E5 筛选、原文摘要）。静态资源 `go:embed` 打进单二进制，离线可用。

## 确定性与对账设计

- 金额全程整数**厘**（1 元=100000 厘），比例运算用整数乘除 `//`，禁止浮点入金额。
- Go 引擎与 Python oracle 是两套独立实现，唯一共同依据是 `docs/SPEC.md`。
- 三层对账：① rated 明细求和 == 用户汇总；② 用户汇总 == 账期全局汇总；
  ③ 引擎用户汇总 == oracle 用户汇总（逐行金额，差异列 `kind/diff`）。
- DER 流遇首条坏记录即停（错位后无法再同步），生成器把乱码排到 asn1 文件末尾，
  保证两端消费集合一致。

## 异常话单分类

| 代码 | 含义 |
|------|------|
| E1_BAD_FORMAT | 长度/字段数/DER/UTF-8/NUL 等格式错误、乱码 |
| E2_BAD_TIME | end ≤ start 或时间不可解析 |
| E3_BAD_QUANTITY | quantity ≤ 0 |
| E4_NO_SUBSCRIPTION | 起始时刻无匹配订阅 |
| E5_BAD_SEGMENT | 拆段段号集合与 seg_total 不一致（缺段/重复段） |

## 千万级实测（14 核 darwin/arm64，64 分片并行 12）

输入 1.0 GB、1010 万条记录：

| 阶段 | 耗时 | 说明 |
|------|------|------|
| ingest 接入解码 | 18.4 s | 四格式解码、乱码拦截、分片落盘 |
| rate_shards 批价 | 10.8 s | 去重 15 万、合并 30 万组、941 万有效话单 |
| aggregate 汇总对平 | 13.8 s | 10 万用户×月行 |
| **全流程** | **43.5 s** | oracle 对账 100000/100000 行、总额分毫不差 |

异常计数引擎与 oracle 逐项一致：E1=39883、E2=20000、E3=20000、E4=10000、E5=40012；
去重 150000；总额 1,028,476,535,427 厘（约 1028.5 万元）两端相等。

## 测试

```bash
go test ./...          # 编解码往返 + 批价规则（额度/阶梯/夜间/漫游/跨月/取整）
bash scripts/e2e_verify.sh
```

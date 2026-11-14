# 批价引擎确定性规则说明书（SPEC）

本文件是 Go 批价引擎与 Python 独立应收（oracle）共同遵守的唯一计费规则来源。
两套实现必须逐分对齐（金额单位为整数"厘"，1 元 = 100000 厘；价格配置以"厘/单位"存储）。

## 1. 业务对象

### 1.1 业务类型
- `voice` 语音：计量单位"秒"，批价单位"分钟"，**向上取整**
- `sms`   短信：计量单位"条"，批价单位"条"（1 条 CDR = 1 单位）
- `data`  流量：计量单位"字节"，批价单位"MB"（1 MB = 1048576 字节），**向上取整**

### 1.2 CDR 原始字段
| 字段 | 含义 |
|---|---|
| record_id | 全局唯一，格式 `REC<10位序号>-<6hex>`（恰好 20 字符） |
| record_type | fixed / delimited / asn1 |
| user_id | 用户号，格式 `166` + 8 位数字 |
| service | voice / sms / data |
| start_time / end_time | RFC3339 UTC（ASN.1 用 GeneralizedTime UTC） |
| quantity | voice=秒，sms=条，data=字节 |
| roaming | 是否漫游 0/1 |
| call_ref | 同一次长通话各拆段共用的引用号；无拆段为空 |
| seg_no / seg_total | 拆段序号/总段数（从 1 开始）；非拆段为 0/0 |

### 1.3 资费计划 Tariff
```json
{
  "plan_id": "P001",
  "version": "v1",
  "effective_start": "2026-10-01T00:00:00Z",
  "effective_end":   "2026-11-01T00:00:00Z",
  "services": {
    "voice": {
      "unit_size": 60, "rounding": "up",
      "allowance_units": 300,
      "tiers": [{"upto": 100, "rate": 15000}, {"upto": -1, "rate": 10000}],
      "night_window": ["23:00", "07:00"], "night_factor": 5000,
      "roaming_factor": 20000
    }
  }
}
```
- `allowance_units`：账期内免费额度（按批价单位计），0 表示无免费额度
- `tiers`：阶梯价，按"批价单位"累计量分档；`upto=-1` 为无穷档
- `night_factor`：夜间费率系数，基点制（10000 = 原价）；为空或 10000 表示不优惠
- `roaming_factor`：漫游费率系数，基点制，与夜间系数**相乘**后取整
- 短信、流量结构相同（短信无夜间窗口、无漫游时对应字段留空/10000）

### 1.4 用户订阅 Subscription
一个用户每个业务同一时刻只有一条生效订阅；月中换套餐 = 同一业务前后两条订阅：
```
{user_id, service, plan_id, effective_start, effective_end}
```
生成器保证同一用户同一业务的订阅区间连续、不重叠、无缺口。

## 2. 解析与校验

### 2.1 三种文本/二进制格式
1. **定长文本（.fixed）**：UTF-8，每行 120 字符（不含换行），字段偏移：
   `record_id[0:20] user_id[20:32] service[32:38] start[38:58] end[58:78]
    quantity[78:92] roaming[92:94] call_ref[94:110] seg_no[110:114] seg_total[114:118] type[118:120]`
   时间为 20 字符 RFC3339（`2026-10-05T23:10:00Z`），数字字段右对齐补零。
   末列 type 为 2 位数字格式码：`00`=fixed、`01`=delimited、`02`=asn1、`03`=lpbin。
2. **分隔符文本（.delim）**：`|` 分隔，字段顺序
   `record_id,user_id,service,start,end,quantity,roaming,call_ref,seg_no,seg_total,type`
   其中 type 同样为数字格式码（字符串形式）。
3. **ASN.1 DER（.asn1）**：SEQUENCE，定义见 codec/asn1.go；UTC GeneralizedTime 秒精度。
4. **长度前缀二进制（.lpbin，附加格式）**：每条记录 = 4 字节大端长度 + 定长文本行（不含换行）。

### 2.2 校验失败即异常话单（不进入批价）
- E1_BAD_FORMAT：长度/分隔字段数/ASN.1 解码错误/乱码行（含 `\x00` 或 UTF-8 非法序列）
- E2_BAD_TIME：end < start，或时间无法解析
- E3_BAD_QUANTITY：quantity <= 0
- E4_NO_SUBSCRIPTION：找不到与（用户，业务，起始时刻）匹配的订阅
- E5_BAD_SEGMENT：同一 call_ref 的段集合与 seg_total 不一致（缺段/重复段号）
异常话单落 `exceptions/` 分片文件，带 reason 与原文摘要（前 200 字节，不可打印转义）。

### 2.3 去重
- 以 `record_id` 为键全局去重；同键重复出现时保留第一条，其余计入 dup_count。
- 去重在分片内完成（同一 user_id 的话单恒定路由到同一分片：`fnv32a(user_id) mod N`）。

### 2.4 拆段合并
- 同一 `call_ref` 的多段（且 E5 校验通过）合并为一条 CDR：
  start_time = 各段最小 start；end_time = 各段最大 end；
  quantity = 各段 quantity 之和；roaming 取第一段；segments = seg_total。
- 合并后 record_id 取首段 record_id，合并映射写入合并明细（原段 id 列表）。

## 3. 时间切分

### 3.1 切分边界集合 B
对一条 CDR [s, e)，收集以下时间点落在 (s, e) 内部的：
1. 账期边界（月界，UTC 00:00）
2. 该用户该业务的订阅变更时刻
3. 订阅所引用资费版本的 effective_start/end
B 排序后与 s、e 组成片段，每个片段落在唯一的（账期，订阅，资费版本）上。
无匹配订阅的片段整条 CDR 记 E4（生成器保证不会出现片段级缺口，只有整体无订阅的坏数据）。

### 3.2 用量在片段间分配
- voice：按片段**秒数**占比分摊总秒数（整数秒，最后一段兜底余数）；零长度片段不产生
- data：按片段**秒数**占比分摊总字节（最后一段兜底余数）
- sms：整条计入 start_time 所在片段（其余片段不产生）

### 3.3 免费额度池
每个（用户 × 业务 × 账期月 × 订阅 × 资费版本）交集为一个额度池。
先按订阅与账期月的交集秒数占订阅总秒数比例分摊 plan.allowance_units：
`allowance_m = floor(total * sec_m / sec_total)`，最后一个月份池兜底余数，
保证同一订阅各月池 allowance 之和恰为 total；资费版本边界再在月内按秒数占比二次切分，
同样由最后一个池兜底。订阅不跨月、资费不换版时比值为 1，即全额。

## 4. 批价（单位：厘）

对每个片段：
1. 原始用量 → 批价单位数
   - voice：`ceil(seconds / unit_size)`
   - data：`ceil(bytes / unit_size)`
   - sms：数量即单位数
2. 该片段先吃所属额度池的免费额度：
   `free = min(units, pool.remaining)`；`billable = units - free`（同池 CDR 按 start_time 升序、record_id 升序处理，确定性扣减）
3. 阶梯价：维护"该资费版本内已计费单位数"游标（同 user×service×tariffVersion 跨池累计），
   对 billable 数量逐档落入 tiers（upto=-1 为无穷档），得到 base_fee。
4. 系数：
   - 夜间判定按片段中点 `m = start + (end-start)/2`（整数秒向下取整）的 UTC 时分落在
     night_window 区间内（窗口可跨 0 点，如 23:00–07:00）；命中乘 night_factor
   - roaming=1 乘 roaming_factor
   - `fee = round_half_up(base_fee * night_factor/10000 * roaming_factor/10000)`
     （先乘夜间再乘漫游，每步四舍五入到厘）
5. 一条 CDR 的 fee = 各片段 fee 之和。

## 5. 汇总与对账

### 5.1 账务汇总
- 用户级：每（user_id, 账期月）输出 valid_count、各业务 units、total_fee
- 全局：按账期月汇总 total_fee；另输出 dup_count、exception_count（按 reason）
- 对账三等式：
  1. 引擎 rated 明细 fee 求和 == 用户汇总 total_fee
  2. 用户汇总求和 == 全局账期汇总
  3. 引擎用户汇总 == oracle 用户汇总（逐用户、逐业务、逐金额；任何差异输出明细）

### 5.2 重算
- 资费新版本生效后，可指定 [recompute_start, recompute_end) 时间段重算。
- 重算结果与原批价按 user_id × 月份输出 delta（new_fee - old_fee，含业务级拆分）。
- 重算是独立任务目录，不覆盖原结果。

## 6. 分片与任务
- 分片键：fnv32a(user_id) mod shard_count（默认 64），同一用户全部话单、订阅、汇总在同一分片。
- 分片状态机：PENDING → RUNNING → DONE / FAILED；checkpoint 原子写（tmp + rename）。
- 任务中断后重启：DONE 跳过，其余分片从头重跑（分片输出整文件覆盖，幂等）。
- 每阶段记录开始/结束时间戳与输入输出条数，供页面耗时可视化。

## 7. 哈希与确定性
- fnv32a 为标准 FNV-1a 32 位。
- 所有时间 UTC；所有除法与取整规则如上，禁止浮点参与金额计算（占比分摊可用整数乘除）。
- Go 与 Python 输出的 JSON 字段名、枚举值、金额单位完全一致。

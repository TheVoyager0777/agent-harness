# 未决问题(按怀疑度排序)

## 1. f16 为何只有真值 60%
f16 = base64url 解出的 opaque blob,是 BotGuard VM 产出的环境证明。真实会话 3719→4446B 随时间增长;我们恒定 ~2500B。已排除: worker 闲置(ch5 port 双向通信在跑)、合成事件计数(加行为流不长)、cookie(带真 _GRECAPTCHA 不长)、会话龄(45s×6 execute 不长)。**假设: BotGuard 的某些 collector 在 jsdom 里静默失败/产出空**——需要找引擎里 collector 的注册点,单测每个 collector 在我们的 env 里产出什么。

## 2. 判分主体到底是不是 f16
siteverify 判分的输入可能是 f16 的 verdict 字段而非大小。备选: 也许 403 根本不是因为 f16 而是 token 内的其他 score 分量。判别实验缺位——**需要一个能读 score 的 oracle**。思路: 找个有 siteverify 后台可见分数的 sitekey,或者 Google Admin Console 看分数;或者对比 minted token 在我们能控制的低阈值端点上的逐步失败曲线,反推各分量权重。

## 3. anchor iframe 的 __PAYLOAD init 参数
anchor HTML 尾部的 `recaptcha.anchor.Main.init(...)` payload 含服务端下发的完整配置(session token、超时、策略)。我们的引擎在 vm 里被 `window.__capturedInit` 钩子接走 payload 后手动 init——**引擎对 payload 的哪些字段敏感没查过**(可能有 fingerprint 期望值/会话绑定)。

## 4. jsdom 残余泄漏面
已修: Symbol(impl)/webidl 符号枚举、_globalProxy、worker location、indexedDB/caches stub、50+ API 补齐、nativeizer。未审计: `Error.stack` 深度/格式、`Function.prototype.toString` 覆盖盲区、getter/setter 描述符差异、`document.all` 怪癖、GC/heap 探针(performance.memory 我们给了吗?)、`navigator.connection/deviceMemory` 一致性。

## 5. clr 发送时机
我们每次 execute 都发 clr,真页面 0/127。引擎为什么在我们环境里选择发 clr? 可能 clr 的发送条件本身就是检测信号(比如它在 env 异常时才发)。查引擎里 clr 的触发条件。

## 测试通路
- mint: `cd minter && V3_TLS=1 V3_DUMPBODY=1 V3_EXECUTE_N=2 ... node live-v3-single.mjs` → dump/v3-token.txt + dump/reload-body.bin
- create 判定: POST `127.0.0.1:9090/v1/debug/create-direct` `{raw_payload: {battle payload}, token}` → status 200=过,403=recaptcha 挂,401=缺 auth(=recaptcha 已过!)
- 真值: `arena2api-ref/dump/recap-real/`(扩展从真浏览器 google.com/recaptcha iframe 抓的 reload req/resp)

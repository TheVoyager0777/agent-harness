# 目标

让 jsdom 驱动的 reCAPTCHA Enterprise mint simulator(`D:\zcode-workspace\arena-gateway\minter\live-v3-single.mjs`)铸出能通过 arena.ai `create-evaluation` 的 v3 token。判定标准: POST `/nextjs-api/stream/create-evaluation` 返回 200 而非 `403 recaptcha validation failed`。

核心原则(用户定的): **让 simulator 能完整运行人机验证组件本身**,而不是针对某个打分阈值调参。这样 Google 换遥测算法/字段时我们不依赖对具体数据源的逆向——引擎自己跑完收集什么就发什么。

# 已验证事实

- minted token 能过 arena `sign_up` 和 message-continue 的 recaptcha 校验(低阈值),过不了 create(高阈值)
- 浏览器 token + 服务端 curl_cffi create = 200(基准确认,差距纯在 token)
- reload 请求所有可观测字段已与真浏览器逐字节一致
- 差距定位在 reload req 的 **f16**(BotGuard 加密证明块): 真实 ~4200B,我们的 ~2500B,内容不透明

# 架构

引擎 = google 的 `recaptcha__en.js`(850KB),在三层跑: 父页(arena.ai)→ anchor iframe(google.com)→ BotGuard worker。我们用 jsdom + vm.createContext 模拟三层,fetch/XHR/MessageChannel 全部自实现接管。

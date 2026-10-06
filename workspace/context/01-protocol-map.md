# reload 请求协议地图 (POST /recaptcha/enterprise/reload, application/x-protobuf)

真浏览器(127 次抓包) vs 我们的 mint,top-level 字段已完全一致:

| field | 内容 | 真值 | 我们 |
|---|---|---|---|
| f1  | 版本 str | guXhH0v-XMxlzmbTwqkaT4i5 | ✅同 |
| f2  | blob ~2KB | | ✅ |
| f5  | 会话 id str | | ✅ |
| f6  | blob 1B | | ✅ |
| f7  | token | 上一次 rresp[8] 回传(05APDCMQ...) | ✅ EXEC_N≥2 后出现 |
| f8  | action str | chat_submit | ✅同 |
| f14 | sitekey | 6LeTGMcsAAAAALuIlkVwIxaAuZA8VledA6d3Nnb0 | ✅同 |
| f16 | BotGuard 证明 blob(base64url→opaque) | **decoded ~4200B,随会话龄增长** | **~2500B,不长** ←唯一大缺口 |
| f20 | 编码 blob(疑似行为遥测) | ~370B | ~220B |
| f21 | token | 上一次 rresp[14] 回传(0aAPDCMQ...) | ✅ EXEC_N≥2 后出现 |
| f22 | blob 3748B | 两侧相同大小 | ✅ |
| f25 | b64 [[ms,count]] | 会话驻留计时对 | ✅ |
| f28/f29 | varint 20000/30000 | anchor-ms/execute-ms | ✅同 |

# 响应结构 rresp (17 字段)
`["rresp", <v3 token 2360B>, null, 120, null×4, <246B token>, null×3, <130B>, null, <73B>, <76B>, 0]`
- resp[8] → 下个 reload 的 f7
- resp[14] → 下个 reload 的 f21
- resp[1] → 发给 siteverify 的 token(0cAFcWeA 前缀)

# 其他端点
- `clr` POST ~2KB: 客户端日志上报(sitekey+token+计时三元组),真浏览器抓包窗口内 0 次,我们每次 execute 发一次——引擎分支差异,但 clr 在 token 签发后,不影响当次分
- `anchor GET`: 返回内嵌 `0dAFcWeA` 会话 token + `recaptcha-token` 隐藏 input + `__PAYLOAD` init 参数 + set-cookie(SIDCC/1PSIDCC/3PSIDCC),**必须每次真 GET**(过期 anchor = reload 缺字段)
- `webworker.js` GET: 仅 102B `importScripts(recaptcha__en.js)` 包装

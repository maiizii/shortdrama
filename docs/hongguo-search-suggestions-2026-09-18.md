# 红果搜索联想接口与项目实现

验证日期：2026-09-18。范围仅红果，目标是输入时返回候选词的搜索联想。

同日追加了[正式搜索补全修复](hongguo-search-completeness-2026-09-18.md)。下文的“最多 10 条”仅指输入联想；正式搜索另取名称检索的剧集元数据并与官网搜索结果合并，不沿用此显示上限。下方验证表为果果剧库本轮同步验证，最新整体验证见[双项目同步记录](dual-project-sync-2026-09-18.md)。

## 已验证接口

红果官网公开前端使用同域 GET 接口：

```text
https://hongguoduanju.com/incent_resource/suggestion
```

| 参数 | 用途 |
| --- | --- |
| `app_id=8662` | 红果应用标识，官网 HTTP 客户端默认携带 |
| `query` | 输入文本，按 URL 查询参数编码 |
| `count` | 请求候选数量，官网默认 10 |
| `web_id` | 官网传入访客标识；本次省略也成功 |

可复现请求：

```bash
curl -q --get 'https://hongguoduanju.com/incent_resource/suggestion' \
  --data-urlencode 'app_id=8662' \
  --data-urlencode 'query=永' \
  --data-urlencode 'count=10'
```

原始 JSON 的顶层字段为 `suggest_list`，没有额外的 `data` 包装。各项包含 `name`、`keyword`、`word_type`、`display_words`，部分附带 `video_data`。

`name` 是展示及点击后搜索的文本；`keyword` 可能是剧集 ID、类别 ID 或词条标识，不应一律当搜索文字。剧集 ID 长于 JavaScript 安全整数范围，后端应保持字符串或使用 `json.Number`，避免精度损失。

## 实测结果

- `query=永&count=10`：HTTP 200，10 条候选，包括《永世长青》《永夜苟神：十万住户供我发育》《永冬之下，冻土之上》等，与截图有多个相同标题。
- `query=重生&count=10`：HTTP 200，10 条候选。
- 另以 `curl -q` 禁用默认 curl 配置进行匿名请求，HTTP 200；未提供 Cookie、API Key、App 签名或 `web_id`。
- 省略 `app_id` 的两个请求均返回 HTTP 500；接入时必须按已验证形式携带它。
- 实测类型含 `short_play_name`、`common_query`、`short_play_category`；官网渲染代码还支持演员类型。
- 部分有效候选的 `video_data` 为空，不能因此直接丢弃候选词。

这是官网正在使用的内部接口。本次验证说明当前可用，未验证所有网络环境、长期稳定性或与 App 完全相同的排序。

## 本项目实现

剧库选择“红果”或“全部站源”且当前账号有红果权限时，搜索框输入会显示红果候选词。推荐模式不启用联想；此版本的来源入口仅包含红果。点击、触摸候选，或用上下方向键选择后回车，按候选的 `name` 执行已有联网搜索；直接回车和搜索按钮仍可使用。Esc、Tab、失焦、清空及切换站源会关闭下拉。最近搜索仅在空输入时展示。

服务端入口：

```text
GET /api/ui/search/suggestions?q=永
```

成功响应只包含查询文本、来源和候选名称 / 类型，不转发剧集 ID 或 `video_data`：

```json
{"query":"永","source":"hongguo","data":[{"name":"永世长青","type":"short_play_name"}]}
```

请求固定访问红果官网，沿用现有代理、来源限流和退避；整体超时 5 秒，不为每次输入追加网络重试。响应限制 256 KiB，候选去空白、去重后最多 10 条，保留没有 `video_data` 的有效候选。缓存有效期 2 分钟、最多 128 个查询，同时合并同词请求并限制最多 32 个待处理查询。一个浏览器取消请求后，仍有需求的调用可在自身期限内重新获取。接口检查当前账号的红果权限，联想本身不加载详情或修改剧库。

前端采用 300 毫秒防抖、取消旧请求、序号检查、6 秒超时，以及 60 秒 / 32 项缓存。中文输入法选字期间不请求联想、不触发联网搜索；选字完成后再调度。候选文字通过 DOM 文本节点展示，高亮保留 Unicode 字符；下拉使用 combobox / listbox 语义，手机限制可见高度并允许触摸滚动。联想为空、超时或接口失败时，普通搜索仍可使用。

实现参考官网自身的 `28505` 模块（300 毫秒防抖、请求序号检查）及 `16827` 模块（联想列表、类型展示、字符高亮），没有引入新前端依赖。

## 验证

本机为 macOS 14.4 Intel、Go 1.24.1。自动化浏览器使用隔离剧库、账号和模拟上游，不操作实际运行数据。

| 检查 | 结果 |
| --- | --- |
| Go 全量测试 | `go test ./...` 通过 |
| 联想与搜索竞态检查 | `go test -race ./internal/app -run 'Test(HongguoSuggestion\|HongguoSearch\|LibrarySuggestions\|LibrarySearch)' -count=1` 通过 |
| 前端测试 | `node --test internal/webui/*.test.cjs`，45 项通过 |
| 红果真实接口 | 可选在线测试通过，当前返回 10 条候选 |
| Chrome 153 / WebKit 26 | 两个引擎各 10 组交互检查通过，覆盖防抖、候选搜索、缓存、旧响应、中文输入法、HTML 字面文本、异常回退、来源切换和手机触摸 |
| 外观 | Chrome 桌面及 390 px 手机截图已检查；两个引擎的手机下拉均未越界，滚动后可选最后一项 |
| 成品构建 | macOS Intel / Apple Silicon、Linux amd64、Windows amd64 均以 Go 1.24.1、`CGO_ENABLED=0` 编译；两份 macOS 程序的最低系统标记为 11.0 |
| 成品启动 | macOS Intel 程序使用隔离数据启动成功，页面及全部 41 个内嵌 JS / CSS / 文本资源与源码一致，缓存校验头正确；联想路由可校验空输入 |
| 交付包 | `dist/` 四个平台程序及对应 ZIP 已更新；ZIP 均仅含同名程序，CRC 通过，解压成员与编译产物 SHA-256 一致 |

WebKit 自动化模拟不等同于 macOS 12.6 及其旧版 Safari 的实机验证。官网接口可能调整，暂不可用时保留原有搜索操作。

真实接口测试不在默认测试中运行：

```bash
JUKU_LIVE_SUGGESTIONS=1 go test ./internal/app -run '^TestLiveHongguoSearchSuggestions$' -count=1 -v
```

## 公开证据

- [红果官网搜索页](https://hongguoduanju.com/search/)
- [官网前端 337.6d99ca67.js](https://lf-fe.fqnovelstatic.com/obj/novel-fanqie-fe/growth/incentive-h5-monorepo/apps/hongguo/static/js/async/337.6d99ca67.js)：`10986` 模块定义 `/incent_resource/suggestion`，HTTP 客户端默认参数为 `app_id=8662`，调用参数为 `web_id`、`query`、`count`。
- [公开红果 App 逆向项目的普通搜索实现](https://github.com/zhangbaio/hongguo/blob/5f8a58d10f48954bc9b6a58c9cd6418bf499feb9/hongguo.py#L327)：使用 `/reading/bookapi/search/tab/v`，不能与本次找到的输入联想接口混为一谈。

公开搜索及抽查的仓库中未找到专门封装这一联想接口的项目；可用调用来自红果官网的公开 JavaScript，并已直接请求验证。

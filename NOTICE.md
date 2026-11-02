# 代码来源与许可

Teamcp 以 MIT 许可发布（见 `LICENSE`，待定稿）。

## 改编自 Termcp

`pkg/envelope` 改编自 [open-mcp-ai/termcp](https://github.com/open-mcp-ai/termcp) 的
`internal/envelope`（MIT 许可）。它是同一套「HTTP 形状信封」的渲染器，只在两处偏离原实现：

1. **计数单位是消息**：`Content-Range: messages 1-20/57`，而不是字节区间。
2. **截断与区间分离**：新增 `CutBody`，按字节截断正文并写入 `Truncated` 头，**不动**按条计数的
   `Content-Range`。原实现的 `Cut` 把两者绑在一起，在消息方言下会把窗口谎报成一个调用方并未拿到的区间。
3. **slot 分隔被做实**：`Reply.Text` 保证两个 slot 之间恰好一个空行。原实现用
   `strings.Join(parts, "\n")`，空行只在 body 恰好以换行结尾时出现——正文必须逐字节保留，
   所以补的是**分隔符**缺的那个换行，而不是正文。

其余设计（状态码词表、标头渲染顺序、文本/结构化双投影、`end+1` 即下一个游标）原样保留，
因为它们是这套信封值得复用的全部理由。

上游若更新该包，需要人工比对后再同步——**本仓库不依赖 termcp 模块**，两份代码各自演进。

# 第三方声明

## ZCode 3.17.2 system 提示词（`zcode317_prompt.json`）

该文件逐字节取自 [gakiyukr/zcode2api-plus](https://github.com/gakiyukr/zcode2api-plus)
的 `internal/upstream/zcode_system.json`。该仓库以 MIT License 发布，文件内注释说明
文本来自 ZCode 3.17.2 官方客户端（`Copyright (c) 2026 yetone`，其自身又注明移植自
magpie 0.1.52 的 zcode 插件）。

我们只把它用于 `ZCODE_SYSTEM_SHAPE=317` 灰度形态：上游按 byte-for-byte 校验前两块
system 文本，任何改写都会让请求吃 405/3012。

```
MIT License

Copyright (c) 2026 yetone

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```
